package fleetapi

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
)

// U1 — the two deadlines the fleet's state snapshot is built on, and the
// hazard hiding under the one that had no seam.
//
// Everything in this package's suite answered a probe INSTANTLY before
// this file: the whole vocabulary for "unreachable" was 127.0.0.1:1 (an
// immediate ECONNREFUSED) and a .invalid hostname (an immediate DNS
// failure). Both resolve in microseconds, so neither snapshotTimeout nor
// hostProbeTimeout was ever reached by a test, and the code path that
// runs when they ARE reached — the one that decides what an operator is
// told about a box fleetd could not finish looking at — was reachable
// only in production.
//
// The hazard, in one line: probeTCP returned a bare bool, so "fleetd's
// own snapshot budget expired" and "the box is off" left the function
// spelled identically, and the display ladder renders the second one as
// OFF/AWAY. It is this repo's most recurring defect class (absent
// evidence read as a healthy value) in the one file structurally
// invisible to the C20 scan that hunts it, because a single `bool`
// return does not match the (T, bool) shape the scan looks for.

// ─── primitives ─────────────────────────────────────────────────────────────

// tlsBlackhole accepts TCP and never speaks TLS, so every dial burns the
// whole dial timeout — what a powered-down box behind a DROP rule looks
// like, and the only shape that exposes a serial fan-out.
//
// Promoted here from c13_test.go: it is this package's only "far side
// that hangs" primitive, and the limit of what it can stall is itself
// load-bearing (see TestU1_AcceptAndHoldCannotStallConnect).
func tlsBlackhole(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		var held []net.Conn
		defer func() {
			for _, c := range held {
				c.Close()
			}
		}()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			held = append(held, c)
		}
	}()
	return ln.Addr().String()
}

// u1StallCell is a llama-swap stand-in whose /running and /v1/models
// ACCEPT the request and never answer it — the dead-but-listening cell
// snapshotTimeout exists for. The repo already had this idiom 21 times
// and every one of them was on an SSE route; none was on a snapshot
// probe, which is why the budget had no test.
func u1StallCell(t *testing.T) string {
	t.Helper()
	stall := func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /running", stall)
	mux.HandleFunc("GET /v1/models", stall)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// u1HangingDial is a connect that never completes: the dropped SYN of a
// filtered port or a box that lost power mid-conversation. It counts its
// attempts so a test can prove the probe was ATTEMPTED — "unfinished"
// about a dial that never happened would be its own invented
// observation.
func u1HangingDial(attempts *atomic.Int64) hostDialer {
	return func(ctx context.Context, addr string) (net.Conn, error) {
		attempts.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	}
}

// u1Server builds a Server with the persistence paths set and no
// watchers started: every assertion here is about one probe round.
func u1Server(t *testing.T, cells ...Cell) *Server {
	t.Helper()
	dir := t.TempDir()
	s := New(cells, filepath.Join(dir, "hist.json"), testDaemonInfo, Options{
		IntentPath:   filepath.Join(dir, "intent.json"),
		LastSeenPath: filepath.Join(dir, "last-seen.json"),
	})
	t.Cleanup(s.Close)
	return s
}

// u1Cell finds one cell in a snapshot by name.
func u1Cell(t *testing.T, snap StateSnapshot, name string) CellSnapshot {
	t.Helper()
	for _, c := range snap.Cells {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("cell %q not in snapshot", name)
	return CellSnapshot{}
}

// u1HostReachable renders the tri-state for a failure message.
func u1HostReachable(c CellSnapshot) string {
	if c.HostReachable == nil {
		if c.HostProbeUnfinished {
			return "unknown (probe unfinished)"
		}
		return "unknown (no probe configured)"
	}
	if *c.HostReachable {
		return "true"
	}
	return "false"
}

// ─── the snapshot budget ────────────────────────────────────────────────────

// TestU1_SnapshotDeadlinesAreWiredFromTheConstants is the seam's own
// guard. Making a constant injectable is how a production path quietly
// ends up with a zero timeout: New forgets the field, every test passes
// because every test sets it, and the fleet ships with no bound at all.
func TestU1_SnapshotDeadlinesAreWiredFromTheConstants(t *testing.T) {
	s := u1Server(t)
	if s.snapTimeout != snapshotTimeout {
		t.Errorf("New wired snapTimeout = %v, want the constant %v — production would run the round on whatever this is", s.snapTimeout, snapshotTimeout)
	}
	if s.hostDial != hostProbeTimeout {
		t.Errorf("New wired hostDial = %v, want the constant %v", s.hostDial, hostProbeTimeout)
	}
	if s.hostConnect == nil {
		t.Error("New left hostConnect nil; probeTCP's fallback covers it, but the production dialer should be named at construction like every other seam here")
	}
}

// TestU1_HostProbeTimeoutFitsInsideTheSnapshotBudget pins the claim
// hostProbeTimeout's own comment makes and nothing checked: the host
// dial must fit INSIDE the snapshot budget. Raise it past 3s and every
// cell with a filtered host_probe stops being a cell whose host did not
// answer and becomes a cell fleetd never finished looking at — including
// the other cells in the same round, whose rows were fine.
func TestU1_HostProbeTimeoutFitsInsideTheSnapshotBudget(t *testing.T) {
	if hostProbeTimeout >= snapshotTimeout {
		t.Fatalf("hostProbeTimeout (%v) does not fit inside snapshotTimeout (%v): a filtered port now expires the whole round, and every cell in it reports an unknown host",
			hostProbeTimeout, snapshotTimeout)
	}
}

// TestU1_OneStalledCellCannotOutlastTheSnapshotBudget is the invariant
// snapshotTimeout was added for, asserted for the first time: a cell that
// accepts the probe and never answers costs ONE budget, and the healthy
// cell beside it still renders.
func TestU1_OneStalledCellCannotOutlastTheSnapshotBudget(t *testing.T) {
	live := newFakeCell(t)
	live.modelsJSON = `{"object":"list","data":[{"id":"qwen3.6-27b","object":"model"}]}`
	s := u1Server(t,
		Cell{Name: "dead-cell", URL: u1StallCell(t), Class: "opportunistic"},
		Cell{Name: "live-cell", URL: live.srv.URL, Class: "always_on"},
	)
	s.snapTimeout = 400 * time.Millisecond

	start := time.Now()
	snap := s.Snapshot(context.Background())
	elapsed := time.Since(start)

	// Deliberately loose: this asserts the SHAPE (the round is bounded by
	// the server's budget) and not a benchmark. The real 3s constant
	// blows straight through it, which is the mutation this pins.
	if elapsed > 1500*time.Millisecond {
		t.Errorf("Snapshot took %v with snapTimeout=%v: the round is not bounded by the server's budget", elapsed, s.snapTimeout)
	}
	if dead := u1Cell(t, snap, "dead-cell"); dead.Reachable {
		t.Error("a cell that never answered is reported reachable")
	}
	got := u1Cell(t, snap, "live-cell")
	if !got.Reachable {
		t.Error("the healthy cell is unreachable: one dead cell poisoned the whole round, which is exactly what the budget exists to prevent")
	}
	if len(got.Models) != 1 || got.Models[0].ID != "qwen3.6-27b" {
		t.Errorf("live-cell models = %+v, want the catalog it served", got.Models)
	}
}

// TestU1_StalledCellsShareOneBudget: the fan-out is per-cell goroutines,
// so N dead cells cost one budget and not N. Serialise it and a fleet
// with four absent boxes turns a 3s ceiling into a 12s one, and the
// caller's own deadline starts cutting off cells that were never probed
// — a snapshot that INVENTS absence, which is C13's REV-1 finding one
// subsystem over.
func TestU1_StalledCellsShareOneBudget(t *testing.T) {
	const n = 8
	addr := u1StallCell(t)
	cells := make([]Cell, 0, n)
	for i := range n {
		cells = append(cells, Cell{Name: "dead-" + string(rune('a'+i)), URL: addr, Class: "opportunistic"})
	}
	s := u1Server(t, cells...)
	s.snapTimeout = 300 * time.Millisecond

	start := time.Now()
	snap := s.Snapshot(context.Background())
	elapsed := time.Since(start)

	if elapsed > 4*s.snapTimeout {
		t.Errorf("Snapshot took %v for %d stalled cells at %v each: the probes are serial", elapsed, n, s.snapTimeout)
	}
	for _, c := range snap.Cells {
		if c.Reachable {
			t.Errorf("%s: reachable, want not", c.Name)
		}
	}
}

// ─── the hazard: three failures that rendered as "the box is off" ───────────

// TestU1_ProbeTCPSeparatesRefusalFromAnExpiredBudget is the hazard at the
// function level, and needs no seam at all — which is the uncomfortable
// part: the distinction was one `if` away for the whole life of the
// file.
func TestU1_ProbeTCPSeparatesRefusalFromAnExpiredBudget(t *testing.T) {
	// A closed port on a host that is demonstrably up. This is an
	// OBSERVATION and stays one: false, with the known bit set.
	got := probeTCP(context.Background(), "127.0.0.1:1", 0, nil)
	if up, known := got.Observed(); !known || up {
		t.Errorf("refused port → %v, want an observed false: the host answered, the port is shut", got)
	}

	// The same address, reached with a budget that has already run out.
	// Nothing was learned about the host — the dial may not even have
	// been attempted — so the only honest answer is "unknown".
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	got = probeTCP(expired, "127.0.0.1:1", 0, nil)
	if got.IsKnown() {
		t.Errorf("expired budget → %v, want unknown. false here is how 'fleetd was too busy to finish its own snapshot' reaches the operator as 'your machine is switched off'", got)
	}

	// And the successful case still says true, so the unknown branch has
	// not swallowed the positive one.
	if up, known := probeTCP(context.Background(), tlsBlackhole(t), time.Second, nil).Observed(); !known || !up {
		t.Errorf("listening port → (%v, %v), want an observed true", up, known)
	}
}

// TestU1_AcceptAndHoldCannotStallConnect documents why the dial is
// injected rather than pointed at this package's existing hang
// primitive. tlsBlackhole stalls a HANDSHAKE; by the time it holds the
// connection, connect(2) has already returned success. A test that used
// it to exercise hostProbeTimeout would be asserting nothing — it is the
// fastest path through probeTCP, not the slowest.
func TestU1_AcceptAndHoldCannotStallConnect(t *testing.T) {
	addr := tlsBlackhole(t)
	start := time.Now()
	// Timeout 0 and dial nil: the production defaults, so this also pins
	// probeTCP's own defaulting. Drop the `timeout <= 0` fallback and
	// this dial gets an already-expired deadline and reports the box
	// absent — the same absent-evidence failure one layer down.
	got := probeTCP(context.Background(), addr, 0, nil)
	elapsed := time.Since(start)
	if up, known := got.Observed(); !known || !up {
		t.Fatalf("accept-and-hold → %v, want an observed true", got)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("connect to an accept-and-hold listener took %v; if this ever DOES stall, hostDialer's justification changes", elapsed)
	}
}

// TestU1_ExpiredSnapshotBudgetIsNotReportedAsAPoweredOffBox is the whole
// unit in one test: the same hang, seen through the state document the
// operator reads.
//
// Before the fix this rendered host_reachable:false → OFF/AWAY, which is
// the control plane telling a human the machine is switched off because
// fleetd ran out of time to ask. Now the value is ABSENT with a reason,
// and the ladder falls to OFF/AWAY? — a question, which is what an
// unfinished observation is.
func TestU1_ExpiredSnapshotBudgetIsNotReportedAsAPoweredOffBox(t *testing.T) {
	var attempts atomic.Int64
	s := u1Server(t, Cell{
		Name: "roamer", URL: "http://127.0.0.1:1", Class: "roaming",
		HostProbe: "10.255.255.1:22",
	})
	s.snapTimeout = 300 * time.Millisecond
	// Longer than the round: the SNAPSHOT's budget is what expires,
	// which is the case that used to be indistinguishable from "off".
	s.hostDial = 30 * time.Second
	s.hostConnect = u1HangingDial(&attempts)

	start := time.Now()
	snap := s.Snapshot(context.Background())
	elapsed := time.Since(start)

	c := u1Cell(t, snap, "roamer")
	if c.HostReachable != nil {
		t.Errorf("host_reachable = %s after the round's own budget expired. That false renders OFF/AWAY: fleetd would be telling the operator the box is powered off on the strength of its own busy-ness", u1HostReachable(c))
	}
	if !c.HostProbeUnfinished {
		t.Error("host_probe_unfinished is not set, so this unknown is indistinguishable from the no-host_probe-configured one — the two silences are different facts and doctor reads them differently")
	}
	if c.Display != DisplayOffAwayQ {
		t.Errorf("display = %s, want %s: an unfinished observation is a question", c.Display, DisplayOffAwayQ)
	}
	if attempts.Load() == 0 {
		t.Error("the host probe was never dialled, so 'unfinished' would be describing something that did not happen")
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("Snapshot took %v: a hung host probe is outliving the round it is nested in", elapsed)
	}
}

// TestU1_HostSilenceInsideTheBudgetIsStillAnObservation is the other
// half, and the reason hostProbeTimeout is the SMALLER number: a probe
// cut off by its OWN 2s cap, while the 3s round is still running, is a
// real negative observation about that host and must keep saying so.
// Collapse the two and the honest OFF/AWAY disappears along with the
// dishonest one.
func TestU1_HostSilenceInsideTheBudgetIsStillAnObservation(t *testing.T) {
	var attempts atomic.Int64
	s := u1Server(t, Cell{
		Name: "roamer", URL: "http://127.0.0.1:1", Class: "roaming",
		HostProbe: "10.255.255.1:22",
	})
	// The probe's own cap fires first; the round has budget left over.
	s.snapTimeout = 2 * time.Second
	s.hostDial = 120 * time.Millisecond
	s.hostConnect = u1HangingDial(&attempts)

	start := time.Now()
	snap := s.Snapshot(context.Background())
	elapsed := time.Since(start)

	c := u1Cell(t, snap, "roamer")
	if c.HostReachable == nil || *c.HostReachable {
		t.Errorf("host_reachable = %s, want an observed false: nothing answered in %v and the round still had budget, which is an answer about the HOST", u1HostReachable(c), s.hostDial)
	}
	if c.HostProbeUnfinished {
		t.Error("host_probe_unfinished is set on a probe that finished: it ran out its own timeout, which is a completed negative observation")
	}
	if c.Display != DisplayOffAway {
		t.Errorf("display = %s, want %s", c.Display, DisplayOffAway)
	}
	if elapsed > s.snapTimeout {
		t.Errorf("the round took %v, past its own %v budget: the host probe is not bounded by hostDial", elapsed, s.snapTimeout)
	}
}

// TestU1_AnnounceSettlesAnUnfinishedProbe pins the one-directional
// invariant the new field carries: unfinished means there is NO value,
// never a value with a caveat. A heartbeat is the host answering, so it
// resolves the same question the probe failed to finish — and a reader
// is never handed two fields to reconcile.
func TestU1_AnnounceSettlesAnUnfinishedProbe(t *testing.T) {
	var attempts atomic.Int64
	s := u1Server(t, Cell{
		Name: "roamer", URL: "http://127.0.0.1:1", Class: "roaming",
		HostProbe: "10.255.255.1:22",
	})
	s.snapTimeout = 300 * time.Millisecond
	s.hostDial = 30 * time.Second
	s.hostConnect = u1HangingDial(&attempts)
	s.recordAnnounce(&AnnounceRequest{
		V: AnnounceVersion, Cell: "roamer", Seq: 1,
		Intent: &AnnounceIntent{State: "serving", Since: time.Now().UTC()},
	})

	c := u1Cell(t, s.Snapshot(context.Background()), "roamer")
	if c.HostReachable == nil || !*c.HostReachable {
		t.Errorf("host_reachable = %s, want true: the box just sent a heartbeat", u1HostReachable(c))
	}
	if c.HostProbeUnfinished {
		t.Error("host_probe_unfinished survived beside a host_reachable value; the field explains a MISSING value and there is no longer one missing")
	}
	if c.Display != DisplayServing {
		t.Errorf("display = %s, want %s", c.Display, DisplayServing)
	}
}

// TestU1_AWithdrawnCellSettlesAnUnfinishedProbeToo is the same invariant
// on presence's other writer. A clean withdraw is the box saying
// goodbye, which the design treats as the host being gone for our
// purposes — a DECLARATION that answers the question the probe did not
// finish. Either way the marker must not survive beside the value it
// exists to stand in for.
func TestU1_AWithdrawnCellSettlesAnUnfinishedProbeToo(t *testing.T) {
	var attempts atomic.Int64
	s := u1Server(t, Cell{
		Name: "roamer", URL: "http://127.0.0.1:1", Class: "roaming",
		HostProbe: "10.255.255.1:22",
	})
	s.snapTimeout = 300 * time.Millisecond
	s.hostDial = 30 * time.Second
	s.hostConnect = u1HangingDial(&attempts)
	s.recordAnnounce(&AnnounceRequest{
		V: AnnounceVersion, Cell: "roamer", Seq: 1,
		Intent: &AnnounceIntent{State: "withdrawing", Since: time.Now().UTC()},
	})

	c := u1Cell(t, s.Snapshot(context.Background()), "roamer")
	if c.HostReachable == nil || *c.HostReachable {
		t.Errorf("host_reachable = %s, want false: the cell declared a clean goodbye", u1HostReachable(c))
	}
	if c.HostProbeUnfinished {
		t.Error("host_probe_unfinished survived beside a host_reachable value on the withdraw path")
	}
	if c.Display != DisplayOffAway {
		t.Errorf("display = %s, want %s", c.Display, DisplayOffAway)
	}
}

// TestU1_DoctorSeparatesAnUnfinishedProbeFromAnUnconfiguredOne is the
// consumer half, and it is the reason the marker is a field and not a
// comment. doctor reads a nil host_reachable as "no host_probe
// configured" and tells the operator to set one — which, for a cell
// whose probe WAS configured and WAS dialled, is a config edit that
// changes nothing and a diagnosis of the wrong box. Distinguishing the
// two silences in the snapshot without teaching the one reader that
// interprets them would have moved the defect rather than fixed it.
func TestU1_DoctorSeparatesAnUnfinishedProbeFromAnUnconfiguredOne(t *testing.T) {
	var attempts atomic.Int64
	s, _ := doctorServer(t,
		Cell{Name: fleetcfg.FrontCell, URL: "http://127.0.0.1:1"},
		Cell{Name: "roamer", URL: "http://127.0.0.1:1", Class: "roaming", HostProbe: "10.255.255.1:22"},
		Cell{Name: "unconfigured", URL: "http://127.0.0.1:1", Class: "roaming"},
	)
	s.snapTimeout = 300 * time.Millisecond
	s.hostDial = 30 * time.Second
	s.hostConnect = u1HangingDial(&attempts)

	rep := s.Doctor(context.Background())

	stalled := mustCheck(t, rep, "roaming.announcer", "roamer")
	if !strings.Contains(stalled.Summary, "budget") {
		t.Errorf("summary = %q, want it to name fleetd's own budget", stalled.Summary)
	}
	if strings.Contains(stalled.Fix, "set cells.") {
		t.Errorf("fix = %q — it tells the operator to configure a host_probe that is already configured and was already dialled", stalled.Fix)
	}
	if stalled.Level != LevelUnknown {
		t.Errorf("level = %s, want %s: nothing was learned about this box", stalled.Level, LevelUnknown)
	}

	// And the genuinely-unconfigured cell keeps the advice that is right
	// for it — the new branch must not swallow the old one.
	unconfigured := mustCheck(t, rep, "roaming.announcer", "unconfigured")
	if !strings.Contains(unconfigured.Summary, "no host_probe") {
		t.Errorf("summary = %q, want the unconfigured-probe row unchanged", unconfigured.Summary)
	}
	if !strings.Contains(unconfigured.Fix, "set cells.unconfigured.host_probe") {
		t.Errorf("fix = %q, want the unconfigured-probe advice unchanged", unconfigured.Fix)
	}
}

// ─── last_seen persistence ──────────────────────────────────────────────────

// u1OnDisk reads the persisted sighting store — what survives a fleetd
// restart, as opposed to what the process is holding.
func u1OnDisk(t *testing.T, s *Server, cell string) (time.Time, bool) {
	t.Helper()
	seen := loadLastSeen(s.lastSeenPath)
	at, ok := seen[cell]
	return at, ok
}

// u1InMemory reads the live sighting under the hub lock.
func u1InMemory(t *testing.T, s *Server, cell string) (time.Time, bool) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.lastSeen[cell]
	return at, ok
}

// TestU1_LastSeenWritesOnTheFirstSightingThenOnlyPastTheAgeGate covers
// lastSeenPersistAge, which had no test of any kind — nor did
// recordSighting or noteSighting, the two functions the whole presence
// axis writes through. Both ends of the gate are defects: writing every
// sighting rewrites the file every 15s per cell for a value that only
// matters once the cell is GONE, and never writing loses the sighting
// entirely for exactly the no-inbound-port cells presence exists for.
func TestU1_LastSeenWritesOnTheFirstSightingThenOnlyPastTheAgeGate(t *testing.T) {
	s := u1Server(t)
	t0 := time.Date(2026, 8, 6, 9, 0, 0, 0, time.UTC)

	s.recordSighting("gpu-cell", t0, false)
	got, ok := u1OnDisk(t, s, "gpu-cell")
	if !ok || !got.Equal(t0) {
		t.Fatalf("first sighting on disk = (%v, %v), want %v: a cell sighted once and never again has nothing else to leave behind", got, ok, t0)
	}

	// One minute on: fresher in memory, unchanged on disk. This is the
	// gate doing its job, and the only observable difference between the
	// two halves.
	s.recordSighting("gpu-cell", t0.Add(time.Minute), false)
	if got, _ := u1OnDisk(t, s, "gpu-cell"); !got.Equal(t0) {
		t.Errorf("on disk = %v after a sighting %v later, want it left at %v — the file is being rewritten on every heartbeat", got, time.Minute, t0)
	}
	if got, ok := u1InMemory(t, s, "gpu-cell"); !ok || !got.Equal(t0.Add(time.Minute)) {
		t.Errorf("in memory = (%v, %v), want the fresh %v: the age gate is on the WRITE, not on the sighting", got, ok, t0.Add(time.Minute))
	}

	// Six minutes past the last WRITE (not the last sighting): due.
	s.recordSighting("gpu-cell", t0.Add(6*time.Minute), false)
	if got, _ := u1OnDisk(t, s, "gpu-cell"); !got.Equal(t0.Add(6 * time.Minute)) {
		t.Errorf("on disk = %v, want %v: past lastSeenPersistAge (%v) the sighting must reach the file", got, t0.Add(6*time.Minute), lastSeenPersistAge)
	}
}

// TestU1_LastSeenWritesOnATransitionRegardlessOfAge: returned / stale /
// withdrawn are the moments whose timestamp a human actually reads, so
// they bypass the age gate. Lose this and the fleet page shows the
// minute the announcer LAST HAPPENED TO BE WRITTEN, up to five minutes
// before the box went away.
func TestU1_LastSeenWritesOnATransitionRegardlessOfAge(t *testing.T) {
	s := u1Server(t)
	t0 := time.Date(2026, 8, 6, 9, 0, 0, 0, time.UTC)
	s.recordSighting("roamer", t0, false)

	at := t0.Add(time.Minute)
	s.recordSighting("roamer", at, true)
	if got, _ := u1OnDisk(t, s, "roamer"); !got.Equal(at) {
		t.Errorf("on disk = %v, want the transition's %v — %v inside lastSeenPersistAge, which a transition is supposed to ignore", got, at, time.Minute)
	}
}

// TestU1_NoteSightingRecordsNowAndPersistsIt: noteSighting is what every
// caller in the package actually uses (watcher connect, snapshot probe,
// announce) and it had no test either.
func TestU1_NoteSightingRecordsNowAndPersistsIt(t *testing.T) {
	s := u1Server(t)
	before := time.Now().UTC()
	s.noteSighting("front")
	after := time.Now().UTC()

	mem, ok := u1InMemory(t, s, "front")
	if !ok {
		t.Fatal("noteSighting recorded nothing")
	}
	if mem.Before(before) || mem.After(after) {
		t.Errorf("last_seen = %v, want an instant inside [%v, %v]", mem, before, after)
	}
	disk, ok := u1OnDisk(t, s, "front")
	if !ok || !disk.Equal(mem) {
		t.Errorf("on disk = (%v, %v), want the first sighting persisted as %v", disk, ok, mem)
	}
}
