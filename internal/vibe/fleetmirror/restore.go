package fleetmirror

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
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
	"syscall"
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
	// Force skips the takeover probe entirely. It exists because the probe
	// has one honest false positive — see ScanTakeover — and it is also
	// the only escape from a probe that could not settle an address. That
	// is deliberate: it is the one flag in this design that asserts a
	// HUMAN confirmed the old front is down, and a gentler second flag for
	// "the probe timed out, proceed anyway" would be an easier lever
	// reaching the same irreversible place.
	Force bool
	// Dial is injectable so the probe is testable without a listener.
	Dial       func(network, addr string, timeout time.Duration) (net.Conn, error)
	ProbeAddrs []string // overrides the manifest's recorded identity (tests, drills)
	// DialTimeout bounds ONE address's dial. It is a real bound, not a
	// hint: ScanTakeover hands it to the dialer AND holds the dialer to
	// it, because a probe that hangs stops a recovery that is already
	// happening because something died.
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

// TakeoverUnknown is one address the probe dialed and could NOT settle:
// the dial came back neither as an answer nor as a definite refusal, so
// the question "is the old front still serving" was not answered at all.
//
// It is not a hit, and it is emphatically not a miss. A front that is
// off and a front that is alive behind a firewall that DROPs are the
// same silence from a standby, and the whole of C19 rests on a human
// having established which one it is.
type TakeoverUnknown struct {
	What string `json:"what"`
	Addr string `json:"addr"`
	Why  string `json:"why"`
}

// TakeoverScan is everything one probe learned, with the three states
// kept apart. The pair of an empty Hits slice and a non-empty Settled
// slice is the ONLY shape that means "nothing is there, go ahead".
type TakeoverScan struct {
	// Hits are addresses that answered. Any hit is a refusal.
	Hits []TakeoverHit
	// Settled is every address whose dial gave a DEFINITE answer either
	// way — it answered, or the stack said nothing is listening there.
	// These are the only addresses about which anything was learned.
	Settled []string
	// Unknown is every address whose dial ran out its budget or failed in
	// a way that is not a definite negative. Empty Hits plus a non-empty
	// Unknown is not evidence of a dead front; it is the absence of
	// evidence, and Restore refuses on it.
	Unknown []TakeoverUnknown
}

// RestoreReport is what the command prints. It is also what the drill
// asserts on.
type RestoreReport struct {
	Manifest *Manifest     `json:"manifest"`
	Actions  []Action      `json:"actions"`
	Takeover []TakeoverHit `json:"takeover,omitempty"`
	// Probed is every address the takeover probe SETTLED — it answered, or
	// the stack said nothing is listening there. It is reported rather
	// than counted silently because an empty probe and a quiet one are
	// the same absence of hits and completely different evidence.
	//
	// An address whose dial timed out is deliberately NOT here: "probed"
	// has always meant "this address is real evidence", and a dial that
	// never came back is not. It is in Unresolved instead.
	Probed []string `json:"probed,omitempty"`
	// Unresolved names the addresses the probe could not settle. A
	// restore stops on any of them: see ErrProbeInconclusive.
	Unresolved []TakeoverUnknown `json:"unresolved,omitempty"`
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

// ErrProbeInconclusive is returned when the probe dialed the fleet's own
// address and could not find out whether anything is there.
//
// It is ErrNoProbeTargets' sibling, one layer in: that one is "nothing
// to dial", this one is "dialed, learned nothing". Before it existed, a
// dial that TIMED OUT and a dial that came back REFUSED produced the
// identical empty hit list, so an old front sitting behind a firewall
// that DROPs packets — the ordinary way a half-dead host presents —
// read as a dead one and the restore PROCEEDED. That is two fronts under
// one identity, which is the exact disaster the takeover refusal exists
// to prevent, arrived at through the guard rather than around it.
//
// A refusal means "nothing is there, go ahead". A timeout means "I could
// not find out". Collapsing them resolved the ambiguity in the one
// direction that cannot be undone afterwards.
var ErrProbeInconclusive = errors.New("the takeover probe could not find out whether the fleet's address still answers")

// ErrPartialState is returned when a restore would leave a state or
// config dir holding SOME of this archive's files beside files that were
// already there. See Restore.
var ErrPartialState = errors.New("this box already holds part of a fleet's state")

// ScanTakeover dials the addresses the manifest recorded as the fleet's
// identity and reports what each dial established — and, just as
// loadbearing, which of them established nothing.
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
//
// The scan separates three states that all used to arrive as an empty
// hit list, and only ONE of them means "go ahead":
//
//   - Hits: something answered. Refuse.
//   - Settled with no hits: every recorded address gave a definite
//     negative. This is the dead old front, and the only green light.
//   - Unknown: a dial ran out its budget, or failed in a way that is not
//     a definite negative. Nothing was learned; refuse.
//
// A fourth is the caller's: NOTHING was dialable — hosts.yaml absent at
// mirror time, `fleetd_url` unset (it is `omitempty`), no `front` cell
// declared. That leaves all three slices empty, and Restore answers it
// with ErrNoProbeTargets.
//
// Addresses are dialed in sequence, so the whole scan is bounded by
// N × 2 × DialTimeout — 8s for the two recorded addresses at the 2s
// default, and N × 4s when an operator repeats --probe-addr. Sequential
// on purpose: this is the guard everything else rests on, two addresses
// is the normal case, and doctor's parallel fan-out buys seconds at the
// cost of being harder to reason about here.
func ScanTakeover(opts RestoreOptions, id Identity) TakeoverScan {
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
	var scan TakeoverScan
	for _, t := range targets {
		addr := dialAddr(t.raw)
		if addr == "" {
			continue
		}
		switch verdict, why := probeOne(dial, addr, timeout); verdict {
		case verdictAnswered:
			scan.Settled = append(scan.Settled, addr)
			scan.Hits = append(scan.Hits, TakeoverHit{What: t.what, Addr: addr})
		case verdictNothingThere:
			scan.Settled = append(scan.Settled, addr)
		default:
			scan.Unknown = append(scan.Unknown, TakeoverUnknown{What: t.what, Addr: addr, Why: why})
		}
	}
	return scan
}

// TakeoverProbe is ScanTakeover's two-return shape, kept for callers
// that predate the third state.
//
// Deprecated: it cannot express "I could not find out", so use
// ScanTakeover. What it does NOT do is hand that state back as a healthy
// value: if ANY address went unsettled it returns an empty second slice,
// so a caller holding the old `len(probed) == 0` guard refuses rather
// than proceeds. This is the function whose empty second return means
// "stop".
//
// Dropping the unsettled addresses and returning the settled ones was
// not enough, and the mixed scan is exactly why: fleetd refused (host
// rebooted) and the front timed out (alive, behind a DROP rule) left one
// address in `probed` and no hits — the all-clear, for the one scan that
// most needs a refusal. A caller that cannot see the third state must be
// told nothing rather than a subset.
func TakeoverProbe(opts RestoreOptions, id Identity) ([]TakeoverHit, []string) {
	scan := ScanTakeover(opts, id)
	if len(scan.Unknown) > 0 {
		return scan.Hits, nil
	}
	return scan.Hits, scan.Settled
}

// probeVerdict is what one dial established, if anything.
type probeVerdict int

const (
	probeVerdictZero    probeVerdict = iota // unused: the zero value is nobody's answer
	verdictAnswered                         // something is serving there
	verdictNothingThere                     // the stack gave a DEFINITE negative
	verdictUnsettled                        // no answer either way — see TakeoverUnknown
)

type dialOutcome struct {
	conn net.Conn
	err  error
}

// probeOne dials one address and says what that settled.
//
// The dial runs behind a hard deadline of its own. opts.Dial is an
// injectable seam, seams take fakes and wrappers, and the moment a probe
// runs is the worst moment in the fleet's life for one to hang forever
// because a dialer ignored the timeout it was handed. A dialer that has
// not returned in twice its own budget is not going to, and a probe that
// overran is unsettled — never a miss.
func probeOne(dial func(network, addr string, timeout time.Duration) (net.Conn, error), addr string, timeout time.Duration) (probeVerdict, string) {
	// Twice the dialer's own budget, except where doubling overflowed —
	// at which point the honest bound is the budget itself rather than a
	// negative duration that fires before the dial has begun.
	watchdog := 2 * timeout
	if watchdog <= 0 {
		watchdog = timeout
	}
	ch := make(chan dialOutcome, 1)
	go func() {
		conn, err := dial("tcp", addr, timeout)
		ch <- dialOutcome{conn: conn, err: err}
	}()
	select {
	case out := <-ch:
		// The CONNECTION is checked first, and before the error. A dialer
		// that returns both — a proxy, a happy-eyeballs wrapper, a future
		// TLS seam reporting a non-fatal condition — established a session
		// with something, which is the strongest evidence there is that the
		// old front is up. Reading its error and discarding the conn scored
		// that as "nothing is there" AND leaked the socket into the process
		// about to write a state dir.
		if out.conn != nil {
			out.conn.Close()
			return verdictAnswered, ""
		}
		if out.err != nil {
			return classifyDialErr(out.err)
		}
		// Neither a connection nor an error. Nothing in the stdlib does
		// this; a fake or a wrapper can, and reading it as "nothing
		// answered" would be a guess made by the code that exists to stop
		// guessing. (It also used to panic on the Close above, mid-recovery,
		// with a live front possibly still up.)
		return verdictUnsettled, "the dialer returned neither a connection nor an error"
	case <-time.After(watchdog):
		// The dial may still land. Take the connection off the buffered
		// channel and close it rather than leaking a socket into a process
		// that is about to write a state dir.
		go func() {
			if out := <-ch; out.conn != nil {
				out.conn.Close()
			}
		}()
		return verdictUnsettled, "the dial had not returned after " + watchdog.String() +
			", the " + timeout.String() + " it was given twice over"
	}
}

// classifyDialErr sorts a failed dial into "nothing is there" and "I
// could not find out", and the list of things that mean the FIRST is
// closed by construction: a definite negative is a definite negative,
// and everything else is doubt.
//
// The direction matters more than the membership. A new error class
// nobody thought about lands in doubt and refuses a restore, which costs
// an operator one `--force` after they have confirmed the old front is
// down; the other default costs two fronts under one identity, folding
// the same cumulative totals into two ledgers that can never afterwards
// be reconciled.
func classifyDialErr(err error) (probeVerdict, string) {
	// The host's own stack answered the SYN with a RST: it is up enough to
	// say "nothing is listening on this port". That is the ordinary shape
	// of a front host that has been rebooted or whose llama-swap is
	// stopped, and it is the only network answer that means absence.
	//
	// ECONNRESET is deliberately NOT here. It means something accepted or
	// answered and then tore the connection down — a reverse proxy that is
	// up and shedding load, a middlebox — which is evidence of a LIVE
	// peer, nearer a hit than a miss. It falls through to doubt below.
	if errors.Is(err, syscall.ECONNREFUSED) {
		return verdictNothingThere, ""
	}
	// The recorded name does not resolve FROM HERE — and "from here" is
	// the whole of it. NXDOMAIN was a definite negative for two phases on
	// the reasoning that "the identity a standby assumes IS that name; if
	// it resolves nowhere, no client is reaching the old front by it",
	// which reasons about the wrong host. Resolution happens on the box
	// doing the asking, and the box doing the asking is by construction
	// the one that was NOT the front: a standby on another VLAN whose
	// resolver has no internal zone, or whose /etc/resolv.conf points at
	// the front host that just died, NXDOMAINs every recorded name while
	// the old front is alive and serving every client that already holds
	// its address.
	//
	// That is the shape this branch has to fail closed on, because it is
	// not a rare one: a resolver that answers nothing answers nothing for
	// EVERY recorded name at once, so the whole scan settles clean and the
	// restore proceeds. Nothing about the old front was learned; what was
	// learned is that this box cannot ask. An operator who has confirmed
	// the old front is down pays one --force, or names an address the
	// probe can settle with --probe-addr.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return verdictUnsettled, "the name does not resolve from here, which is a fact about THIS box's " +
			"resolver and not about the old front: a standby whose resolver lacks the internal zone " +
			"NXDOMAINs every recorded name, including the name of a front that is alive and serving"
	}
	// A timeout is the dangerous one, and it is the ordinary presentation
	// of a firewall that DROPs rather than REJECTs.
	var netErr net.Error
	if (errors.As(err, &netErr) && netErr.Timeout()) ||
		errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return verdictUnsettled, "the dial timed out: from here, a front that is off and a live one behind a " +
			"DROP rule are the same silence"
	}
	return verdictUnsettled, err.Error() + " — neither an answer nor a definite refusal"
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
		scan := ScanTakeover(opts, m.Identity)
		probed := scan.Settled
		rep.Probed = probed
		rep.Unresolved = scan.Unknown
		if len(scan.Hits) > 0 {
			rep.Takeover = scan.Hits
			var names []string
			for _, h := range scan.Hits {
				names = append(names, h.What+" ("+h.Addr+")")
			}
			return rep, fmt.Errorf("%w: %s. Confirm the old front is DOWN before standing up a second one — "+
				"two fleetds both accept announces, both fire warm and sleep schedules, and both fold the same "+
				"cumulative totals into two ledgers that cannot afterwards be reconciled. "+
				"If this is the standby answering on an address you already moved, re-run with --force",
				ErrTakeover, strings.Join(names, ", "))
		}
		// Checked BEFORE the empty-probe branch below, and after the hits
		// above: an answered address is the more specific finding, and
		// "dialed and learned nothing" must not be reported as "there was
		// nothing to dial" — those send an operator to two different
		// places.
		if len(scan.Unknown) > 0 {
			var names []string
			for _, u := range scan.Unknown {
				names = append(names, u.What+" ("+u.Addr+"): "+u.Why)
			}
			return rep, fmt.Errorf("%w: %s. This is about the DIAL, not about the host — nothing was learned, "+
				"and an empty hit list from a probe that never settled is not evidence that the old front is dead. "+
				"Reading it as one is how a front that is merely firewalled off, or merely absent from THIS "+
				"box's resolver, becomes a second LIVE fleetd folding the same "+
				"cumulative totals into a second ledger that can never afterwards be reconciled. "+
				"Confirm the old front is DOWN by hand and re-run with --force, or give the probe an address it "+
				"can settle with --probe-addr",
				ErrProbeInconclusive, strings.Join(names, ", "))
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
