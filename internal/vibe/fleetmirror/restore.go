package fleetmirror

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Verify reads an archive back and checks every claim the manifest makes
// about it. A mirror nobody has ever read back is a belief; this is the
// cheapest thing that turns it into a fact, and `restore` runs it before
// it writes anything so a corrupt archive fails BEFORE it has half
// replaced a working state dir.
func Verify(archive string) (*Manifest, error) {
	f, err := os.Open(archive) //nolint:gosec // an operator-named archive path
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", archive, err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var m *Manifest
	seen := map[string]string{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return m, fmt.Errorf("%s: %w", archive, err)
		}
		if h.Typeflag != tar.TypeReg {
			return m, fmt.Errorf("%s: %s is not a regular file", archive, h.Name)
		}
		if err := safeName(h.Name); err != nil {
			return m, fmt.Errorf("%s: %w", archive, err)
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxFileBytes+1))
		if err != nil {
			return m, err
		}
		if int64(len(data)) > maxFileBytes {
			return m, fmt.Errorf("%s: %s exceeds the per-file limit", archive, h.Name)
		}
		if h.Name == ManifestName {
			var mm Manifest
			if err := json.Unmarshal(data, &mm); err != nil {
				return nil, fmt.Errorf("%s: manifest: %w", archive, err)
			}
			if mm.Version != ManifestVersion {
				return &mm, fmt.Errorf("%s: manifest version %d, this build understands %d", archive, mm.Version, ManifestVersion)
			}
			m = &mm
			continue
		}
		sum := sha256.Sum256(data)
		seen[h.Name] = hex.EncodeToString(sum[:])
	}
	if m == nil {
		return nil, fmt.Errorf("%s: no %s — not a fleet mirror", archive, ManifestName)
	}

	var problems []error
	for _, e := range m.Entries {
		got, ok := seen[e.Archive]
		if !ok {
			problems = append(problems, errors.New(e.Archive+": in the manifest, missing from the archive"))
			continue
		}
		if got != e.SHA256 {
			problems = append(problems, errors.New(e.Archive+": sha256 mismatch (manifest "+short(e.SHA256)+", archive "+short(got)+")"))
		}
		delete(seen, e.Archive)
	}
	for name := range seen {
		problems = append(problems, errors.New(name+": in the archive, absent from the manifest"))
	}
	return m, errors.Join(problems...)
}

func short(sum string) string {
	if len(sum) > 12 {
		return sum[:12]
	}
	return sum
}

// safeName rejects anything that would let an archive write outside the
// destination it was handed. A backup tool that unpacks "../.." is an
// arbitrary-write primitive wearing a helpful name.
func safeName(name string) error {
	if name == "" {
		return errors.New("empty entry name")
	}
	if path.IsAbs(name) || filepath.IsAbs(name) || strings.HasPrefix(name, "/") {
		return errors.New(name + ": absolute path in archive")
	}
	if filepath.VolumeName(name) != "" {
		return errors.New(name + ": volume-qualified path in archive")
	}
	clean := path.Clean(name)
	if clean != name {
		return errors.New(name + ": unclean path in archive")
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return errors.New(name + ": escapes the archive root")
		}
	}
	return nil
}

// RestoreOptions places an archive's slots on the standby box. Every
// destination is explicit and an unset one is a SKIP with a note, never
// a guess: a restore that quietly writes a state dir the operator did
// not name is the worst possible surprise at the worst possible time.
type RestoreOptions struct {
	Archive     string
	StateDir    string
	ConfigDir   string
	FrontConfig string
	FrontExtras string
	// Overwrite permits replacing files that already exist on the
	// standby. Off by default: an accidental restore over a LIVE fleetd
	// is the second-worst outcome this command can produce.
	Overwrite bool
	DryRun    bool
	// Force skips the takeover probe. It exists because the probe has one
	// honest false positive — see TakeoverProbe.
	Force bool
	// Dial is injectable so the probe is testable without a listener.
	Dial        func(network, addr string, timeout time.Duration) (net.Conn, error)
	ProbeAddrs  []string // overrides the manifest's recorded identity (tests, drills)
	DialTimeout time.Duration
}

// Action is one planned or performed file placement.
type Action struct {
	Archive string      `json:"archive"`
	Dest    string      `json:"dest,omitempty"`
	Slot    Slot        `json:"slot"`
	Mode    fs.FileMode `json:"mode"`
	// Status: written | would-write | exists | no-destination | manual
	Status string `json:"status"`
	Note   string `json:"note,omitempty"`
}

// TakeoverHit is one address that answered while a standby was being
// stood up — the fleet's own address, still serving from somewhere.
type TakeoverHit struct {
	What string `json:"what"`
	Addr string `json:"addr"`
}

// RestoreReport is what the command prints. It is also what the drill
// asserts on.
type RestoreReport struct {
	Manifest *Manifest     `json:"manifest"`
	Actions  []Action      `json:"actions"`
	Takeover []TakeoverHit `json:"takeover,omitempty"`
	Wrote    int           `json:"wrote"`
	DryRun   bool          `json:"dry_run"`
}

// ErrTakeover is returned when something still answers on the fleet's
// own addresses.
var ErrTakeover = errors.New("the fleet's address still answers")

// TakeoverProbe dials the addresses the manifest recorded as the
// fleet's identity and reports which of them answer.
//
// This is the whole of C19's answer to "what if two boxes answer at
// once", and it is deliberately a REFUSAL rather than an election.
// Two fronts under one name split every client between two catalogs;
// two fleetds both accept announces, both queue piggyback verbs, both
// run warm and sleep schedules, and both fold the same cumulative
// announce totals into two append-only ledgers that can never afterwards
// be reconciled. Nothing detects that state, because each half looks
// healthy from where it stands.
//
// The one false positive is honest and documented: if the operator has
// ALREADY moved the address to this box and started something on it, the
// probe reaches itself and refuses. That is why the runbook's order is
// restore first, address second, and why --force exists.
func TakeoverProbe(opts RestoreOptions, id Identity) []TakeoverHit {
	timeout := opts.DialTimeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	dial := opts.Dial
	if dial == nil {
		dial = func(network, addr string, d time.Duration) (net.Conn, error) {
			return net.DialTimeout(network, addr, d)
		}
	}
	type target struct{ what, raw string }
	targets := []target{{"fleetd", id.FleetdURL}, {"the front", id.FrontURL}}
	if len(opts.ProbeAddrs) > 0 {
		targets = nil
		for _, a := range opts.ProbeAddrs {
			targets = append(targets, target{"declared address", a})
		}
	}
	var hits []TakeoverHit
	for _, t := range targets {
		addr := dialAddr(t.raw)
		if addr == "" {
			continue
		}
		conn, err := dial("tcp", addr, timeout)
		if err != nil {
			continue
		}
		conn.Close()
		hits = append(hits, TakeoverHit{What: t.what, Addr: addr})
	}
	return hits
}

// dialAddr turns a recorded URL (or a bare host:port) into something
// net.Dial accepts.
func dialAddr(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		if _, _, err := net.SplitHostPort(raw); err == nil {
			return raw
		}
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	if u.Port() != "" {
		return net.JoinHostPort(u.Hostname(), u.Port())
	}
	if u.Scheme == "https" {
		return net.JoinHostPort(u.Hostname(), "443")
	}
	return net.JoinHostPort(u.Hostname(), "80")
}

// Restore verifies an archive and places its slots. It writes nothing
// until every check has passed.
func Restore(opts RestoreOptions) (*RestoreReport, error) {
	m, err := Verify(opts.Archive)
	if err != nil {
		return nil, fmt.Errorf("refusing to restore: %w", err)
	}
	rep := &RestoreReport{Manifest: m, DryRun: opts.DryRun}

	if !opts.Force {
		if hits := TakeoverProbe(opts, m.Identity); len(hits) > 0 {
			rep.Takeover = hits
			var names []string
			for _, h := range hits {
				names = append(names, h.What+" ("+h.Addr+")")
			}
			return rep, fmt.Errorf("%w: %s. Confirm the old front is DOWN before standing up a second one — "+
				"two fleetds both accept announces, both fire warm and sleep schedules, and both fold the same "+
				"cumulative totals into two ledgers that cannot afterwards be reconciled. "+
				"If this is the standby answering on an address you already moved, re-run with --force",
				ErrTakeover, strings.Join(names, ", "))
		}
	}

	dest := func(e Entry) (string, string) {
		switch e.Slot {
		case SlotState:
			if opts.StateDir == "" {
				return "", "no --state-dir given"
			}
			return filepath.Join(opts.StateDir, filepath.FromSlash(e.Rel)), ""
		case SlotConfig:
			if opts.ConfigDir == "" {
				return "", "no --config-dir given"
			}
			return filepath.Join(opts.ConfigDir, filepath.FromSlash(e.Rel)), ""
		case SlotFront:
			if e.Rel == "config.yaml" {
				if opts.FrontConfig == "" {
					return "", "no --front-config given: render one with `vibe router render --cell front` instead"
				}
				return opts.FrontConfig, ""
			}
			if opts.FrontExtras == "" {
				return "", "no --front-extras given"
			}
			return opts.FrontExtras, ""
		default:
			return "", "extras are restored by hand: they came from absolute paths on the old host"
		}
	}

	// Plan first, write second. Every refusal an operator could hit is
	// decided before the first byte lands, so a restore either happens or
	// does not — it never stops halfway with a state dir holding one
	// fleet's token and another fleet's intent.
	type job struct {
		e    Entry
		dest string
	}
	var jobs []job
	for _, e := range m.Entries {
		if err := safeName(e.Archive); err != nil {
			return rep, err
		}
		d, why := dest(e)
		if d == "" {
			status := "no-destination"
			if e.Slot == SlotExtra {
				status = "manual"
			}
			rep.Actions = append(rep.Actions, Action{Archive: e.Archive, Slot: e.Slot, Mode: e.Mode, Status: status, Note: why})
			continue
		}
		if _, err := os.Stat(d); err == nil && !opts.Overwrite {
			rep.Actions = append(rep.Actions, Action{Archive: e.Archive, Dest: d, Slot: e.Slot, Mode: e.Mode,
				Status: "exists", Note: "already present; --overwrite to replace"})
			continue
		}
		status := "written"
		if opts.DryRun {
			status = "would-write"
		}
		rep.Actions = append(rep.Actions, Action{Archive: e.Archive, Dest: d, Slot: e.Slot, Mode: e.Mode, Status: status})
		jobs = append(jobs, job{e: e, dest: d})
	}
	if opts.DryRun {
		return rep, nil
	}

	payload, err := readArchiveFn(opts.Archive)
	if err != nil {
		return rep, err
	}
	for _, j := range jobs {
		data, ok := payload[j.e.Archive]
		if !ok {
			return rep, errors.New(j.e.Archive + ": vanished between verify and restore")
		}
		// Verify read the archive; this read it again. Between the two the
		// file could have been replaced — by another mirror run finishing,
		// by a half-copied file on a network mount, by anything — and then
		// what lands on the standby is not what was checked. Cheap to
		// close, and "verified" is a claim this command makes to somebody
		// mid-incident.
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); got != j.e.SHA256 {
			return rep, fmt.Errorf("%s: content changed after verification (manifest %s, now %s) — nothing further written",
				j.e.Archive, short(j.e.SHA256), short(got))
		}
		if err := os.MkdirAll(filepath.Dir(j.dest), 0o755); err != nil {
			return rep, err
		}
		mode := j.e.Mode.Perm()
		if mode == 0 {
			mode = 0o600
		}
		// The mode is RESTORED, not inherited: the control-plane token is
		// 0600 on the front host and a standby that widens it has quietly
		// published the fleet's root credential.
		if err := writeFileMode(j.dest, data, mode); err != nil {
			return rep, err
		}
		rep.Wrote++
	}
	return rep, nil
}

func writeFileMode(dest string, data []byte, mode fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".vibe-restore-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, dest)
}

// readArchiveFn is the seam the "changed after verification" guard is
// tested through: the race it closes cannot be produced from outside the
// process, and a guard nothing exercises is a guard nobody trusts.
var readArchiveFn = readArchive

func readArchive(archive string) (map[string][]byte, error) {
	f, err := os.Open(archive) //nolint:gosec // an operator-named archive path
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	out := map[string][]byte{}
	total := int64(0)
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg || h.Name == ManifestName {
			continue
		}
		if err := safeName(h.Name); err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxFileBytes+1))
		if err != nil {
			return nil, err
		}
		total += int64(len(data))
		if int64(len(data)) > maxFileBytes || total > maxTotalBytes {
			return nil, fmt.Errorf("%s: archive exceeds this build's size limits", archive)
		}
		out[h.Name] = data
	}
}
