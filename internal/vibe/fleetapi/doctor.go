package fleetapi

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
)

// `vibe fleet doctor` (fleet-control C13): the sit-down-after-two-weeks
// command. Twelve phases answer "what is the fleet doing right now"
// extremely well and nothing answers "is the fleet still put together
// correctly" — this does, by composing evidence the substrate already
// collects into verdicts.
//
// Two rules run through every line below.
//
// READ-ONLY. The doctor path issues no verb: it never drains, resumes,
// warms, unloads, probes, wakes, renders, queues a piggyback command,
// writes intent, takes or releases a lease, sends a notification or
// writes any config. It is meant to be the command an operator reaches
// for mid-incident without first reasoning about what it will do.
// Test-pinned both behaviourally (state files and queues are unchanged
// across a run) and structurally (a source scan for mutating
// identifiers).
//
// UNKNOWN IS NOT OK. A check that could not be evaluated says so, in its
// own level, with the reason. This repo has been bitten by absent
// evidence reading as a healthy zero repeatedly (C5's M2 idle floor,
// C7a's unmeasured_req, C8's under-5-samples verdict, C9's
// fingerprint_source) and a doctor — whose whole reward is a screen of
// green — is where that mistake is cheapest to make and most expensive
// to keep.

// Level is one check's verdict. The zero value is deliberately not a
// valid level: a check that forgot to set one must fail a test rather
// than inherit "ok" (C12's AccessUndecided discipline).
type Level string

const (
	LevelOK      Level = "ok"
	LevelWarn    Level = "warn"
	LevelFail    Level = "fail"
	LevelUnknown Level = "unknown"
)

// severity orders the levels for rendering: a tired human reads
// top-down and stops at the first thing that matters.
func (l Level) severity() int {
	switch l {
	case LevelFail:
		return 0
	case LevelWarn:
		return 1
	case LevelUnknown:
		return 2
	case LevelOK:
		return 3
	default:
		return 4
	}
}

// DoctorCheck is one question, asked and answered.
//
// ID names what the check PROVES, never what it wishes it could prove —
// wake.configured rather than wake.armed, tls.not_after rather than
// tls.valid, auth.inbound (an announce that authenticated) rather than
// auth.ok. An operator reading a screen of OK must be reading true
// sentences.
type DoctorCheck struct {
	ID      string `json:"id"`
	Cell    string `json:"cell,omitempty"`
	Level   Level  `json:"level"`
	Summary string `json:"summary"`
	Detail  string `json:"detail,omitempty"`
	Fix     string `json:"fix,omitempty"`
}

// DoctorSummary is the counted roll-up (the CLI's exit code reads it).
type DoctorSummary struct {
	Cells   int `json:"cells"`
	Checks  int `json:"checks"`
	OK      int `json:"ok"`
	Warn    int `json:"warn"`
	Fail    int `json:"fail"`
	Unknown int `json:"unknown"`
}

// DoctorReport is the whole document: the same one GET /api/fleet/doctor
// serves, fleet_doctor returns and `vibe fleet doctor --json` prints.
type DoctorReport struct {
	GeneratedAt time.Time `json:"generated_at"`
	// Fleetd names where the report came from. Filled in by the CLI (the
	// server does not know the address a client dialed it on).
	Fleetd  string        `json:"fleetd,omitempty"`
	Checks  []DoctorCheck `json:"checks"`
	Summary DoctorSummary `json:"summary"`
}

// Add appends a check and keeps the summary honest. Exported because the
// CLI's degraded path (fleetd unreachable) builds a report of its own
// from the same type.
func (r *DoctorReport) Add(c DoctorCheck) {
	r.Checks = append(r.Checks, c)
	r.Summary.Checks++
	switch c.Level {
	case LevelOK:
		r.Summary.OK++
	case LevelWarn:
		r.Summary.Warn++
	case LevelFail:
		r.Summary.Fail++
	default:
		r.Summary.Unknown++
	}
}

// SortBySeverity orders checks FAIL → WARN → UNKNOWN → OK, stable within
// a level so emission order (which is grouped by subject) survives.
func (r *DoctorReport) SortBySeverity() {
	sort.SliceStable(r.Checks, func(i, j int) bool {
		return r.Checks[i].Level.severity() < r.Checks[j].Level.severity()
	})
}

// DoctorHost is the set of fleetd-HOST facts the doctor cannot read from
// inside this package. Supplied by the daemon through Options, exactly
// as DaemonInfo is: fleetapi has never known about the daemon's config,
// its token files or the filesystem it runs on, and this phase does not
// change that.
type DoctorHost struct {
	// StateDir holds intent.json, leases.json, usage.jsonl and
	// last-seen.json — C6's rule is that a failed persist leaves memory
	// unresolved, so this filesystem filling up is a silent failure.
	StateDir string
	// FrontConfig is fleet.front_config: the front's rendered llama-swap
	// config as seen from THIS process. Its being set is the declaration
	// that fleetd shares a filesystem with the front's config dir (C3's
	// render mount contract) — which is the only reason doctor may report
	// anything about the front's disk.
	FrontConfig string
	// TokenMinted reports that this start CREATED the control-plane token
	// rather than loading one. On fleetd that is the signature of a
	// container recreate over an unmounted state dir.
	TokenMinted bool
	// EnvTokenSet reports whether $VIBE_TOKEN is present in fleetd's own
	// environment (the value is never carried).
	EnvTokenSet bool
	// DefsSHA/DefsDirty describe fleetd's own backend-def checkout, the
	// parity reference when it has one.
	DefsSHA   string
	DefsDirty bool
	// Version is fleetd's own vibe build.
	Version string
	// DiskFreeBytes reports free bytes on the filesystem holding path.
	// A func because statfs is not portable and fleetapi is; nil means
	// the disk questions are UNKNOWN rather than assumed fine.
	DiskFreeBytes func(path string) (uint64, error)
}

// CellAuthResult is one outbound credential probe: fleetd calling a
// cell's own daemon with the credential the ACTUATION verbs resolve
// (fleetcfg.CellCredential with the fleetd preference). A doctor that
// resolved credentials its own way would be testing its own code.
type CellAuthResult struct {
	// Attempted is false when the cell declares no daemon_url — after C3
	// that is a legitimate configuration, not a fault, and the announce
	// piggyback queue is that cell's actuation channel.
	Attempted bool
	// OK means the call returned successfully with this credential.
	OK bool
	// Denied means the far side ANSWERED and refused the credential.
	// Distinguished from Err because a 401 is a fact and a timeout is an
	// absence of one.
	Denied bool
	// Source names where the credential came from; never its value.
	Source string
	// CredentialErr is a LOCAL failure to resolve a credential at all (an
	// unreadable or empty token_file). Definitely broken and definitely
	// on fleetd's side, so it is a FAIL rather than the UNKNOWN a
	// transport error earns.
	CredentialErr string
	// Err is the transport or RPC failure, when there was one.
	Err string
}

// Doctor assembles the report. Its evidence is the same StateSnapshot
// every other surface renders (C9's rule that the page, the pager and
// the CLI cannot disagree), plus the presence table, hosts.yaml, the
// injected host facts and one read-only RPC per cell that declares a
// daemon_url.
func (s *Server) Doctor(ctx context.Context) DoctorReport {
	rep := DoctorReport{GeneratedAt: time.Now().UTC()}
	snap := s.Snapshot(ctx)
	pres := s.presenceSnapshot()
	host := DoctorHost{}
	if s.doctorHost != nil {
		host = s.doctorHost()
	}
	rep.Summary.Cells = len(snap.Cells)

	s.checkFleetd(&rep, snap, host)
	s.checkAuth(ctx, &rep, snap, pres, host)
	s.checkVersions(&rep, snap, pres, host)
	s.checkDisk(&rep, pres, host)
	s.checkTLS(ctx, &rep)
	s.checkWakeAndRoaming(&rep, snap, pres)
	s.checkHygiene(&rep, snap)
	return rep
}

// ─── the control plane itself ───────────────────────────────────────────────

func (s *Server) checkFleetd(rep *DoctorReport, snap StateSnapshot, host DoctorHost) {
	if s.intentPath == "" {
		rep.Add(DoctorCheck{
			ID:      "fleetd.role",
			Level:   LevelWarn,
			Summary: "this daemon is not a fleet registry",
			Detail: "fleet_registry is not set, so there is no intent store, no announce endpoint and no MCP facade — " +
				"most of what a fleet doctor asks about does not exist here.",
			Fix: "set fleet_registry: true in config.yaml on the box that runs fleetd.",
		})
		return
	}

	// State dir. Deliberately no probe write: doctor claims read-only, so
	// it reads (stat + free bytes) and does not assert writability, which
	// cannot be established without writing.
	dir := host.StateDir
	switch dir {
	case "":
		rep.Add(DoctorCheck{ID: "fleetd.state_dir", Level: LevelUnknown,
			Summary: "state dir not reported",
			Detail:  "the daemon did not supply its state directory, so free space and existence are unchecked."})
	default:
		if _, err := os.Stat(dir); err != nil {
			rep.Add(DoctorCheck{ID: "fleetd.state_dir", Level: LevelFail,
				Summary: "state dir missing: " + dir,
				Detail: "intent.json, leases.json and usage.jsonl live here. A failed persist leaves fleetd's memory " +
					"holding a resolution the file does not carry (C6).",
				Fix: "check the state volume mount (deploy/fleetd's state contract)."})
			break
		}
		lvl, msg := diskVerdict(host, dir, fleetdDiskWarn, fleetdDiskFail)
		rep.Add(DoctorCheck{ID: "fleetd.state_dir", Level: lvl, Summary: dir + ": " + msg,
			Detail: "holds intent.json, leases.json, usage.jsonl and last-seen.json."})
	}

	// Token minting is the unmounted-state-volume signature.
	switch {
	case host.TokenMinted && snap.Daemon.AuthRejected > 0:
		rep.Add(DoctorCheck{ID: "fleetd.token", Level: LevelWarn,
			Summary: fmt.Sprintf("control-plane token was MINTED at this start, and %d request(s) have been refused since", snap.Daemon.AuthRejected),
			Detail: "that pair is the signature of a container recreate over an unmounted state dir: fleetd wrote a new " +
				"token and every previously-provisioned client now 401s.",
			Fix: "check the state volume mount, then re-provision clients with `vibe token`."})
	case host.TokenMinted:
		rep.Add(DoctorCheck{ID: "fleetd.token", Level: LevelWarn,
			Summary: "control-plane token was MINTED at this start (no existing token file)",
			Detail: "correct exactly once, on first commissioning. Every later occurrence means the state dir did not " +
				"survive, and clients will 401 as they reconnect.",
			Fix: "if this fleetd has run before, check the state volume mount."})
	default:
		rep.Add(DoctorCheck{ID: "fleetd.token", Level: LevelOK,
			Summary: "control-plane token loaded from the state dir (not minted at this start)"})
	}

	// Rejection counters. C12 keeps guest denials separate on purpose:
	// folding them in would destroy the stale-token signal.
	det := ""
	if snap.Daemon.GuestEnabled {
		det = fmt.Sprintf("guest read-only token: on, %d request(s) refused past its two routes.", snap.Daemon.GuestRejected)
	}
	if snap.Daemon.AuthRejected > 0 {
		rep.Add(DoctorCheck{ID: "auth.rejections", Level: LevelWarn,
			Summary: fmt.Sprintf("%d request(s) presented a missing or unknown token since start", snap.Daemon.AuthRejected),
			Detail: strings.TrimSpace("some client holds a stale token. fleetd cannot attribute a rejection to a cell — " +
				"the bearer check happens before any body is parsed. " + det),
			Fix: "check each cell's announcer token and any $VIBE_TOKEN in a shell that talks to fleetd."})
	} else {
		rep.Add(DoctorCheck{ID: "auth.rejections", Level: LevelOK,
			Summary: "no token rejections since start", Detail: det})
	}
}

// diskVerdict renders one filesystem's free space against the two
// thresholds. The NUMBER is always in the message, so an operator who
// disagrees with the thresholds can still use the check.
func diskVerdict(host DoctorHost, path string, warn, fail uint64) (Level, string) {
	if host.DiskFreeBytes == nil {
		return LevelUnknown, "free space not reported (no disk probe wired)"
	}
	free, err := host.DiskFreeBytes(path)
	if err != nil {
		return LevelUnknown, "free space unreadable: " + err.Error()
	}
	msg := fmt.Sprintf("%.1f GiB free", float64(free)/(1<<30))
	switch {
	case free < fail:
		return LevelFail, msg
	case free < warn:
		return LevelWarn, msg
	default:
		return LevelOK, msg
	}
}

// Disk thresholds. Opinions, named here so they can be argued with in
// one place; every message carries the raw number beside the verdict.
const (
	// fleetd's state dir holds small JSON, so the bar is about a
	// filesystem that has effectively stopped accepting writes.
	fleetdDiskFail = 256 << 20
	fleetdDiskWarn = 2 << 30
	// A cell's model filesystem: below this it can no longer pull a
	// quant, which is how a cell quietly leaves the membership-churn loop.
	cellDiskWarnGB = 10.0
)

// ─── credentials, both directions ───────────────────────────────────────────

func (s *Server) checkAuth(ctx context.Context, rep *DoctorReport, snap StateSnapshot, pres map[string]Presence, host DoctorHost) {
	for _, c := range snap.Cells {
		s.checkInbound(rep, c, pres[c.Name], snap.Daemon.AuthRejected)
	}
	// Probed in PARALLEL, like Snapshot's own cell round: each call is
	// bounded at 5s, and run serially a fleet with three boxes off would
	// spend the whole report deadline dialling them — leaving the cells
	// behind them reporting "context deadline exceeded" for a call that
	// was never made. A misdiagnosis is worse than a slow report, and
	// this command exists to diagnose.
	results := make([]CellAuthResult, len(snap.Cells))
	if s.cellAuth != nil {
		var wg sync.WaitGroup
		for i, c := range snap.Cells {
			wg.Add(1)
			go func(i int, name string) {
				defer wg.Done()
				results[i] = s.cellAuth(ctx, name)
			}(i, c.Name)
		}
		wg.Wait()
	}
	for i, c := range snap.Cells {
		s.checkOutbound(rep, c, pres[c.Name], results[i])
	}

	// The C6 finding, surfaced where it belongs. Before C6, $VIBE_TOKEN in
	// fleetd's environment silently overrode every per-cell token_file,
	// making the whole cells.X.token_file config dead. The precedence is
	// fixed; the configuration is still worth naming, because nothing else
	// tells an operator which credential a long-lived fleetd is using.
	var withFile, withoutFile []string
	if s.hosts != nil {
		for name, c := range s.hosts.Cells {
			if c.DaemonURL == "" {
				continue
			}
			if c.TokenFile != "" {
				withFile = append(withFile, name)
			} else {
				withoutFile = append(withoutFile, name)
			}
		}
	}
	sort.Strings(withFile)
	sort.Strings(withoutFile)
	switch {
	case host.EnvTokenSet && len(withFile) > 0:
		rep.Add(DoctorCheck{ID: "auth.credential", Level: LevelWarn,
			Summary: "$VIBE_TOKEN is set in fleetd's environment while cells declare per-cell token_files",
			Detail: fmt.Sprintf("per-cell file wins here since C6 (%s), and the env var is the shared credential for every "+
				"cell without one (%s). Before C6 the env var won everywhere, which made every token_file dead config.",
				strings.Join(withFile, ", "), orNone(withoutFile)),
			Fix: "unset VIBE_TOKEN in fleetd's unit/compose environment, or give every cell a token_file."})
	case host.EnvTokenSet:
		rep.Add(DoctorCheck{ID: "auth.credential", Level: LevelOK,
			Summary: "every cell's outbound credential is the single $VIBE_TOKEN in fleetd's environment",
			Detail:  "no cell declares a token_file. One credential for the fleet is a choice, not a fault (design §6)."})
	default:
		rep.Add(DoctorCheck{ID: "auth.credential", Level: LevelOK,
			Summary: "no $VIBE_TOKEN in fleetd's environment; per-cell token_files and the local token decide"})
	}
}

// checkInbound reads the announce AS the credential test: POST
// /api/fleet/announce is bearer-authed like every other TCP route, so a
// cell whose token is wrong never reaches the presence table at all.
// Zero new plumbing — the heartbeat already proves it every 15s.
func (s *Server) checkInbound(rep *DoctorReport, c CellSnapshot, p Presence, rejected int64) {
	id := "auth.inbound"
	switch {
	case p.Announcing && !p.Stale && !p.Withdrawn:
		rep.Add(DoctorCheck{ID: id, Cell: c.Name, Level: LevelOK,
			Summary: fmt.Sprintf("authenticated announce %s ago (seq %d)", ago(p.ReceivedAt), p.Seq)})
	case p.Announcing && p.Withdrawn:
		rep.Add(DoctorCheck{ID: id, Cell: c.Name, Level: LevelOK,
			Summary: "withdrew cleanly " + ago(p.ReceivedAt) + " ago (the goodbye announce authenticated)"})
	case p.Announcing:
		rep.Add(DoctorCheck{ID: id, Cell: c.Name, Level: LevelWarn,
			Summary: "announces have stopped (last one " + ago(p.ReceivedAt) + " ago)",
			Detail: "this cell authenticated before, so the credential was right then. The box may be away, the announcer " +
				"may have died, or fleetd's token may have rotated under it.",
			Fix: "see roaming.announcer for this cell if it has a host_probe."})
	case c.Name == fleetcfg.FrontCell:
		// The reference front is a llama-swap container with no announcer;
		// fleetd reaches it by probe and by the render mount, so there is
		// no inbound credential for it to hold.
		rep.Add(DoctorCheck{ID: id, Cell: c.Name, Level: LevelOK,
			Summary: "the front does not announce in the reference deployment",
			Detail:  "fleetd reaches it by probe and through the render mount; it holds no inbound credential."})
	default:
		det := "this cell may not run an announcer at all — availability then comes from probes (C1 semantics), and no " +
			"credential of its is ever exercised inbound."
		if rejected > 0 {
			det += fmt.Sprintf(" NOTE: fleetd has refused %d request(s) and cannot attribute them to a cell; some of them "+
				"may be this cell's announcer holding a stale token.", rejected)
		}
		rep.Add(DoctorCheck{ID: id, Cell: c.Name, Level: LevelUnknown,
			Summary: "never announced", Detail: det})
	}
}

func (s *Server) checkOutbound(rep *DoctorReport, c CellSnapshot, p Presence, res CellAuthResult) {
	id := "auth.outbound"
	if s.cellAuth == nil {
		rep.Add(DoctorCheck{ID: id, Cell: c.Name, Level: LevelUnknown,
			Summary: "no outbound credential prober wired",
			Detail:  "this fleetd was built without one, so fleetd→cell auth is unchecked."})
		return
	}
	fresh := p.Announcing && !p.Stale && !p.Withdrawn
	switch {
	case res.CredentialErr != "":
		rep.Add(DoctorCheck{ID: id, Cell: c.Name, Level: LevelFail,
			Summary: "fleetd cannot resolve a credential for this cell",
			Detail:  res.CredentialErr,
			Fix:     "every fleetd-driven verb for this cell fails before it leaves the box."})
	case !res.Attempted && fresh:
		// After C3 daemon_url is an optimization: a cell reachable only by
		// its own announces is correctly configured, and everything
		// configured works. Scoring a deliberate configuration UNKNOWN
		// forever teaches an operator to ignore the level that exists to
		// be noticed.
		rep.Add(DoctorCheck{ID: id, Cell: c.Name, Level: LevelOK,
			Summary: "no daemon_url; the announce piggyback queue is this cell's actuation channel (C3)",
			Detail:  "nothing to authenticate outbound, and inbound is proven fresh."})
	case !res.Attempted && c.Name == fleetcfg.FrontCell:
		// Same rule as auth.inbound's front row: in the reference
		// deployment the front is a llama-swap container with no vibe
		// daemon and no announcer. fleetd reaches it by probe and through
		// the render mount, so there is no credential either way — and a
		// permanent UNKNOWN on a correctly-configured fleet is how an
		// operator learns to ignore the level that exists to be noticed.
		rep.Add(DoctorCheck{ID: id, Cell: c.Name, Level: LevelOK,
			Summary: "the front runs no cell daemon in the reference deployment",
			Detail:  "no outbound credential to hold; fleetd drives the front by rendering its config."})
	case !res.Attempted:
		rep.Add(DoctorCheck{ID: id, Cell: c.Name, Level: LevelUnknown,
			Summary: "no daemon_url and no fresh announce",
			Detail:  "neither direction has a proven credential path for this cell right now.",
			Fix:     "either commission its announcer or give it a daemon_url + token_file."})
	case res.OK:
		rep.Add(DoctorCheck{ID: id, Cell: c.Name, Level: LevelOK,
			Summary: "credential accepted by the cell daemon",
			Detail:  "source: " + res.Source})
	case res.Denied:
		rep.Add(DoctorCheck{ID: id, Cell: c.Name, Level: LevelFail,
			Summary: "the cell daemon REFUSED fleetd's credential",
			Detail:  "source: " + res.Source + ". " + res.Err,
			Fix:     "drain/resume/unload for this cell will 401 until the two sides carry the same token."})
	default:
		rep.Add(DoctorCheck{ID: id, Cell: c.Name, Level: LevelUnknown,
			Summary: "could not reach the cell daemon",
			Detail: "source: " + res.Source + ". " + res.Err +
				" — a wrong credential and a box that is off are indistinguishable from here."})
	}
}

// ─── versions and defs ──────────────────────────────────────────────────────

func (s *Server) checkVersions(rep *DoctorReport, snap StateSnapshot, pres map[string]Presence, host DoctorHost) {
	// Assert the evidence EXISTS before asserting parity over it —
	// otherwise "every cell agrees" is what a fleet of silent cells looks
	// like.
	for _, c := range snap.Cells {
		p := pres[c.Name]
		switch {
		case !p.Announcing && c.Name == fleetcfg.FrontCell:
			// The front's llama-swap config is fleetd's own render, so it
			// has no def checkout to be in parity with, and the reference
			// deployment gives it no announcer to report one from.
			continue
		case !p.Announcing:
			rep.Add(DoctorCheck{ID: "versions.reported", Cell: c.Name, Level: LevelUnknown,
				Summary: "no announce, so no versions block"})
		case p.Versions == nil || (p.Versions.Vibe == "" && p.Versions.DefsSHA == "" && p.Versions.LlamaSwap == ""):
			rep.Add(DoctorCheck{ID: "versions.reported", Cell: c.Name, Level: LevelUnknown,
				Summary: "announces carry no versions block",
				Detail: "def-SHA parity and the version matrix cannot include this cell. `vibe fleet announce` sent no " +
					"versions block before C13.",
				Fix: "upgrade this cell's vibe build."})
		default:
			det := "vibe " + orDash(p.Versions.Vibe) + ", defs " + orDash(p.Versions.DefsSHA)
			if p.Versions.DefsDirty {
				det += " (DIRTY)"
			}
			rep.Add(DoctorCheck{ID: "versions.reported", Cell: c.Name, Level: LevelOK,
				Summary: "versions block present", Detail: det})
		}
	}

	// fleetd's own build and def checkout. Collected and then never
	// reported in the first cut — which is exactly the asymmetry the
	// parity check is about: the box writing the front's render is part
	// of the fleet's version story.
	if host.Version != "" || host.DefsSHA != "" {
		det := "vibe " + orDash(host.Version) + ", defs " + orDash(host.DefsSHA)
		if host.DefsDirty {
			det += " (DIRTY)"
		}
		rep.Add(DoctorCheck{ID: "versions.reported", Level: LevelOK,
			Summary: "fleetd's own build", Detail: det})
	}

	// def-SHA parity.
	clean := map[string][]string{} // sha -> cells
	var dirty, absent []string
	for _, c := range snap.Cells {
		p := pres[c.Name]
		if p.Versions == nil || p.Versions.DefsSHA == "" {
			if p.Announcing {
				absent = append(absent, c.Name)
			}
			continue
		}
		if p.Versions.DefsDirty {
			dirty = append(dirty, c.Name+" ("+p.Versions.DefsSHA+")")
			continue
		}
		clean[p.Versions.DefsSHA] = append(clean[p.Versions.DefsSHA], c.Name)
	}
	sort.Strings(dirty)
	sort.Strings(absent)
	dirtyNote := ""
	if len(dirty) > 0 {
		// A dirty checkout's SHA does not describe what is running, so it
		// can neither agree nor disagree. Counting it as agreement because
		// the string matches is the absent-evidence mistake exactly.
		dirtyNote = " Uncomparable (working tree dirty): " + strings.Join(dirty, ", ") + "."
	}
	switch {
	case len(clean) == 0:
		rep.Add(DoctorCheck{ID: "defs.parity", Level: LevelUnknown,
			Summary: "no cell reports a clean defs SHA",
			Detail:  "nothing to compare." + dirtyNote})
	case len(clean) > 1:
		rep.Add(DoctorCheck{ID: "defs.parity", Level: LevelWarn,
			Summary: "cells disagree about the def checkout",
			Detail:  shaGroups(clean) + dirtyNote,
			Fix:     "converge the backend-def checkouts; a fingerprint mismatch downstream is this, one level up."})
	default:
		var sha string
		for k := range clean {
			sha = k
		}
		det := "every reporting cell is at " + sha + "."
		lvl := LevelOK
		if host.DefsSHA != "" && host.DefsSHA != sha && !host.DefsDirty {
			lvl = LevelWarn
			det += fmt.Sprintf(" fleetd's own def checkout is at %s — the render it writes comes from a different tree than the cells serve.", host.DefsSHA)
		}
		if len(absent) > 0 {
			det += " Not reporting: " + strings.Join(absent, ", ") + "."
		}
		rep.Add(DoctorCheck{ID: "defs.parity", Level: lvl, Summary: "def checkouts agree", Detail: det + dirtyNote})
	}

	// The llama-swap version matrix (futures item 13's canary → gate →
	// fleet sequence is exactly the mid-state this catches).
	vers := map[string][]string{}
	for _, c := range snap.Cells {
		p := pres[c.Name]
		if p.Versions != nil && p.Versions.LlamaSwap != "" {
			vers[p.Versions.LlamaSwap] = append(vers[p.Versions.LlamaSwap], c.Name)
		}
	}
	switch {
	case len(vers) == 0:
		// NOT a silent OK. The field is a C3 reservation that no announcer
		// has ever filled; saying so is what keeps the next agent from
		// reading this UNKNOWN as a bug in doctor.
		rep.Add(DoctorCheck{ID: "versions.llama_swap", Level: LevelUnknown,
			Summary: "no cell reports a llama-swap version",
			Detail: "versions.llama_swap is a reserved announce field and no announcer populates it yet, so the version " +
				"matrix cannot be built. This is a missing producer, not an unreachable fleet."})
	case len(vers) > 1:
		rep.Add(DoctorCheck{ID: "versions.llama_swap", Level: LevelWarn,
			Summary: "cells run different llama-swap versions",
			Detail:  shaGroups(vers),
			Fix:     "the upgrade ritual is canary cell → SSE gate → fleet; this is the mid-state."})
	default:
		var v string
		for k := range vers {
			v = k
		}
		rep.Add(DoctorCheck{ID: "versions.llama_swap", Level: LevelOK,
			Summary: "every reporting cell runs llama-swap " + v})
	}
}

func shaGroups(m map[string][]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		cells := append([]string(nil), m[k]...)
		sort.Strings(cells)
		parts = append(parts, k+": "+strings.Join(cells, ", "))
	}
	return strings.Join(parts, " · ")
}

// ─── disk ───────────────────────────────────────────────────────────────────

// checkDisk reports three DIFFERENT filesystems separately, each named.
// "Disk free on the front" is not fleetd's disk: fleetd is its own
// container and may not share a host with the front at all.
func (s *Server) checkDisk(rep *DoctorReport, pres map[string]Presence, host DoctorHost) {
	// The render mount is the one place fleetd may speak for the front:
	// fleet.front_config being set IS the declaration that the two share
	// a filesystem (C3's render mount contract).
	if host.FrontConfig != "" {
		dir := dirOf(host.FrontConfig)
		lvl, msg := diskVerdict(host, dir, fleetdDiskWarn, fleetdDiskFail)
		rep.Add(DoctorCheck{ID: "disk.free", Cell: fleetcfg.FrontCell, Level: lvl,
			Summary: "front render mount " + dir + ": " + msg,
			Detail:  "the filesystem fleetd writes the front's rendered llama-swap config to."})
	}
	for _, c := range s.cells {
		p := pres[c.Name]
		if !p.Announcing {
			continue // auth.inbound already reports the silence
		}
		if p.Capacity == nil || p.Capacity.DiskFreeGB == 0 {
			rep.Add(DoctorCheck{ID: "disk.free", Cell: c.Name, Level: LevelUnknown,
				Summary: "announces carry no disk figure",
				Detail:  "the cell's capacity block is absent or reports nothing."})
			continue
		}
		lvl := LevelOK
		if p.Capacity.DiskFreeGB < cellDiskWarnGB {
			lvl = LevelWarn
		}
		rep.Add(DoctorCheck{ID: "disk.free", Cell: c.Name, Level: lvl,
			Summary: fmt.Sprintf("%.1f GiB free (announced)", p.Capacity.DiskFreeGB),
			Detail:  "the cell filesystem its weights live on."})
	}
}

func dirOf(path string) string {
	if i := strings.LastIndex(path, "/"); i > 0 {
		return path[:i]
	}
	return path
}

// ─── TLS ────────────────────────────────────────────────────────────────────

// tlsExpiryWarn is how close to notAfter earns a WARN. Three weeks is
// two chances to notice at a weekly sit-down.
const tlsExpiryWarn = 21 * 24 * time.Hour

// tlsDialTimeout bounds one endpoint's handshake.
const tlsDialTimeout = 3 * time.Second

// checkTLS reads the leaf certificate's notAfter on every https fleet
// URL. It deliberately does NOT verify the chain, which is why the check
// is named not_after: a LAN fleet's certs are self-signed or issued by a
// household CA fleetd's container may not carry, and failing those would
// paint a permanent red that means nothing. The message says the chain
// was not checked so an OK cannot be misread as "trusted".
func (s *Server) checkTLS(ctx context.Context, rep *DoctorReport) {
	type ep struct{ cell, url string }
	var eps []ep
	if s.hosts != nil {
		if isHTTPS(s.hosts.FleetdURL) {
			eps = append(eps, ep{"fleetd_url", s.hosts.FleetdURL})
		}
		names := make([]string, 0, len(s.hosts.Cells))
		for n := range s.hosts.Cells {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			c := s.hosts.Cells[n]
			if isHTTPS(c.URL) {
				eps = append(eps, ep{n, c.URL})
			}
			if isHTTPS(c.DaemonURL) {
				eps = append(eps, ep{n, c.DaemonURL})
			}
		}
	}
	if len(eps) == 0 {
		// A definitive negative, not an absence of evidence: there are no
		// certificates, so none can expire.
		rep.Add(DoctorCheck{ID: "tls.not_after", Level: LevelOK,
			Summary: "no TLS endpoints configured",
			Detail:  "every fleet URL is plain HTTP on the LAN, so there is no certificate to expire."})
		return
	}
	for _, e := range eps {
		lvl, sum, det := certNotAfter(ctx, e.url)
		rep.Add(DoctorCheck{ID: "tls.not_after", Cell: e.cell, Level: lvl, Summary: sum, Detail: det})
	}
}

func isHTTPS(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && u.Scheme == "https" && u.Host != ""
}

func certNotAfter(ctx context.Context, raw string) (Level, string, string) {
	u, err := url.Parse(raw)
	if err != nil {
		return LevelUnknown, "unparseable URL: " + raw, ""
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "443")
	}
	// InsecureSkipVerify by design: this check reads an expiry, it does
	// not judge a chain (see the doc comment on checkTLS). DialContext,
	// not DialWithDialer: the latter ignores the report's context
	// entirely, so a cancelled request left N dials running.
	d := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: tlsDialTimeout},
		Config:    &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // reads notAfter only; never used to carry data
	}
	dctx, cancel := context.WithTimeout(ctx, tlsDialTimeout)
	defer cancel()
	conn, err := d.DialContext(dctx, "tcp", host)
	if err != nil {
		return LevelUnknown, host + ": TLS dial failed", err.Error() +
			" — a host that is off and a broken TLS listener are indistinguishable from here."
	}
	defer conn.Close()
	tc, ok := conn.(*tls.Conn)
	if !ok {
		return LevelUnknown, host + ": not a TLS connection", ""
	}
	certs := tc.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return LevelUnknown, host + ": no certificate presented", ""
	}
	na := certs[0].NotAfter
	left := time.Until(na)
	det := "leaf expires " + na.UTC().Format(time.RFC3339) + ". The chain is NOT verified here — this check reads an expiry, not trust."
	switch {
	case left <= 0:
		return LevelFail, host + ": certificate EXPIRED", det
	case left < tlsExpiryWarn:
		return LevelWarn, fmt.Sprintf("%s: certificate expires in %d day(s)", host, int(left.Hours()/24)), det
	default:
		return LevelOK, fmt.Sprintf("%s: certificate valid for %d more day(s)", host, int(left.Hours()/24)), det
	}
}

// ─── wake and the roaming announcer ─────────────────────────────────────────

func (s *Server) checkWakeAndRoaming(rep *DoctorReport, snap StateSnapshot, pres map[string]Presence) {
	for _, c := range snap.Cells {
		if c.Name == fleetcfg.FrontCell {
			continue
		}
		var cell Cell
		for _, rc := range s.cells {
			if rc.Name == c.Name {
				cell = rc
			}
		}
		// wake.configured, not wake.armed: the control plane can see that a
		// MAC or a fallback command is declared. Whether the NIC is armed
		// is not observable from here, and SENDING a packet to find out is
		// a mutation this command does not make — which is exactly why the
		// futures entry pairs doctor with a quarterly fire drill.
		switch {
		case cell.Wake != nil:
			how := "cmd"
			if cell.Wake.MAC != "" {
				how = "MAC " + cell.Wake.MAC
			}
			rep.Add(DoctorCheck{ID: "wake.configured", Cell: c.Name, Level: LevelOK,
				Summary: "wake configured (" + how + ")",
				Detail: "whether the NIC is actually armed is not observable from the control plane; the quarterly fire " +
					"drill's one real wake is that test."})
		case c.Reachable || c.Class == string(fleetcfg.ClassRoaming):
			rep.Add(DoctorCheck{ID: "wake.configured", Cell: c.Name, Level: LevelOK,
				Summary: "no wake configured", Detail: "nothing to wake right now (cell is up, or is a roaming box that left the building)."})
		default:
			rep.Add(DoctorCheck{ID: "wake.configured", Cell: c.Name, Level: LevelWarn,
				Summary: "cell is absent and has no wake path",
				Detail:  "nothing can bring this box back without walking to it.",
				Fix:     "add cells." + c.Name + ".wake (mac, or cmd when fleetd cannot reach L2 broadcast)."})
		}

		s.checkAnnouncerAgent(rep, c, pres[c.Name], snap.Daemon.AuthRejected)
	}
}

// checkAnnouncerAgent is the one genuine DERIVATION in the report: a
// host_probe that answers plus a heartbeat that does not means the
// announce agent is the broken part — which otherwise hides behind
// OFF/AWAY? for a week, because "laptop on a commute" and "launchd
// forgot the plist" look identical in every other surface.
func (s *Server) checkAnnouncerAgent(rep *DoctorReport, c CellSnapshot, p Presence, rejected int64) {
	id := "roaming.announcer"
	roaming := c.Class == string(fleetcfg.ClassRoaming)
	if c.HostReachable == nil {
		if roaming {
			rep.Add(DoctorCheck{ID: id, Cell: c.Name, Level: LevelUnknown,
				Summary: "no host_probe, so 'box away' and 'announcer stopped' are the same observation",
				Fix:     "set cells." + c.Name + ".host_probe to make this answerable."})
		}
		return
	}
	fresh := p.Announcing && !p.Stale && !p.Withdrawn
	switch {
	case *c.HostReachable && fresh:
		rep.Add(DoctorCheck{ID: id, Cell: c.Name, Level: LevelOK,
			Summary: "announcer running (heartbeat " + ago(p.ReceivedAt) + " ago)"})
	case *c.HostReachable && p.Withdrawn:
		rep.Add(DoctorCheck{ID: id, Cell: c.Name, Level: LevelOK,
			Summary: "announcer withdrew cleanly and the box is still up",
			Detail:  "a declared goodbye, not a failure."})
	case *c.HostReachable:
		det := "fleetd just reached this box at L4, so the announce agent is not loaded, not running, or is being " +
			"rejected."
		if rejected > 0 {
			det += fmt.Sprintf(" auth.rejections is %d and climbing, which makes the rejected explanation the likely one.", rejected)
		}
		rep.Add(DoctorCheck{ID: id, Cell: c.Name, Level: LevelWarn,
			Summary: "the box answers but is not announcing",
			Detail:  det,
			Fix:     "check the cell's announce unit/agent, then its token file."})
	default:
		rep.Add(DoctorCheck{ID: id, Cell: c.Name, Level: LevelUnknown,
			Summary: "box is away; whether its announcer would load is unknowable from here"})
	}
}

// ─── hygiene: the findings a fortnight produces ─────────────────────────────

// staleRequestAge is how long a registry intent request may sit unacked
// before it is worth a human's attention. Two announce intervals plus
// slack — the same shape as staleAfter, one order coarser.
const staleRequestAge = 2 * time.Minute

// longLeaseRemaining flags a lease whose TTL outlives any plausible
// batch. C11 caps holds at 24h precisely because a declaration that
// suppresses policy must expire; a plain lease has no such cap.
const longLeaseRemaining = 24 * time.Hour

// awayTooLong is when a declared-away fleet stops reading as a trip.
const awayTooLong = 7 * 24 * time.Hour

func (s *Server) checkHygiene(rep *DoctorReport, snap StateSnapshot) {
	s.checkIntentHygiene(rep, snap)
	s.checkLeases(rep)
	s.checkFingerprints(rep)
	s.checkProbes(rep, snap)
	s.checkWarm(rep, snap)
	s.checkUsage(rep, snap)
	s.checkNotify(rep, snap)
	s.checkRenderMount(rep)
}

func (s *Server) checkIntentHygiene(rep *DoctorReport, snap StateSnapshot) {
	var pending, residue []string
	for _, c := range snap.Cells {
		if c.IntentPending {
			age := time.Duration(0)
			if c.Intent != nil {
				age = time.Since(c.Intent.Since)
			}
			if age > staleRequestAge || c.Intent == nil {
				pending = append(pending, c.Name)
			}
		}
		switch c.Display {
		case DisplayDrainedQ, DisplayInconsistent:
			residue = append(residue, c.Name+" "+c.Display)
		}
	}
	if len(pending) == 0 && len(residue) == 0 {
		rep.Add(DoctorCheck{ID: "intent.hygiene", Level: LevelOK,
			Summary: "no stuck intent requests and no undeclared stops"})
		return
	}
	var det []string
	if len(pending) > 0 {
		det = append(det, "requested but never echoed: "+strings.Join(pending, ", "))
	}
	if len(residue) > 0 {
		det = append(det, "undeclared state: "+strings.Join(residue, ", ")+
			" (reported only — an inferred intent is never acted on, invariant 2)")
	}
	rep.Add(DoctorCheck{ID: "intent.hygiene", Level: LevelWarn,
		Summary: "the intent axis has residue",
		Detail:  strings.Join(det, "; "),
		Fix:     "`vibe cell resume <cell>` clears a stale drain; a stuck request means the cell is not reconciling."})
}

func (s *Server) checkLeases(rep *DoctorReport) {
	if s.leasePath == "" {
		rep.Add(DoctorCheck{ID: "leases.age", Level: LevelOK, Summary: "no lease store on this daemon"})
		return
	}
	s.mu.Lock()
	active := s.activeLeasesLocked()
	s.mu.Unlock()
	var long, holds []string
	for _, l := range active {
		if l.Hold {
			holds = append(holds, fmt.Sprintf("%s/%s held by %s, %s", l.Cell, l.Model, l.Holder, HoldLeft(l.ExpiresAt)))
			continue
		}
		if time.Until(l.ExpiresAt) > longLeaseRemaining {
			long = append(long, fmt.Sprintf("%s/%s by %q, %s left", l.Cell, l.Model, l.Holder, HoldLeft(l.ExpiresAt)))
		}
	}
	sort.Strings(long)
	sort.Strings(holds)
	det := ""
	if len(holds) > 0 {
		det = "active holds: " + strings.Join(holds, "; ") + "."
	}
	if len(long) == 0 {
		rep.Add(DoctorCheck{ID: "leases.age", Level: LevelOK,
			Summary: fmt.Sprintf("%d active lease(s), none outliving a day", len(active)), Detail: det})
		return
	}
	rep.Add(DoctorCheck{ID: "leases.age", Level: LevelWarn,
		Summary: "a lease will outlive the day",
		Detail: strings.Join(long, "; ") + ". A lease suppresses that cell's scheduled warms and its probes for as long " +
			"as it lives. " + det,
		Fix: "DELETE /api/fleet/lease when the work that took it is done."})
}

func (s *Server) checkFingerprints(rep *DoctorReport) {
	if !s.renderLoopRunning() {
		// C9's rule verbatim: without a render pass the mismatch set is
		// permanently empty, and a silent zero would read as "no drift".
		rep.Add(DoctorCheck{ID: "fingerprint.drift", Level: LevelUnknown,
			Summary: "no render loop, so nothing verifies flag fingerprints",
			Detail:  "fleet.front_config is unset; the mismatch set stays empty whether or not a cell has drifted.",
			Fix:     "set fleet.front_config on fleetd (the render mount contract)."})
		return
	}
	fps := s.FingerprintMismatches()
	if len(fps) == 0 {
		rep.Add(DoctorCheck{ID: "fingerprint.drift", Level: LevelOK,
			Summary: "every announced model matches the def it renders from"})
		return
	}
	parts := make([]string, 0, len(fps))
	for _, f := range fps {
		parts = append(parts, fmt.Sprintf("%s/%s (%s, first seen %s ago)", f.Cell, f.Model, f.Mode, ago(f.FirstSeen)))
	}
	rep.Add(DoctorCheck{ID: "fingerprint.drift", Level: LevelWarn,
		Summary: fmt.Sprintf("%d model(s) serving flags the def does not describe", len(fps)),
		Detail:  strings.Join(parts, "; "),
		Fix:     "see defs.parity — a mismatch is usually a def checkout one level up."})
}

func (s *Server) checkProbes(rep *DoctorReport, snap StateSnapshot) {
	measured := false
	for _, c := range snap.Cells {
		for _, m := range c.Models {
			if m.Probe != nil && m.Probe.Verdict != "" && m.Probe.Verdict != VerdictUnknown {
				measured = true
			}
		}
	}
	var skipped []string
	if snap.Probe != nil {
		for _, t := range snap.Probe.Targets {
			if t.State == "skipped" {
				skipped = append(skipped, fmt.Sprintf("%s/%s: %s", t.Cell, t.Model, orDash(t.LastSkip)))
			}
		}
	}
	switch {
	case snap.Probe != nil && len(snap.Probe.Degraded) > 0:
		rep.Add(DoctorCheck{ID: "probe.verdicts", Level: LevelWarn,
			Summary: "degraded: " + strings.Join(snap.Probe.Degraded, ", "),
			Detail:  "a model measurably slower than its own baseline. Nothing acts on this verdict; the runbook is human.",
			Fix:     "probe_model → unload_model → probe_model."})
	case len(skipped) > 0:
		rep.Add(DoctorCheck{ID: "probe.verdicts", Level: LevelWarn,
			Summary: "probe targets are being skipped",
			Detail:  strings.Join(skipped, "; ")})
	case measured:
		rep.Add(DoctorCheck{ID: "probe.verdicts", Level: LevelOK,
			Summary: "no model reports degraded throughput"})
	default:
		// Absence of a degraded verdict is not evidence of health when
		// nothing has measured. Friction pain 2 is that llama-server can
		// rot 10-100x while /health stays green.
		rep.Add(DoctorCheck{ID: "probe.verdicts", Level: LevelUnknown,
			Summary: "nothing measures throughput on this fleet",
			Detail:  "no cell has produced a probe verdict, so 'nothing is slow' is unproven.",
			Fix:     "declare probe_targets in fleetd's config, or run probe_model by hand."})
	}
}

func (s *Server) checkWarm(rep *DoctorReport, snap StateSnapshot) {
	if snap.Warm == nil {
		rep.Add(DoctorCheck{ID: "warm.policy", Level: LevelOK,
			Summary: "no warm targets or schedules configured",
			Detail:  "nothing is declared, so nothing should happen."})
		return
	}
	var bad []string
	for _, t := range snap.Warm.Targets {
		if t.State == "skipped" {
			bad = append(bad, fmt.Sprintf("target %s/%s skipped: %s", t.Cell, t.Model, orDash(t.Detail)))
		}
	}
	for _, sc := range snap.Warm.Schedule {
		if sc.NextFire == nil {
			bad = append(bad, fmt.Sprintf("schedule %q (%s) has no resolved next fire", sc.Cron, sc.Model))
		}
	}
	det := scheduleSummary(snap.Warm.Schedule)
	if len(bad) == 0 {
		rep.Add(DoctorCheck{ID: "warm.policy", Level: LevelOK,
			Summary: fmt.Sprintf("%d warm target(s), %d schedule(s), none skipping", len(snap.Warm.Targets), len(snap.Warm.Schedule)),
			Detail:  det})
		return
	}
	rep.Add(DoctorCheck{ID: "warm.policy", Level: LevelWarn,
		Summary: "the warm policy is not doing what it was declared to do",
		Detail:  strings.Join(bad, "; ") + ". " + det})
}

// scheduleSummary always prints the resolved next fire WITH its zone: a
// wrong TZ in a container that defaults to UTC is the failure C4 built
// the field to make visible, and it is only visible if someone reads it.
func scheduleSummary(entries []warmScheduleState) string {
	if len(entries) == 0 {
		return ""
	}
	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		next := "unresolved"
		if e.NextFire != nil {
			next = e.NextFire.Format("2006-01-02 15:04 MST")
		}
		parts = append(parts, fmt.Sprintf("%q %s → %s", e.Cron, e.Model, next))
	}
	return "next fires: " + strings.Join(parts, "; ")
}

// usageWindowDays is how far back the ledger-flow check looks. Long
// enough that a quiet weekend is not a finding.
const usageWindowDays = 7

func (s *Server) checkUsage(rep *DoctorReport, snap StateSnapshot) {
	if s.usage == nil {
		rep.Add(DoctorCheck{ID: "usage.flow", Level: LevelOK,
			Summary: "usage ledger disabled on this daemon"})
		return
	}
	since := time.Now().In(s.UsageTZ()).AddDate(0, 0, -usageWindowDays).Format("2006-01-02")
	report := s.UsageReport(since)
	fed := map[string]bool{}
	var lost int64
	for _, b := range report.Buckets {
		if b.Basis != usageBasisResident && b.Basis != usageBasisCell && b.Basis != usageBasisCloud {
			fed[b.Cell] = true
		}
		lost += b.LostRows
	}
	var silent []string
	for _, c := range snap.Cells {
		if c.Name == fleetcfg.FrontCell {
			continue // C7a skips the front structurally; it folds no token rows
		}
		p := s.presenceFor(c.Name)
		if p == nil || !p.Announcing || p.Stale {
			continue
		}
		if !fed[c.Name] {
			silent = append(silent, c.Name)
		}
	}
	sort.Strings(silent)
	switch {
	case len(silent) > 0:
		rep.Add(DoctorCheck{ID: "usage.flow", Level: LevelWarn,
			Summary: "announcing cells contributed no tokens in " + fmt.Sprint(usageWindowDays) + " days: " + strings.Join(silent, ", "),
			Detail: "either the cell genuinely served nothing, or its llama-swap has no `store: {path: …}` in its extras — " +
				"without it the activity log is a 1000-row in-memory ring and the meter reads ~nothing.",
			Fix: "check that cell's llama-swap store config."})
	case lost > 0:
		rep.Add(DoctorCheck{ID: "usage.flow", Level: LevelWarn,
			Summary: fmt.Sprintf("%d activity row(s) aged out before the meter read them", lost),
			Detail:  "reported rather than absorbed: silent loss is indistinguishable from an idle cell."})
	default:
		rep.Add(DoctorCheck{ID: "usage.flow", Level: LevelOK,
			Summary: "every announcing cell is feeding the ledger"})
	}
}

func (s *Server) checkNotify(rep *DoctorReport, snap StateSnapshot) {
	n := snap.Notify
	if n == nil || !n.Configured {
		rep.Add(DoctorCheck{ID: "notify.reach", Level: LevelOK,
			Summary: "no webhook configured",
			Detail:  "alarms are visible in fleet_status and reach no device. That is the status quo, not a regression."})
		return
	}
	if n.Scope == ScopeAway {
		lvl, sum := LevelOK, "alarm delivery is suppressed: the fleet is declared AWAY"
		if n.ScopeSince != nil {
			sum += " (declared " + ago(*n.ScopeSince) + " ago)"
			// A declaration explains an absence; a declaration nobody has
			// revisited in a week explains a silent pager.
			if time.Since(*n.ScopeSince) > awayTooLong {
				lvl = LevelWarn
			}
		}
		rep.Add(DoctorCheck{ID: "notify.reach", Level: lvl, Summary: sum + reasonSuffix(n.ScopeReason),
			Detail: fmt.Sprintf("%d notification(s) suppressed so far; coming home sends one digest.", n.Suppressed),
			Fix:    "`vibe fleet notify scope home` when you are back."})
		return
	}
	rep.Add(DoctorCheck{ID: "notify.reach", Level: LevelOK,
		Summary: "alarms deliver to " + n.Endpoint,
		Detail:  "enabled: " + strings.Join(n.Enabled, ", ")})
}

func reasonSuffix(reason string) string {
	if reason == "" {
		return ""
	}
	return " — " + reason
}

// checkRenderMount reports the consequence of an unset fleet.front_config
// explicitly, because its absence is otherwise invisible: no render
// passes means the catalog is not presence-derived, fingerprints are
// never enforced, and C9's fingerprint alarm has no evaluator.
func (s *Server) checkRenderMount(rep *DoctorReport) {
	if !s.renderLoopRunning() {
		rep.Add(DoctorCheck{ID: "front.render_mount", Level: LevelWarn,
			Summary: "fleetd does not render the front's config",
			Detail: "without fleet.front_config the catalog is not presence-derived, strict fingerprints are never " +
				"enforced, and the fingerprint alarm has no evaluator.",
			Fix: "mount the front's config dir into fleetd and set fleet.front_config."})
		return
	}
	rep.Add(DoctorCheck{ID: "front.render_mount", Level: LevelOK,
		Summary: fmt.Sprintf("render loop running (%d front config write(s) this process)", s.RenderCount())})
}

// ─── HTTP ───────────────────────────────────────────────────────────────────

// doctorTimeout bounds the whole report. It is generous because the
// report fans out to every cell twice (the state snapshot's probes and
// the outbound credential calls) plus one TLS dial per https endpoint,
// each individually bounded.
const doctorTimeout = 20 * time.Second

func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), doctorTimeout)
	defer cancel()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.Doctor(ctx))
}

// ─── small helpers ──────────────────────────────────────────────────────────

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func orNone(v []string) string {
	if len(v) == 0 {
		return "none"
	}
	return strings.Join(v, ", ")
}
