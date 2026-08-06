package fleetmirror

import (
	"archive/tar"
	"bytes"
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
		if err := safeName(e.Archive); err != nil {
			problems = append(problems, err)
			continue
		}
		if err := safeRel(e); err != nil {
			problems = append(problems, err)
			continue
		}
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

// safeRel applies the same rule to the manifest's Rel, which is the
// field a restore actually JOINS onto --state-dir. safeName guarding
// only Entry.Archive was a guard in one of two paths: a manifest with a
// harmless `archive` and a `rel` of "../../.." wrote wherever it liked,
// with the mode the manifest asked for. The archive is untrusted input —
// it comes off a backup target, which is the one machine in this design
// that is neither the front nor a cell.
func safeRel(e Entry) error {
	if err := safeName(e.Rel); err != nil {
		return errors.New(e.Archive + ": rel " + err.Error())
	}
	return nil
}

// inside reports whether p lands within root once both are cleaned.
// (mirror.go's `under` answers the same question with its arguments the
// other way round; this one reads in the direction the plan loop asks
// it.) It is the belt to safeRel's braces: the check that actually
// matters is where the path is JOINED, and it costs nothing to make it
// there too.
func inside(root, p string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
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
	// Probed is every address the takeover probe was able to DIAL. It is
	// reported rather than counted silently because an empty probe and a
	// quiet one are the same absence of hits and completely different
	// evidence.
	Probed []string `json:"probed,omitempty"`
	// Preserved names append-only files that were moved aside instead of
	// being replaced.
	Preserved []string `json:"preserved,omitempty"`
	Wrote     int      `json:"wrote"`
	DryRun    bool     `json:"dry_run"`
}

// ErrTakeover is returned when something still answers on the fleet's
// own addresses.
var ErrTakeover = errors.New("the fleet's address still answers")

// ErrNoProbeTargets is returned when the manifest recorded no address
// the probe could dial. The takeover refusal is this phase's whole
// contribution to the two-boxes-answering problem, and a probe with
// nothing to aim at returns the same empty hit list as a probe that
// found the old front dead. Reporting the second when it means the first
// is absent evidence wearing a healthy value — this repo's oldest
// mistake, six occurrences before this one.
var ErrNoProbeTargets = errors.New("nothing to probe: the mirror recorded no fleetd or front address")

// ErrPartialState is returned when a restore would leave a state or
// config dir holding SOME of this archive's files beside files that were
// already there. See Restore.
var ErrPartialState = errors.New("this box already holds part of a fleet's state")

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
// It returns the hits AND every address it was able to dial. The second
// return is not decoration: a manifest whose identity records no URL —
// hosts.yaml absent at mirror time, `fleetd_url` unset (it is
// `omitempty`), no `front` cell declared — yields an empty hit list that
// is indistinguishable from "the old box is dead", and the caller must
// be able to tell those apart.
func TakeoverProbe(opts RestoreOptions, id Identity) ([]TakeoverHit, []string) {
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
	var probed []string
	for _, t := range targets {
		addr := dialAddr(t.raw)
		if addr == "" {
			continue
		}
		probed = append(probed, addr)
		conn, err := dial("tcp", addr, timeout)
		if err != nil {
			continue
		}
		conn.Close()
		hits = append(hits, TakeoverHit{What: t.what, Addr: addr})
	}
	return hits, probed
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
		hits, probed := TakeoverProbe(opts, m.Identity)
		rep.Probed = probed
		if len(hits) > 0 {
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
		if len(probed) == 0 {
			return rep, fmt.Errorf("%w (fleetd_url and the front cell's url are both absent or unparseable). "+
				"The probe found nothing to dial, which is NOT the same as finding the old front dead — and this "+
				"refusal is the only mechanical thing standing between you and two fleetds folding the same "+
				"cumulative totals into two ledgers. Pass --probe-addr host:port for the old front, or --force "+
				"once you have confirmed it is down by hand",
				ErrNoProbeTargets)
		}
	}

	// dest answers where an entry goes, or why it does not go anywhere.
	dest := func(e Entry) (string, string) {
		root := ""
		switch e.Slot {
		case SlotState:
			if opts.StateDir == "" {
				return "", "no --state-dir given"
			}
			root = opts.StateDir
		case SlotConfig:
			if opts.ConfigDir == "" {
				return "", "no --config-dir given"
			}
			root = opts.ConfigDir
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
		return filepath.Join(root, filepath.FromSlash(e.Rel)), ""
	}
	// root is the directory a slot's entries must land inside. Empty for
	// the front slot, whose two destinations are named files.
	root := func(s Slot) string {
		switch s {
		case SlotState:
			return opts.StateDir
		case SlotConfig:
			return opts.ConfigDir
		default:
			return ""
		}
	}

	// Plan first, write second. Every refusal an operator could hit is
	// decided before the first byte lands, so a restore either happens or
	// does not — it never stops halfway with a state dir holding one
	// fleet's token and another fleet's intent.
	var jobs []restoreJob
	kept := map[Slot][]string{}
	for _, e := range m.Entries {
		if err := safeName(e.Archive); err != nil {
			return rep, err
		}
		// Verify already ran safeRel over every entry; repeating it here is
		// the "find every producer" habit applied to one function — Restore
		// is the only caller today, and the join is two lines below.
		if err := safeRel(e); err != nil {
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
		if r := root(e.Slot); r != "" && !inside(r, d) {
			return rep, fmt.Errorf("%s: rel %q lands outside %s — refusing", e.Archive, e.Rel, r)
		}
		if _, err := os.Stat(d); err == nil && !opts.Overwrite {
			kept[e.Slot] = append(kept[e.Slot], d)
			rep.Actions = append(rep.Actions, Action{Archive: e.Archive, Dest: d, Slot: e.Slot, Mode: e.Mode,
				Status: "exists", Note: "already present; --overwrite to replace"})
			continue
		}
		status := "written"
		if opts.DryRun {
			status = "would-write"
		}
		rep.Actions = append(rep.Actions, Action{Archive: e.Archive, Dest: d, Slot: e.Slot, Mode: e.Mode, Status: status})
		jobs = append(jobs, restoreJob{e: e, dest: d})
	}

	// A slot is all or nothing. Skipping the files that already exist and
	// writing the ones that do not produces exactly the hybrid this
	// function's own comment says it never produces: this box's token
	// beside the archive's intent.json, a fleetd that authenticates as one
	// fleet and holds another's declarations. Nothing downstream would
	// ever notice — every file parses. So the mix is refused BEFORE the
	// first byte, and the two clean answers stay available: --overwrite
	// (replace it all) or an empty destination.
	for _, s := range []Slot{SlotState, SlotConfig} {
		if len(kept[s]) == 0 || !slotHasJob(jobs, s) {
			continue
		}
		return rep, fmt.Errorf("%w: %s/ already holds %s, and this archive would add others beside them. "+
			"A state dir with this box's token and another fleet's intent is a fleetd that authenticates as one "+
			"fleet and acts on another's declarations, and nothing downstream notices. Restore into an empty "+
			"directory, or pass --overwrite to replace what is there",
			ErrPartialState, s, strings.Join(kept[s], ", "))
	}

	if opts.DryRun {
		return rep, nil
	}

	payload, err := readArchiveFn(opts.Archive)
	if err != nil {
		return rep, err
	}
	// Verify read the archive; this read it again. Between the two the
	// file could have been replaced — by another mirror run finishing,
	// by a half-copied file on a network mount, by anything — and then
	// what lands on the standby is not what was checked. Every payload is
	// re-hashed HERE, before the first write: doing it inside the write
	// loop meant a tampered last entry aborted a restore that had already
	// placed six files, which is the half-restored state dir the plan
	// step exists to prevent.
	for _, j := range jobs {
		data, ok := payload[j.e.Archive]
		if !ok {
			return rep, errors.New(j.e.Archive + ": vanished between verify and restore — nothing written")
		}
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); got != j.e.SHA256 {
			return rep, fmt.Errorf("%s: content changed after verification (manifest %s, now %s) — nothing written",
				j.e.Archive, short(j.e.SHA256), short(got))
		}
	}

	appendOnly := appendOnlyDests()
	for _, j := range jobs {
		data := payload[j.e.Archive]
		if err := os.MkdirAll(filepath.Dir(j.dest), 0o755); err != nil {
			return rep, err
		}
		// C7a's ledger is append-only and irreplaceable: cells announce
		// CUMULATIVE totals, so rows that go do not come back. --overwrite
		// replacing it with an older copy is a silent, permanent data loss,
		// so anything the archive does not already contain is moved aside
		// first and named in the report. When the on-disk file is a prefix
		// of the archived one the archive is a strict superset and nothing
		// is at risk, so no sidecar is made.
		if appendOnly[j.e.Archive] {
			side, err := preserveAppendOnly(j.dest, data)
			if err != nil {
				return rep, err
			}
			if side != "" {
				rep.Preserved = append(rep.Preserved, side)
				markPreserved(rep, j.e.Archive, side)
			}
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

// restoreJob is one planned write, decided before any byte lands.
type restoreJob struct {
	e    Entry
	dest string
}

func slotHasJob(jobs []restoreJob, s Slot) bool {
	for _, j := range jobs {
		if j.e.Slot == s {
			return true
		}
	}
	return false
}

// preserveAppendOnly moves an existing append-only file aside when the
// bytes about to replace it would lose rows. Returns the sidecar path,
// or "" when nothing needed preserving.
func preserveAppendOnly(dest string, incoming []byte) (string, error) {
	existing, err := os.ReadFile(dest) //nolint:gosec // the destination this restore was asked to write
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("%s: cannot read the append-only file this restore would replace: %w", dest, err)
	}
	if bytes.HasPrefix(incoming, existing) {
		// The archive contains everything on disk and then some: replacing
		// it loses nothing.
		return "", nil
	}
	side := dest + ".pre-restore-" + time.Now().UTC().Format("20060102T150405Z")
	for i := 1; ; i++ {
		if _, err := os.Stat(side); errors.Is(err, fs.ErrNotExist) {
			break
		}
		if i > 99 {
			return "", errors.New(dest + ": cannot find an unused name to preserve the append-only file under")
		}
		side = fmt.Sprintf("%s.pre-restore-%s-%d", dest, time.Now().UTC().Format("20060102T150405Z"), i)
	}
	if err := os.Rename(dest, side); err != nil {
		return "", err
	}
	return side, nil
}

func markPreserved(rep *RestoreReport, archive, side string) {
	for i := range rep.Actions {
		if rep.Actions[i].Archive == archive {
			rep.Actions[i].Note = "append-only: the file that was here is preserved at " + side
		}
	}
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
