package fleetapi

// Post-merge reconciliation: C6's MIN-G wired the announce piggyback
// producer for fleetmcp's `unload_model` ONLY — the doc also names
// warmtarget/warmsched as producers, and those were C4 files absent from
// the C6 branch. These cover the other half, mirroring
// fleetmcp/c6_review_test.go's pair: a delivery failure becomes a queued
// verb, a DEFINITIVE answer stays the error it is.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// warmQueueFixture builds a cell that is in the registry (so the front
// can route to it) AND announces `model` in a stopped state — the shape
// the nothing-resident restore actually meets.
func warmQueueFixture(t *testing.T, warmErr error, announced ...string) (*Server, *warmProbe, WarmTarget, *warmTargetState, warmLoopConfig) {
	t.Helper()
	probe := &warmProbe{err: warmErr}
	s := newWarmServer(t, []Cell{{Name: "heavy", URL: "http://127.0.0.1:1", Class: "always_on"}})
	var models []AnnounceModel
	for _, id := range announced {
		models = append(models, AnnounceModel{ID: id, State: "stopped"})
	}
	if len(announced) > 0 {
		presenceOf(s, "heavy", models...)
	}
	target := WarmTarget{Cell: "heavy", Model: "default-model", RestoreAfterIdle: time.Hour}
	st := &warmTargetState{Cell: target.Cell, Model: target.Model, RestoreAfterIdleS: 3600}
	cfg := warmLoopConfig{targets: []WarmTarget{target}, frontURL: "http://front.test", warmFn: probe.warm}
	return s, probe, target, st, cfg
}

// queuedFor drains whatever the cell would collect on its next announce.
func queuedFor(s *Server, cell string) []AnnounceCommand {
	return s.drainCommands(cell, 99)
}

// TestWarmTargetUndeliverableWarmQueuesOnAnnounce is MIN-G's actual
// purpose on the warm-target side: a front that cannot deliver the warm
// is a DELAY (the cell collects the verb on its next heartbeat), not a
// dead policy entry.
func TestWarmTargetUndeliverableWarmQueuesOnAnnounce(t *testing.T) {
	s, probe, target, st, cfg := warmQueueFixture(t,
		errors.New("dial tcp 127.0.0.1:9000: connect: connection refused"), "default-model")

	s.restore(target, st, cfg, "nothing resident")

	if got := probe.got(); len(got) != 1 {
		t.Fatalf("the front warm was not attempted first: %v", got)
	}
	cmds := queuedFor(s, "heavy")
	if len(cmds) != 1 || cmds[0].Verb != "warm" || cmds[0].Model != "default-model" {
		t.Fatalf("commands = %+v, want the piggybacked warm", cmds)
	}
	got := warmStateOf(s, st)
	if !strings.Contains(got.Detail, "queued") {
		t.Errorf("detail = %q, want the queued-for-next-announce note", got.Detail)
	}
	// A queued warm has warmed nothing yet. Stamping LastRestore here
	// would be the overclaim this whole phase exists to stop.
	if got.LastRestore != nil {
		t.Errorf("last_restore stamped for a warm that only got QUEUED: %v", got.LastRestore)
	}
	if got.State != "waiting" {
		t.Errorf("state = %q, want waiting", got.State)
	}
}

// TestWarmTargetDefiniteRefusalIsNotQueued mirrors
// TestMCPUnloadDefiniteAnswerIsNotQueued: the piggyback is for delivery
// failures. A 4xx is the front ANSWERING, and the cell refuses the same
// verb identically — reporting that as "queued" is a lie.
func TestWarmTargetDefiniteRefusalIsNotQueued(t *testing.T) {
	s, _, target, st, cfg := warmQueueFixture(t,
		&warmHTTPError{Status: http.StatusNotFound, Body: "model not found"}, "default-model")

	s.restore(target, st, cfg, "nothing resident")

	if cmds := queuedFor(s, "heavy"); len(cmds) != 0 {
		t.Fatalf("a definitive 404 was queued for the announce path: %+v", cmds)
	}
	got := warmStateOf(s, st)
	if strings.Contains(got.Detail, "queued") {
		t.Errorf("detail = %q claims a queue that does not exist", got.Detail)
	}
	if !strings.Contains(got.Detail, "404") {
		t.Errorf("detail = %q, want the front's actual answer", got.Detail)
	}
}

// TestWarmTargetQueueValidatesAgainstAnnouncedModels: QueueCommand's own
// rule reaches the warm loops too — a warm nothing would ever execute is
// an error, not a queue entry that rots.
func TestWarmTargetQueueValidatesAgainstAnnouncedModels(t *testing.T) {
	s, _, target, st, cfg := warmQueueFixture(t, errors.New("connection refused"), "some-other-model")

	s.restore(target, st, cfg, "nothing resident")

	if cmds := queuedFor(s, "heavy"); len(cmds) != 0 {
		t.Fatalf("queued a warm for a model the cell never announced: %+v", cmds)
	}
	if d := warmStateOf(s, st).Detail; !strings.Contains(d, "does not announce") {
		t.Errorf("detail = %q, want the validation failure named", d)
	}
}

// TestWarmTargetNeverAnnouncedCellIsNotQueued: with nothing to collect
// the command, failing is better than pretending.
func TestWarmTargetNeverAnnouncedCellIsNotQueued(t *testing.T) {
	s, _, target, st, cfg := warmQueueFixture(t, errors.New("connection refused"))

	s.restore(target, st, cfg, "nothing resident")

	if cmds := queuedFor(s, "heavy"); len(cmds) != 0 {
		t.Fatalf("queued for a cell that has never announced: %+v", cmds)
	}
	d := warmStateOf(s, st).Detail
	if !strings.Contains(d, "never announced") {
		t.Errorf("detail = %q, want the never-announced reason", d)
	}
	// The ORIGINAL cause must survive alongside it — an operator reading
	// only "never announced" would chase the wrong box.
	if !strings.Contains(d, "connection refused") {
		t.Errorf("detail = %q dropped the original warm failure", d)
	}
}

// TestWarmTargetUnregisteredCellSkipsTheFrontAndNamesTheRegistry.
//
// This test was called …AnnounceOnlyCell… and described "a cell fleetd
// knows only through its announces", which is a state fleetd forbids:
// POST /api/fleet/announce refuses a cell absent from hosts.yaml, and
// hosts.yaml requires a url on every cell it does carry. The fixture
// below — presence for a cell the server has no registry entry for —
// therefore models a MISCONFIGURATION, and the one that reaches this
// branch in production is a backend def's `cell:` (or a sleep entry)
// naming a box hosts.yaml has never heard of. The behaviour is
// unchanged and still right: no front warm, and the piggyback attempt
// follows. What changed is that the reason names the registry instead of
// sending the operator to look for a dead announcer.
func TestWarmTargetUnregisteredCellSkipsTheFrontAndNamesTheRegistry(t *testing.T) {
	probe := &warmProbe{}
	s := newWarmServer(t, nil)
	presenceOf(s, "roamer", AnnounceModel{ID: "default-model", State: "stopped"})
	target := WarmTarget{Cell: "roamer", Model: "default-model", RestoreAfterIdle: time.Hour}
	st := &warmTargetState{Cell: target.Cell, Model: target.Model}
	cfg := warmLoopConfig{targets: []WarmTarget{target}, frontURL: "http://front.test", warmFn: probe.warm}

	s.restore(target, st, cfg, "nothing resident")

	if got := probe.got(); len(got) != 0 {
		t.Errorf("warmed through the front for a cell it cannot route to: %v", got)
	}
	cmds := queuedFor(s, "roamer")
	if len(cmds) != 1 || cmds[0].Verb != "warm" || cmds[0].Model != "default-model" {
		t.Fatalf("commands = %+v, want the piggybacked warm", cmds)
	}
	d := warmStateOf(s, st).Detail
	if !strings.Contains(d, "registry") || !strings.Contains(d, "hosts.yaml") {
		t.Errorf("detail = %q, want the missing hosts.yaml entry named — the cell cannot be announce-only, "+
			"because an announce from a cell absent from the registry is refused", d)
	}
	if strings.Contains(d, "announce-only") {
		t.Errorf("detail = %q still calls it announce-only, a state POST /api/fleet/announce forbids", d)
	}
}

// TestAnnounce_UnknownCellIsRefusedAndLeavesNoTrace is the other half of
// the same decision, and the reason the doc rather than the code was
// wrong: a cell absent from hosts.yaml cannot announce itself into the
// registry, so "announce-only membership" has never been reachable.
// Loosening it would make an announce a fleet-wide write from an
// unauthenticated name (design §6 — the fleet token authenticates the
// connection, never the cell it claims to be).
func TestAnnounce_UnknownCellIsRefusedAndLeavesNoTrace(t *testing.T) {
	s := newWarmServer(t, nil)
	rec := httptest.NewRecorder()
	body := `{"v":1,"cell":"stranger","seq":1,"interval_s":15,` +
		`"intent":{"state":"serving","since":"2026-08-05T00:00:00Z"},` +
		`"models":[{"id":"default-model","state":"ready"}]}`
	s.handleAnnounce(rec, httptest.NewRequest(http.MethodPost, "/api/fleet/announce", strings.NewReader(body)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("announce from a cell absent from hosts.yaml: HTTP %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not in the registry") {
		t.Errorf("body = %q, want the registry named", rec.Body.String())
	}
	if p := s.PresenceFor("stranger"); p != nil {
		t.Errorf("a refused announce created presence: %+v", p)
	}
	// And the refusal is what keeps the warm loops' registry lookup
	// meaningful: nothing can arrive at frontCanRoute having announced
	// itself into existence.
	if s.frontCanRoute("stranger") {
		t.Error("frontCanRoute is true for a cell that is not in the registry")
	}
}

// schedQueueFixture is schedFixture plus a satisfied guard and an
// announcing cell, so evalScheduleEntry actually reaches the warm.
func schedQueueFixture(t *testing.T, warmErr error) (*Server, *warmProbe, WarmScheduleEntry, cronSpec, *warmScheduleState, time.Time) {
	t.Helper()
	s, probe, entry, spec, st, now := schedFixture(t)
	probe.err = warmErr
	presenceOf(s, "heavy", AnnounceModel{ID: entry.Model, State: "stopped"})
	s.mu.Lock()
	s.inFlight["heavy"] = 0
	s.inFlightSeen["heavy"] = true
	s.mu.Unlock()
	return s, probe, entry, spec, st, now
}

// TestWarmScheduleUndeliverableWarmQueuesOnAnnounce: a 5xx is the front
// failing to DELIVER (upstream down), which is exactly the piggyback's
// case.
func TestWarmScheduleUndeliverableWarmQueuesOnAnnounce(t *testing.T) {
	s, probe, entry, spec, st, now := schedQueueFixture(t,
		&warmHTTPError{Status: http.StatusBadGateway, Body: "upstream down"})
	cellOf := func(string) (string, error) { return "heavy", nil }

	s.evalScheduleEntry(entry, spec, st, cellOf, time.UTC, probe.warm, "http://front.test", now)

	cmds := queuedFor(s, "heavy")
	if len(cmds) != 1 || cmds[0].Verb != "warm" || cmds[0].Model != entry.Model {
		t.Fatalf("commands = %+v, want the piggybacked warm", cmds)
	}
	got := schedStateOf(s, 0)
	if !strings.Contains(got.LastNote, "queued") {
		t.Errorf("last_note = %q, want the queued note", got.LastNote)
	}
	// last_fire is the record of a warm that HAPPENED; a queued one has
	// not happened.
	if got.LastFire != nil {
		t.Errorf("last_fire stamped for a warm that only got QUEUED: %v", got.LastFire)
	}
	if got.NextFire == nil || !got.NextFire.After(now) {
		t.Errorf("next_fire not re-parked after a queued warm: %+v", got.NextFire)
	}
}

// TestWarmScheduleDefiniteRefusalIsNotQueued: same rule, schedule side.
func TestWarmScheduleDefiniteRefusalIsNotQueued(t *testing.T) {
	s, probe, entry, spec, st, now := schedQueueFixture(t,
		&warmHTTPError{Status: http.StatusBadRequest, Body: "bad model id"})
	cellOf := func(string) (string, error) { return "heavy", nil }

	s.evalScheduleEntry(entry, spec, st, cellOf, time.UTC, probe.warm, "http://front.test", now)

	if cmds := queuedFor(s, "heavy"); len(cmds) != 0 {
		t.Fatalf("a definitive 400 was queued: %+v", cmds)
	}
	got := schedStateOf(s, 0)
	if strings.Contains(got.LastNote, "queued") {
		t.Errorf("last_note = %q claims a queue that does not exist", got.LastNote)
	}
	if !strings.Contains(got.LastNote, "400") {
		t.Errorf("last_note = %q, want the front's actual answer", got.LastNote)
	}
	if got.LastFire != nil {
		t.Errorf("last_fire stamped for a refused warm: %v", got.LastFire)
	}
}

// TestWarmScheduleFrontOnlyAliasHasNoCellToQueueOn: the unguarded
// front-only alias keeps warming through the front (C5's rule), and when
// that fails there is by definition no cell to piggyback on. The note
// must say so rather than invent one.
func TestWarmScheduleFrontOnlyAliasHasNoCellToQueueOn(t *testing.T) {
	s, probe, entry, spec, st, now := schedQueueFixture(t, errors.New("connection refused"))
	noCell := func(string) (string, error) { return "", nil }

	s.evalScheduleEntry(entry, spec, st, noCell, time.UTC, probe.warm, "http://front.test", now)

	if got := probe.got(); len(got) != 1 {
		t.Fatalf("front-only alias did not attempt the front warm: %v", got)
	}
	if cmds := queuedFor(s, "heavy"); len(cmds) != 0 {
		t.Fatalf("queued onto a cell the model was never resolved to: %+v", cmds)
	}
	if note := schedStateOf(s, 0).LastNote; !strings.Contains(note, "no cell resolved") {
		t.Errorf("last_note = %q, want the no-cell-to-piggyback-on reason", note)
	}
}

// TestWarmViaFrontTypesItsStatus is the test-truth guard on the 4xx
// rule: every assertion above rides an INJECTED warmFn, so if the
// production warm stopped returning the typed error the rule would be
// inert in production and the suite would stay green.
func TestWarmViaFrontTypesItsStatus(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		definitive bool
	}{
		{"404 is the front answering", http.StatusNotFound, true},
		{"502 is a delivery failure", http.StatusBadGateway, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "nope", tc.status)
			}))
			defer srv.Close()
			err := warmViaFront(t.Context(), srv.URL, "default-model")
			if err == nil {
				t.Fatal("a non-200 warm reported success")
			}
			if got := definitiveWarmRefusal(err); got != tc.definitive {
				t.Errorf("definitiveWarmRefusal(%v) = %v, want %v", err, got, tc.definitive)
			}
		})
	}

	// A transport failure carries no status and is never definitive.
	if definitiveWarmRefusal(warmViaFront(t.Context(), "http://127.0.0.1:1", "default-model")) {
		t.Error("a transport failure was read as the front answering")
	}
}
