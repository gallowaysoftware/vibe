// Package fleetmirror captures the fleet state that dies with the front
// host, so a spare box can assume the front's identity by hand
// (fleet-control C19).
//
// It is deliberately NOT high availability. Nothing here elects a
// leader, watches a heartbeat or promotes anything: the control plane
// changes what the catalog SAYS, never where a request GOES, and an
// automatic front promotion is exactly the silent rerouting that rule
// forbids. What this package does is make a MANUAL recovery fast and
// rehearsable — one archive with everything the standby needs, a
// manifest that says what is in it and what is not, and a restore that
// refuses to run while the box it is replacing still answers.
//
// Three properties are load-bearing:
//
//   - The archive is a claim that can be checked. Every entry carries a
//     sha256 and Verify recomputes it, because a mirror nobody has ever
//     read back is a belief, not a backup.
//   - What it does NOT cover is enumerated in the manifest, not left to
//     the reader's memory. Model weights, the cells' own state, the
//     credential VALUES and DNS itself are all outside it, and a
//     recovery that discovers that at 2am has already failed.
//   - Credentials referenced from hosts.yaml are recorded as PATHS and
//     never as bytes. fleetd's own token is the identity being assumed
//     and is carried (excluding it turns a recovery into a fleet-wide
//     re-key); other boxes' secrets are not, because a mirror that
//     scoops up every credential it can reach is how one compromised
//     backup target becomes a compromised fleet.
package fleetmirror

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
)

const (
	// ManifestVersion is the archive format's version. A restore refuses
	// a version it does not know rather than guessing at the layout.
	ManifestVersion = 1
	// ManifestName is the manifest's path inside the archive. Written
	// FIRST, so `tar tzf` shows a human what they are holding before
	// anything else — Verify itself tolerates any order, because an
	// archive is not the place to enforce a convenience.
	ManifestName = "manifest.json"
	// ArchivePrefix/ArchiveSuffix bound what `--keep` is allowed to
	// delete: pruning matches this package's own naming and nothing else.
	ArchivePrefix = "fleet-mirror-"
	ArchiveSuffix = ".tar.gz"
	// ArchiveTimeFormat is UTC and sorts lexically, so "newest" is a
	// string comparison and does not depend on a filesystem's mtime.
	ArchiveTimeFormat = "20060102T150405Z"

	maxFileBytes  = 256 << 20
	maxTotalBytes = 512 << 20
)

// Slot is where an entry came from, and therefore where a restore puts
// it back. The archive stores a slot plus a relative path rather than an
// absolute one: a standby's directory layout is its own business, and
// unpacking absolute paths out of an archive is how a backup tool
// becomes an arbitrary-write primitive.
type Slot string

const (
	SlotState  Slot = "state"  // $XDG_STATE_HOME/vibe
	SlotConfig Slot = "config" // $XDG_CONFIG_HOME/vibe
	SlotFront  Slot = "front"  // the front's rendered llama-swap config + extras
	SlotExtra  Slot = "extra"  // whatever --include named; never auto-restored
)

// Entry is one captured file.
type Entry struct {
	Slot Slot `json:"slot"`
	// Rel is the path relative to the slot root ("fleet/intent.json").
	Rel string `json:"rel"`
	// Archive is the path inside the tar ("state/fleet/intent.json").
	Archive string `json:"archive"`
	// Source is where it was read from, for the operator's benefit. A
	// restore never uses it.
	Source  string      `json:"source"`
	Size    int64       `json:"size"`
	Mode    fs.FileMode `json:"mode"`
	ModTime time.Time   `json:"mod_time"`
	SHA256  string      `json:"sha256"`
	// Secret marks a file whose CONTENT is a credential. --no-secrets
	// drops exactly these.
	Secret bool `json:"secret,omitempty"`
	// Why states what the fleet loses without this file. It is in the
	// manifest rather than only in this source file because the person
	// reading it is mid-incident on a different box.
	Why string `json:"why,omitempty"`
}

// Reference is a credential the fleet USES and this archive deliberately
// does not carry: the path, whether it existed, and its mode. The values
// live in the private fleet repo (ground rule 3), and the runbook's job
// is to say what to fetch from where.
type Reference struct {
	Kind   string      `json:"kind"`
	Cell   string      `json:"cell,omitempty"`
	Path   string      `json:"path"`
	Exists bool        `json:"exists"`
	Mode   fs.FileMode `json:"mode,omitempty"`
	Why    string      `json:"why,omitempty"`
}

// Identity is what a standby has to ASSUME. Recorded because the
// addresses are the fleet's real coupling: cells announce to fleetd_url,
// every client dials the front's url, and a spare box is only the front
// once it answers on those.
type Identity struct {
	FleetdURL string `json:"fleetd_url,omitempty"`
	FrontURL  string `json:"front_url,omitempty"`
	// FrontImage is fleet.front_image — the digest the standby must run.
	// A standby that pulls a different llama-swap is how a recovery turns
	// into the 2026-08-05 in-flight-wire incident (C16).
	FrontImage string       `json:"front_image,omitempty"`
	Timezone   string       `json:"timezone,omitempty"`
	Cells      []CellRecord `json:"cells,omitempty"`
}

// CellRecord is one membership row, flattened for a human reading the
// manifest on a box that does not have hosts.yaml yet.
type CellRecord struct {
	Name      string `json:"name"`
	URL       string `json:"url,omitempty"`
	Class     string `json:"class,omitempty"`
	DaemonURL string `json:"daemon_url,omitempty"`
}

// Manifest is the archive's self-description: what is in it, what is
// referenced, what is missing, and what it does not cover.
type Manifest struct {
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	Host      string    `json:"host,omitempty"`
	Vibe      string    `json:"vibe,omitempty"`

	StateDir    string `json:"state_dir,omitempty"`
	ConfigDir   string `json:"config_dir,omitempty"`
	FrontConfig string `json:"front_config,omitempty"`

	Identity   Identity    `json:"identity"`
	Entries    []Entry     `json:"entries"`
	References []Reference `json:"references,omitempty"`
	// Missing names known state files that were not there. Usually
	// benign (a fleet with no leases has no leases.json) and always
	// stated, because "absent" and "not looked for" must not read the
	// same (C13).
	Missing []string `json:"missing,omitempty"`
	// Errors are files that exist and could not be read. A mirror with
	// errors is a partial mirror and says so everywhere it is rendered.
	Errors []string `json:"errors,omitempty"`
	// NotCovered is the fixed list of things no mirror of the front host
	// can contain.
	NotCovered []string `json:"not_covered"`
	// Gaps are the things this archive was SUPPOSED to carry and does
	// not: --no-secrets drops, a missing config dir, no backend defs, a
	// guest token outside the mirrored state dir. Separate from Warnings
	// because they are the difference between an archive that can restore
	// the fleet and one that cannot, and `vibe fleet doctor` scores them —
	// a capture gap rendering as a green mirror.contents is this repo's
	// oldest defect class (absent evidence wearing a healthy value).
	Gaps []string `json:"gaps,omitempty"`
	// Warnings are advisory: true of a correct configuration, worth
	// knowing, not a reason to score the mirror. A permanent WARN on a
	// healthy fleet is one an operator learns to ignore (C13).
	Warnings []string `json:"warnings,omitempty"`
	// Credentials reports that the archive carries at least one secret
	// file, which makes the archive itself as sensitive as the front
	// host.
	Credentials bool `json:"credentials"`
}

// Receipt is what the mirror leaves BEHIND on the front host, in the
// state dir fleetd already reads, so `vibe fleet doctor` can answer "is
// the mirror fresh" without a new mount, a new route or a network call.
// A stale backup is the classic failure of this whole idea.
type Receipt struct {
	Version     int       `json:"version"`
	At          time.Time `json:"at"`
	Archive     string    `json:"archive"`
	Dest        string    `json:"dest"`
	Bytes       int64     `json:"bytes"`
	Files       int       `json:"files"`
	Missing     []string  `json:"missing,omitempty"`
	Errors      []string  `json:"errors,omitempty"`
	Gaps        []string  `json:"gaps,omitempty"`
	Warnings    []string  `json:"warnings,omitempty"`
	Credentials bool      `json:"credentials"`
	Host        string    `json:"host,omitempty"`
}

// Options configures Create. Every path is explicit: fleetd's state and
// config dirs are bind mounts on the front host and almost never the
// XDG defaults of whoever runs the mirror.
type Options struct {
	StateDir  string
	ConfigDir string
	// FrontConfig / FrontExtras are the front's rendered llama-swap
	// config and the operator-owned YAML merged into it, as seen FROM
	// THE HOST. fleetd's own fleet.front_config is a path inside its
	// container, so the caller resolves these and may legitimately have
	// neither.
	FrontConfig string
	FrontExtras string
	// Include names extra files or directories (the compose stack, the
	// .env, a unit file). They are archived under extras/ and never
	// auto-restored.
	Include []string
	// Out is the mirror destination directory. It must not live under
	// StateDir or ConfigDir: a mirror that dies with the box is not a
	// mirror.
	Out string
	// Keep bounds how many of this package's own archives survive in Out.
	// Zero keeps everything.
	Keep int
	// NoSecrets drops every entry whose CONTENT is a credential — the
	// control-plane token, the guest token, and the front's rendered
	// config and extras (which carry llama-swap's apiKeys after C15).
	// The trade is stated in the runbook: the recovery then includes a
	// re-key and a re-render.
	NoSecrets bool

	Hosts      *fleetcfg.File
	FrontImage string
	Timezone   string
	GuestToken string // fleet.guest_token_file, absolute
	NotifyURL  string // fleet.notify.url_file, absolute
	NotifyTok  string // fleet.notify.token_file, absolute
	Vibe       string
	Host       string
	Now        func() time.Time
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now().UTC()
	}
	return time.Now().UTC()
}

// stateFile is one row of the enumeration this phase is really about:
// the state that must survive the front host dying. Producer is the
// paths.go function that owns the location, which is what binds this
// table to the code — a future phase that adds a state file and does not
// add it here fails TestMirrorCoversEveryFleetStateFile.
type stateFile struct {
	producer string
	rel      string
	secret   bool
	// appendOnly marks a file whose history is the file: replacing it
	// with an older copy DESTROYS rows that exist nowhere else. The
	// restore side reads this table rather than the manifest, so an
	// archive written by an older build gets the same protection.
	appendOnly bool
	why        string
}

// knownState is derived from paths.go rather than spelled out, so
// moving a file moves the mirror with it.
func knownState() []stateFile {
	rel := func(abs string) string {
		r, err := filepath.Rel(paths.StateHome(), abs)
		if err != nil {
			return abs
		}
		return filepath.ToSlash(r)
	}
	return []stateFile{
		{producer: "TokenFile", rel: rel(paths.TokenFile()), secret: true,
			why: "the control-plane bearer. A standby that mints a fresh one 401s every cell, every client and every MCP registration at once."},
		{producer: "IntentFile", rel: rel(paths.IntentFile()),
			why: "declared cell intent (axis 2). Losing it turns every deliberate drain into a DRAINED? mystery and re-arms C9's absence alarms."},
		{producer: "LastSeenFile", rel: rel(paths.LastSeenFile()),
			why: "last sightings of absent cells. Losing it makes every away box look newly absent."},
		{producer: "LeasesFile", rel: rel(paths.LeasesFile()),
			why: "advisory leases and C11 holds. Losing a hold re-arms the warm policy against a model somebody is evaluating."},
		{producer: "StartHistoryFile", rel: rel(paths.StartHistoryFile()),
			why: "cold-start ETAs. Losing it costs accuracy, not correctness."},
		{producer: "UsageLedgerFile", rel: rel(paths.UsageLedgerFile()), appendOnly: true,
			why: "the append-only token ledger. IRREPLACEABLE: cells announce CUMULATIVE totals, so a fresh ledger restarts the running total rather than back-filling, and C7b's payback bars are computed from it."},
		{producer: "FrontCloudUsageFile", rel: rel(paths.FrontCloudUsageFile()),
			why: "the cursor for the front's cloud_peer spend. Losing it re-ingests whatever the front's activity log still holds, double-counting that window."},
		{producer: "NotifyScopeFile", rel: rel(paths.NotifyScopeFile()),
			why: "the declared away/home notification scope. Losing it delivers holiday alarms."},
		{producer: "MirrorReceiptFile", rel: rel(paths.MirrorReceiptFile()),
			why: "the previous mirror's receipt, so a restored fleetd knows how old its own state was."},
		// Cell-side files. A fleetd host is usually not a cell, so these
		// are normally absent — and their absence here is the point: C8's
		// probe baselines and the C7a cell cursor live on the CELLS and do
		// NOT die with the front.
		{producer: "CellIntentFile", rel: rel(paths.CellIntentFile()),
			why: "this box's own cell intent, if it is also a cell. Cells keep their own copy; the front's death does not lose another box's."},
		{producer: "CellUsageFile", rel: rel(paths.CellUsageFile()),
			why: "this box's own usage cursor, if it is also a cell."},
		{producer: "CellProbeFile", rel: rel(paths.CellProbeFile()),
			why: "this box's own C8 probe baselines, if it is also a cell. Every other cell's baselines live on that cell and survive the front."},
	}
}

// appendOnlyDests is the RECEIVING side's copy of which restored files
// are append-only. It is derived from the same table Create captures
// from, and it is consulted on the standby rather than trusted from the
// archive's manifest, because "the sending side guards and the receiving
// side does not" is this repo's most recurring defect.
func appendOnlyDests() map[string]bool {
	out := map[string]bool{}
	for _, sf := range knownState() {
		if sf.appendOnly {
			out[path.Join(string(SlotState), sf.rel)] = true
		}
	}
	return out
}

// NotCovered is the fixed, deliberate list of what a mirror of the front
// host cannot contain. It is data rather than prose so `verify` prints
// it on the box doing the recovery.
func NotCovered(frontImage string) []string {
	img := strings.TrimSpace(frontImage)
	if img == "" {
		img = "(undeclared — see fleet.front_image)"
	}
	return []string{
		"model weights: the GGUF/EXL3 library is not here. A standby front needs none — the front serves peers, not models.",
		"the llama-swap image: the standby must run " + img + ". Pulling a different one is how a recovery becomes the v247 in-flight-wire incident (C16).",
		"the cells' own state: cell-intent, the C7a usage cursor and C8's probe baselines live on each CELL and survive the front dying.",
		"credential VALUES referenced by hosts.yaml (cell token_file, swap_key_file) and by fleet.notify: paths are recorded, bytes are not. Fetch them from the private fleet repo.",
		"DNS, DHCP reservations and the LAN address itself: assuming the front's identity is a change out there, not in here.",
		"systemd units, docker itself, and the vibe binary: rebuild or reinstall them from this repo.",
		"anything not passed to --include: the compose stack and its .env are on the front host's disk and are not found by guessing.",
	}
}

// Create writes one archive, its sidecar manifest and the receipt.
func Create(opts Options) (*Manifest, *Receipt, error) {
	if strings.TrimSpace(opts.Out) == "" {
		return nil, nil, errors.New("no destination: pass --out (a directory that is NOT on the front host's own disk)")
	}
	if opts.StateDir == "" {
		return nil, nil, errors.New("no state dir")
	}
	for _, guard := range []struct{ name, dir string }{
		{"state dir", opts.StateDir},
		{"config dir", opts.ConfigDir},
	} {
		if guard.dir == "" {
			continue
		}
		if under(opts.Out, guard.dir) {
			return nil, nil, fmt.Errorf("--out %s is inside the %s (%s): a mirror stored on the box it is mirroring dies with it",
				opts.Out, guard.name, guard.dir)
		}
	}

	m := &Manifest{
		Version:    ManifestVersion,
		CreatedAt:  opts.now(),
		Host:       opts.Host,
		Vibe:       opts.Vibe,
		StateDir:   opts.StateDir,
		ConfigDir:  opts.ConfigDir,
		NotCovered: NotCovered(opts.FrontImage),
	}
	m.Identity = identityOf(opts)
	m.Warnings = append(m.Warnings, identityWarnings(m.Identity)...)

	files := map[string][]byte{}
	add := func(e Entry, data []byte) {
		if opts.NoSecrets && e.Secret {
			m.Gaps = append(m.Gaps, "--no-secrets: "+e.Archive+" was not captured ("+e.Why+")")
			return
		}
		if e.Secret {
			m.Credentials = true
		}
		sum := sha256.Sum256(data)
		e.SHA256 = hex.EncodeToString(sum[:])
		e.Size = int64(len(data))
		m.Entries = append(m.Entries, e)
		files[e.Archive] = data
	}

	total := int64(0)
	read := func(src string) ([]byte, os.FileInfo, error) {
		fi, err := os.Stat(src)
		if err != nil {
			return nil, nil, err
		}
		if fi.IsDir() {
			return nil, nil, errors.New("is a directory")
		}
		if fi.Size() > maxFileBytes {
			return nil, nil, fmt.Errorf("%d bytes exceeds the %d-byte per-file limit", fi.Size(), int64(maxFileBytes))
		}
		if total+fi.Size() > maxTotalBytes {
			return nil, nil, fmt.Errorf("archive would exceed the %d-byte limit", int64(maxTotalBytes))
		}
		data, err := os.ReadFile(src) //nolint:gosec // operator-supplied paths; this command's whole job is reading them
		if err != nil {
			return nil, nil, err
		}
		total += int64(len(data))
		return data, fi, nil
	}

	// ── state ───────────────────────────────────────────────────────────
	for _, sf := range knownState() {
		src := filepath.Join(opts.StateDir, filepath.FromSlash(sf.rel))
		data, fi, err := read(src)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				m.Missing = append(m.Missing, sf.rel+" (never created)")
				continue
			}
			m.Errors = append(m.Errors, src+": "+err.Error())
			continue
		}
		add(Entry{Slot: SlotState, Rel: sf.rel, Archive: path.Join(string(SlotState), sf.rel),
			Source: src, Mode: fi.Mode().Perm(), ModTime: fi.ModTime().UTC(),
			Secret: sf.secret, Why: sf.why}, data)
	}
	// The guest token's location is configurable, so it is not in the
	// table. Captured only when it lives inside the mirrored state dir —
	// which is the deployment the fleetd README requires, because a path
	// one level up is container-local and re-minted on every recreate.
	if gt := strings.TrimSpace(opts.GuestToken); gt != "" {
		if rel, ok := relUnder(opts.StateDir, gt); ok {
			if !haveRel(m.Entries, SlotState, rel) {
				data, fi, err := read(gt)
				switch {
				case errors.Is(err, fs.ErrNotExist):
					m.Missing = append(m.Missing, rel+" (guest token declared, not yet minted)")
				case err != nil:
					m.Errors = append(m.Errors, gt+": "+err.Error())
				default:
					add(Entry{Slot: SlotState, Rel: rel, Archive: path.Join(string(SlotState), rel),
						Source: gt, Mode: fi.Mode().Perm(), ModTime: fi.ModTime().UTC(), Secret: true,
						Why: "the C12 guest read-only bearer. A standby that mints a fresh one silently revokes every guest."}, data)
				}
			}
		} else {
			m.Gaps = append(m.Gaps, "fleet.guest_token_file ("+gt+") is outside the mirrored state dir and was NOT captured")
		}
	}

	// ── config ──────────────────────────────────────────────────────────
	if opts.ConfigDir == "" {
		m.Gaps = append(m.Gaps, "no config dir given: hosts.yaml, config.yaml and the backend defs are NOT in this archive")
	}
	if opts.ConfigDir != "" {
		for _, rel := range []string{
			filepath.Base(paths.HostsFile()),
			filepath.Base(paths.ConfigFile()),
		} {
			src := filepath.Join(opts.ConfigDir, rel)
			data, fi, err := read(src)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					m.Missing = append(m.Missing, rel+" (not in the config dir)")
					continue
				}
				m.Errors = append(m.Errors, src+": "+err.Error())
				continue
			}
			why := "fleet membership: the single source of which cells exist and where they answer."
			if rel == filepath.Base(paths.ConfigFile()) {
				why = "fleetd's role and every declared policy: warm targets and schedules, probe targets, the sleep schedule, notify, the front mount."
			}
			add(Entry{Slot: SlotConfig, Rel: rel, Archive: path.Join(string(SlotConfig), rel),
				Source: src, Mode: fi.Mode().Perm(), ModTime: fi.ModTime().UTC(), Why: why}, data)
		}
		// The backend defs are the render INPUT: without them a standby
		// fleetd refuses to render (an empty defs mount would otherwise
		// write a peerless front config over a good one).
		defs := filepath.Join(opts.ConfigDir, filepath.Base(paths.BackendsDir()))
		if err := walkInto(defs, func(src, rel string, fi os.FileInfo) {
			data, _, err := read(src)
			if err != nil {
				m.Errors = append(m.Errors, src+": "+err.Error())
				return
			}
			arel := path.Join(filepath.Base(paths.BackendsDir()), filepath.ToSlash(rel))
			add(Entry{Slot: SlotConfig, Rel: arel, Archive: path.Join(string(SlotConfig), arel),
				Source: src, Mode: fi.Mode().Perm(), ModTime: fi.ModTime().UTC(),
				Why: "a backend def — the render input. A standby with no defs cannot render the front's config at all."}, data)
		}); err != nil && !errors.Is(err, fs.ErrNotExist) {
			m.Errors = append(m.Errors, defs+": "+err.Error())
		} else if errors.Is(err, fs.ErrNotExist) {
			m.Missing = append(m.Missing, filepath.Base(paths.BackendsDir())+"/ (no defs dir)")
		}
		// A defs dir that exists and holds nothing is the one shape that
		// looks like a successful capture and is not: the standby's render
		// has no input, and fleetd correctly refuses to write a peerless
		// front config over a good one (C3). Say it here, where it is
		// still cheap to fix.
		if defsCaptured(m) == 0 {
			m.Gaps = append(m.Gaps, "no backend defs captured: a standby restored from this archive cannot RENDER the front's config (it can still serve the rendered one)")
		}
	}

	// ── the front's rendered config ─────────────────────────────────────
	for _, f := range []struct{ src, rel, why string }{
		{opts.FrontConfig, "config.yaml",
			"the front's RENDERED llama-swap config. This is the file that makes a standby serve in minutes instead of after a re-render; it carries the fleet's peer list verbatim."},
		{opts.FrontExtras, "extras.yaml",
			"fleet.front_extras: the operator-owned YAML merged into every render (apiKeys, store). Without it the first render on the standby deletes the front's llama-swap credential."},
	} {
		if strings.TrimSpace(f.src) == "" {
			continue
		}
		data, fi, err := read(f.src)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				m.Missing = append(m.Missing, "front/"+f.rel+" ("+f.src+" not readable from here — fleetd sees it inside its container; pass the host path)")
				continue
			}
			m.Errors = append(m.Errors, f.src+": "+err.Error())
			continue
		}
		add(Entry{Slot: SlotFront, Rel: f.rel, Archive: path.Join(string(SlotFront), f.rel),
			Source: f.src, Mode: fi.Mode().Perm(), ModTime: fi.ModTime().UTC(),
			// The rendered config carries whatever front_extras merged in,
			// which after C15 is llama-swap's apiKeys.
			Secret: true, Why: f.why}, data)
	}

	// ── operator extras ─────────────────────────────────────────────────
	for _, inc := range opts.Include {
		abs, err := filepath.Abs(inc)
		if err != nil {
			m.Errors = append(m.Errors, inc+": "+err.Error())
			continue
		}
		fi, err := os.Stat(abs)
		if err != nil {
			m.Errors = append(m.Errors, abs+": "+err.Error())
			continue
		}
		if !fi.IsDir() {
			data, fi2, err := read(abs)
			if err != nil {
				m.Errors = append(m.Errors, abs+": "+err.Error())
				continue
			}
			arel := extraRel(abs)
			add(Entry{Slot: SlotExtra, Rel: arel, Archive: path.Join("extras", arel),
				Source: abs, Mode: fi2.Mode().Perm(), ModTime: fi2.ModTime().UTC(),
				Why: "--include: restored by hand, never automatically."}, data)
			continue
		}
		if err := walkInto(abs, func(src, rel string, fi os.FileInfo) {
			data, _, err := read(src)
			if err != nil {
				m.Errors = append(m.Errors, src+": "+err.Error())
				return
			}
			arel := extraRel(src)
			add(Entry{Slot: SlotExtra, Rel: arel, Archive: path.Join("extras", arel),
				Source: src, Mode: fi.Mode().Perm(), ModTime: fi.ModTime().UTC(),
				Why: "--include: restored by hand, never automatically."}, data)
		}); err != nil {
			m.Errors = append(m.Errors, abs+": "+err.Error())
		}
	}

	m.References = references(opts)
	sort.Slice(m.Entries, func(i, j int) bool { return m.Entries[i].Archive < m.Entries[j].Archive })

	if err := os.MkdirAll(opts.Out, 0o700); err != nil {
		return nil, nil, err
	}
	archive := filepath.Join(opts.Out, ArchivePrefix+m.CreatedAt.Format(ArchiveTimeFormat)+ArchiveSuffix)
	// The name is second-resolution; two runs inside one second would
	// otherwise leave one archive and two receipts, the older of which
	// describes a file that no longer exists.
	for i := 1; ; i++ {
		if _, err := os.Stat(archive); errors.Is(err, fs.ErrNotExist) {
			break
		}
		if i > 99 {
			return nil, nil, errors.New("cannot find an unused archive name in " + opts.Out)
		}
		archive = filepath.Join(opts.Out, fmt.Sprintf("%s%s-%d%s", ArchivePrefix, m.CreatedAt.Format(ArchiveTimeFormat), i, ArchiveSuffix))
	}
	n, err := writeArchive(archive, m, files)
	if err != nil {
		return nil, nil, err
	}
	if err := writeJSONFile(strings.TrimSuffix(archive, ArchiveSuffix)+".manifest.json", m, 0o600); err != nil {
		return nil, nil, err
	}

	rc := &Receipt{
		Version: ManifestVersion, At: m.CreatedAt, Archive: archive, Dest: opts.Out,
		Bytes: n, Files: len(m.Entries), Missing: m.Missing, Errors: m.Errors,
		Gaps: m.Gaps, Warnings: m.Warnings, Credentials: m.Credentials, Host: m.Host,
	}
	if err := WriteReceipt(opts.StateDir, rc); err != nil {
		// The archive is written and good; only doctor's freshness answer
		// is lost. Say so loudly rather than failing the mirror — and the
		// direction is safe: doctor then reports "never ran", which adds
		// concern rather than subtracting it.
		m.Warnings = append(m.Warnings, "receipt not written ("+err.Error()+"): `vibe fleet doctor` will report this mirror as never having run")
		rc.Warnings = m.Warnings
	}
	if err := prune(opts.Out, opts.Keep); err != nil {
		m.Warnings = append(m.Warnings, "prune: "+err.Error())
		rc.Warnings = m.Warnings
	}
	return m, rc, nil
}

// defsCaptured counts backend defs in the archive so far.
func defsCaptured(m *Manifest) int {
	n := 0
	prefix := filepath.Base(paths.BackendsDir()) + "/"
	for _, e := range m.Entries {
		if e.Slot == SlotConfig && strings.HasPrefix(e.Rel, prefix) {
			n++
		}
	}
	return n
}

// references records the credential files the fleet uses and this
// archive does not carry.
func references(opts Options) []Reference {
	var refs []Reference
	note := func(p string) (bool, fs.FileMode) {
		fi, err := os.Stat(p)
		if err != nil {
			return false, 0
		}
		return true, fi.Mode().Perm()
	}
	addRef := func(kind, cell, p, why string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		exists, mode := note(p)
		refs = append(refs, Reference{Kind: kind, Cell: cell, Path: p, Exists: exists, Mode: mode, Why: why})
	}
	if opts.Hosts != nil {
		names := make([]string, 0, len(opts.Hosts.Cells))
		for n := range opts.Hosts.Cells {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			c := opts.Hosts.Cells[n]
			addRef("cell_token", n, c.TokenFile, "that cell's vibe daemon bearer: without it a restored fleetd cannot drain, resume or suspend the box (the announce piggyback queue still works)")
			addRef("swap_key", n, c.SwapKeyFile, "that cell's llama-swap API key (C15): without it every fleetd->llama-swap request to the cell is refused")
		}
	}
	addRef("notify_url", "", opts.NotifyURL, "the C9 webhook endpoint, which is bearer-equivalent: without it the standby sends no alarms")
	addRef("notify_token", "", opts.NotifyTok, "the C9 webhook's own token")
	return refs
}

func identityOf(opts Options) Identity {
	id := Identity{FrontImage: opts.FrontImage, Timezone: opts.Timezone}
	if opts.Hosts == nil {
		return id
	}
	id.FleetdURL = opts.Hosts.FleetdURL
	names := make([]string, 0, len(opts.Hosts.Cells))
	for n := range opts.Hosts.Cells {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		c := opts.Hosts.Cells[n]
		if n == fleetcfg.FrontCell {
			id.FrontURL = c.URL
		}
		id.Cells = append(id.Cells, CellRecord{Name: n, URL: c.URL, Class: string(c.Class), DaemonURL: c.DaemonURL})
	}
	return id
}

// identityWarnings says, at mirror time, what the recovery will cost.
// A literal IP is not a misconfiguration — this fleet's front is a
// macvlan container on a static address — but it decides whether the
// takeover is one DNS record or an edit on every box, and that is worth
// knowing before the night it matters rather than during it.
func identityWarnings(id Identity) []string {
	var out []string
	for _, f := range []struct{ what, raw string }{
		{"the front", id.FrontURL},
		{"fleetd", id.FleetdURL},
	} {
		if h := hostOf(f.raw); h != "" && net.ParseIP(h) != nil {
			out = append(out, f.what+" is addressed by literal IP ("+f.raw+"): a standby must TAKE that address. A DNS name would make the takeover one record.")
		}
	}
	return out
}

func hostOf(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(u.Host); err == nil {
		return h
	}
	return u.Host
}

// ── archive io ─────────────────────────────────────────────────────────

func writeArchive(dest string, m *Manifest, files map[string][]byte) (int64, error) {
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".mirror-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()
	// 0600 before a byte is written: the archive carries the fleet's
	// control-plane token unless --no-secrets said otherwise.
	if err := tmp.Chmod(0o600); err != nil {
		return 0, err
	}
	gz := gzip.NewWriter(tmp)
	tw := tar.NewWriter(gz)

	mj, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return 0, err
	}
	if err := writeTarFile(tw, ManifestName, mj, 0o600, m.CreatedAt); err != nil {
		return 0, err
	}
	for _, e := range m.Entries {
		if err := writeTarFile(tw, e.Archive, files[e.Archive], e.Mode, e.ModTime); err != nil {
			return 0, err
		}
	}
	if err := tw.Close(); err != nil {
		return 0, err
	}
	if err := gz.Close(); err != nil {
		return 0, err
	}
	if err := tmp.Sync(); err != nil {
		return 0, err
	}
	fi, err := tmp.Stat()
	if err != nil {
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

func writeTarFile(tw *tar.Writer, name string, data []byte, mode fs.FileMode, mod time.Time) error {
	if mode == 0 {
		mode = 0o600
	}
	h := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Mode:     int64(mode.Perm()),
		Size:     int64(len(data)),
		ModTime:  mod,
		Format:   tar.FormatPAX,
	}
	if err := tw.WriteHeader(h); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

func writeJSONFile(dest string, v any, mode fs.FileMode) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".vibe-*")
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

// WriteReceipt records a mirror run where fleetd can read it.
func WriteReceipt(stateDir string, rc *Receipt) error {
	rel, err := filepath.Rel(paths.StateHome(), paths.MirrorReceiptFile())
	if err != nil {
		return err
	}
	dest := filepath.Join(stateDir, rel)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	return writeJSONFile(dest, rc, 0o644)
}

// ReadReceipt reads the last recorded mirror run. It is READ-ONLY —
// C13's doctor path calls it, and a doctor that creates a file is not
// safe to run mid-incident.
func ReadReceipt(stateDir string) (*Receipt, error) {
	rel, err := filepath.Rel(paths.StateHome(), paths.MirrorReceiptFile())
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(stateDir, rel)) //nolint:gosec // a fixed name under the daemon's own state dir
	if err != nil {
		return nil, err
	}
	var rc Receipt
	if err := json.Unmarshal(data, &rc); err != nil {
		return nil, err
	}
	return &rc, nil
}

// ── helpers ────────────────────────────────────────────────────────────

func walkInto(root string, fn func(src, rel string, fi os.FileInfo)) error {
	fi, err := os.Stat(root)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return errors.New(root + " is not a directory")
	}
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// Symlinks are skipped rather than followed: an archive is not
		// the place to resolve a link into somebody else's file.
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		fn(p, rel, info)
		return nil
	})
}

// extraRel keeps an --include'd file's origin visible inside the archive
// (/srv/front/.env becomes extras/srv/front/.env) so two includes with
// the same basename cannot collide and a human can see where each came
// from.
func extraRel(abs string) string {
	s := filepath.ToSlash(abs)
	s = strings.TrimPrefix(s, "/")
	s = strings.ReplaceAll(s, "../", "")
	if vol := filepath.VolumeName(abs); vol != "" {
		s = strings.ReplaceAll(s, ":", "")
	}
	return s
}

func haveRel(entries []Entry, slot Slot, rel string) bool {
	for _, e := range entries {
		if e.Slot == slot && e.Rel == rel {
			return true
		}
	}
	return false
}

// relUnder reports p's path relative to dir when p is inside it.
func relUnder(dir, p string) (string, bool) {
	ad, err1 := filepath.Abs(dir)
	ap, err2 := filepath.Abs(p)
	if err1 != nil || err2 != nil {
		return "", false
	}
	rel, err := filepath.Rel(ad, ap)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

func under(p, dir string) bool {
	if _, ok := relUnder(dir, p); ok {
		return true
	}
	ad, err1 := filepath.Abs(dir)
	ap, err2 := filepath.Abs(p)
	return err1 == nil && err2 == nil && ad == ap
}

// archiveKey splits an archive name into the stamp it was written at and
// its collision counter. It exists because the two do NOT sort together
// as one string: `…Z-1.tar.gz` compares LOWER than `…Z.tar.gz` ('-' <
// '.'), so a plain lexical sort calls the second run of a second the
// oldest archive in the directory — which made `--keep 1` delete the
// archive whose path had just gone into the receipt, and made `Newest`
// hand a recovery the older of the two.
func archiveKey(name string) (string, int) {
	mid := strings.TrimSuffix(strings.TrimPrefix(name, ArchivePrefix), ArchiveSuffix)
	if i := strings.LastIndex(mid, "-"); i >= 0 {
		if seq, err := strconv.Atoi(mid[i+1:]); err == nil {
			return mid[:i], seq
		}
	}
	return mid, 0
}

// newerFirst orders archives newest-first by (stamp, collision counter).
func newerFirst(a, b string) bool {
	as, an := archiveKey(a)
	bs, bn := archiveKey(b)
	if as != bs {
		return as > bs
	}
	return an > bn
}

func isArchiveName(name string, isDir bool) bool {
	return !isDir && strings.HasPrefix(name, ArchivePrefix) && strings.HasSuffix(name, ArchiveSuffix)
}

// prune deletes all but the newest keep archives, matching only this
// package's own naming so a shared backup directory is never at risk.
func prune(dir string, keep int) error {
	if keep <= 0 {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if !isArchiveName(e.Name(), e.IsDir()) {
			continue
		}
		names = append(names, e.Name())
	}
	if len(names) <= keep {
		return nil
	}
	sort.Slice(names, func(i, j int) bool { return newerFirst(names[i], names[j]) })
	var errs []error
	for _, n := range names[keep:] {
		if err := os.Remove(filepath.Join(dir, n)); err != nil {
			errs = append(errs, err)
		}
		side := filepath.Join(dir, strings.TrimSuffix(n, ArchiveSuffix)+".manifest.json")
		if err := os.Remove(side); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Newest returns the most recent archive in dir, by this package's
// lexically-sortable UTC naming.
func Newest(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	best := ""
	for _, e := range entries {
		n := e.Name()
		if !isArchiveName(n, e.IsDir()) {
			continue
		}
		if best == "" || newerFirst(n, best) {
			best = n
		}
	}
	if best == "" {
		return "", fmt.Errorf("no %s*%s in %s", ArchivePrefix, ArchiveSuffix, dir)
	}
	return filepath.Join(dir, best), nil
}
