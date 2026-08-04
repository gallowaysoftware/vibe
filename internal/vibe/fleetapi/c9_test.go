package fleetapi

// C9 coverage: which conditions fleetd derives from the SAME snapshot
// every other surface renders, the away/home fleet scope, and the two
// properties that keep the notifier from taking fleetd with it —
// shutdown and secrecy.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetnotify"
	"github.com/gallowaysoftware/vibe/internal/vibe/router"
)

func notifyServer(t *testing.T, cells []Cell) *Server {
	t.Helper()
	s := New(cells, t.TempDir()+"/hist.json", testDaemonInfo, Options{
		IntentPath:      t.TempDir() + "/intent.json",
		LeasePath:       t.TempDir() + "/leases.json",
		NotifyScopePath: t.TempDir() + "/notify-scope.json",
	})
	s.baseBackoff = 10 * time.Millisecond
	s.maxBackoff = 50 * time.Millisecond
	t.Cleanup(s.Close)
	return s
}

// snapCell builds the derived snapshot for one cell without probing:
// decorate is the function under test's real input, so the conditions
// are derived from exactly what /api/fleet/state would carry.
func snapCell(s *Server, name string, reachable bool, hostUp *bool) CellSnapshot {
	snap := CellSnapshot{Name: name, URL: "http://127.0.0.1:1", Class: s.cellClass(name),
		Reachable: reachable, HostReachable: hostUp, Models: []ModelState{}}
	s.decorate(&snap)
	return snap
}

func boolp(v bool) *bool { return &v }

func kinds(conds []fleetnotify.Condition) []string {
	out := make([]string, 0, len(conds))
	for _, c := range conds {
		out = append(out, string(c.Kind)+"/"+c.Scope)
	}
	return out
}

// TestNotifyConditions_OnlyAlwaysOnAbsenceAlarms is the class table's
// alarm column, evaluated: absence is alarming for always_on and normal
// for the other two classes, forever. A laptop closing its lid at 23:00
// every night must produce nothing, or the notifier is worse than none.
func TestNotifyConditions_OnlyAlwaysOnAbsenceAlarms(t *testing.T) {
	s := notifyServer(t, []Cell{
		{Name: "front", URL: "http://127.0.0.1:1", Class: "always_on"},
		{Name: "gpu-cell", URL: "http://127.0.0.1:1", Class: "opportunistic"},
		{Name: "laptop", URL: "http://127.0.0.1:1", Class: "roaming"},
	})
	snap := StateSnapshot{Cells: []CellSnapshot{
		snapCell(s, "front", false, boolp(false)),
		snapCell(s, "gpu-cell", false, boolp(false)),
		snapCell(s, "laptop", false, boolp(false)),
	}}
	got := kinds(s.notifyConditions(snap))
	if len(got) != 1 || got[0] != "cell_absent/front" {
		t.Fatalf("conditions = %v, want only the always_on cell", got)
	}
}

// TestNotifyConditions_AlwaysOnAbsenceAlarmsInEveryUnexplainedDisplay
// walks the three display states the design table gives an absent cell
// with no declared intent.
func TestNotifyConditions_AlwaysOnAbsenceAlarmsInEveryUnexplainedDisplay(t *testing.T) {
	s := notifyServer(t, []Cell{{Name: "front", URL: "http://127.0.0.1:1", Class: "always_on"}})
	for _, tc := range []struct {
		name    string
		hostUp  *bool
		display string
	}{
		{"host up, cell down", boolp(true), DisplayDrainedQ},
		{"host down", boolp(false), DisplayOffAway},
		{"no host probe", nil, DisplayOffAwayQ},
	} {
		cell := snapCell(s, "front", false, tc.hostUp)
		if cell.Display != tc.display {
			t.Fatalf("%s: display = %s, want %s", tc.name, cell.Display, tc.display)
		}
		got := kinds(s.notifyConditions(StateSnapshot{Cells: []CellSnapshot{cell}}))
		if len(got) != 1 || got[0] != "cell_absent/front" {
			t.Errorf("%s (%s): conditions = %v", tc.name, tc.display, got)
		}
	}
}

// TestNotifyConditions_DeclaredDrainSuppressesButInferredIntentNeverDoes
// is the invariant this phase is most able to break. Declared intent may
// SUPPRESS an alarm — the operator wrote down why, and paging on an
// explanation is how a notifier gets muted. Inferred intent does
// nothing: DRAINED? is read as the FACT that the intent store is empty,
// which is precisely the alarm.
func TestNotifyConditions_DeclaredDrainSuppressesButInferredIntentNeverDoes(t *testing.T) {
	s := notifyServer(t, []Cell{{Name: "front", URL: "http://127.0.0.1:1", Class: "always_on"}})

	// No intent, host up: DRAINED? — the design's "deliberate stop or
	// crash loop". Alarms.
	unexplained := snapCell(s, "front", false, boolp(true))
	if got := kinds(s.notifyConditions(StateSnapshot{Cells: []CellSnapshot{unexplained}})); len(got) != 1 {
		t.Fatalf("DRAINED? did not alarm: %v", got)
	}

	if _, err := s.SetIntent("front", "drained", "gaming", "23:00"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		hostUp  *bool
		display string
	}{
		{"declared drain, host up", boolp(true), DisplayDrained},
		{"declared drain, host down", boolp(false), DisplayOff},
	} {
		cell := snapCell(s, "front", false, tc.hostUp)
		if cell.Display != tc.display {
			t.Fatalf("%s: display = %s, want %s", tc.name, cell.Display, tc.display)
		}
		if got := kinds(s.notifyConditions(StateSnapshot{Cells: []CellSnapshot{cell}})); len(got) != 0 {
			t.Errorf("%s: paged on an explanation the operator wrote down: %v", tc.name, got)
		}
	}
}

// TestNotifyConditions_InconsistentIsANagNotAnAlarm: the cell answers,
// the fleet serves, and the design already calls INCONSISTENT a nag.
func TestNotifyConditions_InconsistentIsANagNotAnAlarm(t *testing.T) {
	s := notifyServer(t, []Cell{{Name: "front", URL: "http://127.0.0.1:1", Class: "always_on"}})
	if _, err := s.SetIntent("front", "drained", "gaming", ""); err != nil {
		t.Fatal(err)
	}
	cell := snapCell(s, "front", true, boolp(true))
	if cell.Display != DisplayInconsistent {
		t.Fatalf("display = %s, want INCONSISTENT", cell.Display)
	}
	if got := kinds(s.notifyConditions(StateSnapshot{Cells: []CellSnapshot{cell}})); len(got) != 0 {
		t.Fatalf("INCONSISTENT alarmed: %v", got)
	}
}

// TestNotifyConditions_DrainWithAnActiveLeaseAlarmsAndNamesTheHolder is
// the "did I just strand a 19-hour job?" answer, arriving without being
// asked. Leases stay advisory: this reports, it never blocks.
func TestNotifyConditions_DrainWithAnActiveLeaseAlarmsAndNamesTheHolder(t *testing.T) {
	s := notifyServer(t, []Cell{{Name: "gpu-cell", URL: "http://127.0.0.1:1", Class: "opportunistic"}})
	if _, err := s.SetIntent("gpu-cell", "drained", "gaming", ""); err != nil {
		t.Fatal(err)
	}

	// Drained, no lease: nothing.
	cell := snapCell(s, "gpu-cell", false, boolp(true))
	if got := kinds(s.notifyConditions(StateSnapshot{Cells: []CellSnapshot{cell}})); len(got) != 0 {
		t.Fatalf("a plain drain alarmed: %v", got)
	}

	s.mu.Lock()
	s.leases[leaseKey("gpu-cell", "qwen3.6-27b", "batch-a")] = Lease{
		Cell: "gpu-cell", Model: "qwen3.6-27b", Holder: "batch-a",
		ExpiresAt: time.Now().Add(time.Hour),
	}
	s.mu.Unlock()

	cell = snapCell(s, "gpu-cell", false, boolp(true))
	conds := s.notifyConditions(StateSnapshot{Cells: []CellSnapshot{cell}})
	if len(conds) != 1 || conds[0].Kind != fleetnotify.KindDrainWithLease {
		t.Fatalf("conditions = %v", kinds(conds))
	}
	for _, want := range []string{"batch-a", "qwen3.6-27b", "gaming"} {
		if !strings.Contains(conds[0].Detail, want) {
			t.Fatalf("detail does not name %q: %q", want, conds[0].Detail)
		}
	}

	// Expiry resolves it: a lease is a hold, and a crashed consumer must
	// not haunt the pager either.
	s.mu.Lock()
	s.leases[leaseKey("gpu-cell", "qwen3.6-27b", "batch-a")] = Lease{
		Cell: "gpu-cell", Model: "qwen3.6-27b", Holder: "batch-a",
		ExpiresAt: time.Now().Add(-time.Second),
	}
	s.mu.Unlock()
	cell = snapCell(s, "gpu-cell", false, boolp(true))
	if got := kinds(s.notifyConditions(StateSnapshot{Cells: []CellSnapshot{cell}})); len(got) != 0 {
		t.Fatalf("an expired lease still alarmed: %v", got)
	}
}

// TestNotifyConditions_FingerprintMismatchRidesTheRenderPassSet: the
// mismatch EVENT fires once and then goes silent (renderPass runs on
// triggers, and a steady wrong hash triggers nothing), so persistence is
// measured against the set the pass rebuilds.
func TestNotifyConditions_FingerprintMismatchRidesTheRenderPassSet(t *testing.T) {
	s := notifyServer(t, []Cell{{Name: "gpu-cell", URL: "http://127.0.0.1:1", Class: "opportunistic"}})
	if got := kinds(s.notifyConditions(StateSnapshot{})); len(got) != 0 {
		t.Fatalf("a mismatch appeared out of nowhere: %v", got)
	}

	first := time.Now().UTC().Add(-time.Hour)
	s.setFingerprintMismatches([]FingerprintMismatch{
		{Cell: "gpu-cell", Model: "bge-m3", Expected: "aaaa1111bbbb2222", Got: "cccc3333dddd4444", Mode: "strict"},
	}, first)
	conds := s.notifyConditions(StateSnapshot{})
	if len(conds) != 1 || conds[0].Kind != fleetnotify.KindFingerprint || conds[0].Scope != "gpu-cell/bge-m3" {
		t.Fatalf("conditions = %v", kinds(conds))
	}
	if !strings.Contains(conds[0].Detail, "strict") || !strings.Contains(conds[0].Detail, first.Format(time.RFC3339)) {
		t.Fatalf("detail lost the mode or the since: %q", conds[0].Detail)
	}

	// A later pass that finds it again must NOT restart the clock — the
	// dwell measures persistence.
	s.setFingerprintMismatches([]FingerprintMismatch{
		{Cell: "gpu-cell", Model: "bge-m3", Expected: "aaaa1111bbbb2222", Got: "cccc3333dddd4444", Mode: "strict"},
	}, time.Now().UTC())
	if got := s.FingerprintMismatches(); len(got) != 1 || !got[0].FirstSeen.Equal(first) {
		t.Fatalf("first_seen moved: %+v", got)
	}

	// A pass that finds nothing clears it.
	s.setFingerprintMismatches(nil, time.Now().UTC())
	if got := kinds(s.notifyConditions(StateSnapshot{})); len(got) != 0 {
		t.Fatalf("a resolved mismatch survived the pass: %v", got)
	}
}

// TestNotifyConditions_DegradedModelIsAConditionButNotADefaultAlarm:
// C8's verdict is available to a policy that asks for it and absent from
// the shipped one.
func TestNotifyConditions_DegradedModelIsAConditionButNotADefaultAlarm(t *testing.T) {
	s := notifyServer(t, []Cell{{Name: "gpu-cell", URL: "http://127.0.0.1:1", Class: "opportunistic"}})
	cell := snapCell(s, "gpu-cell", true, boolp(true))
	cell.Models = []ModelState{{ID: "qwen", State: "ready", Probe: probeBlock(VerdictDegraded, 4, 40)}}
	conds := s.notifyConditions(StateSnapshot{Cells: []CellSnapshot{cell}})
	if len(conds) != 1 || conds[0].Kind != fleetnotify.KindModelDegraded {
		t.Fatalf("conditions = %v", kinds(conds))
	}
	tr := fleetnotify.NewTracker(fleetnotify.Policy{})
	if out := tr.Step(time.Now(), conds, false); len(out) != 0 {
		t.Fatalf("the default policy delivered a degraded-model alarm: %+v", out)
	}
}

// TestRenderPass_PopulatesAndClearsTheFingerprintMismatchSet drives the
// REAL render loop: without this, every fingerprint test above could
// pass against a set nothing ever writes to.
func TestRenderPass_PopulatesAndClearsTheFingerprintMismatchSet(t *testing.T) {
	strictDef := llmDef("embed-x", "gpu", "strict")
	cells := []Cell{
		{Name: "front", URL: "http://127.0.0.1:1", Class: "always_on"},
		{Name: "gpu", URL: "http://127.0.0.1:3", Class: "always_on"},
	}
	hosts := &fleetcfg.File{Cells: map[string]fleetcfg.Cell{
		"front": {URL: "http://127.0.0.1:1", Class: fleetcfg.ClassAlwaysOn},
		"gpu":   {URL: "http://127.0.0.1:3", Class: fleetcfg.ClassAlwaysOn},
	}}
	probe := newRenderProbe(strictDef)
	cfg := RenderLoopConfig{FullWaveTimeout: 30 * time.Second, RenderMinInterval: time.Millisecond}
	cfg.Hosts = hosts
	cfg.FrontConfigPath = filepath.Join(t.TempDir(), "front.yaml")
	cfg.LoadDefs = probe.loadDefs
	cfg.Render = probe.render
	cfg.WriteFile = probe.writeFile
	s := New(cells, filepath.Join(t.TempDir(), "hist.json"), testDaemonInfo, Options{})
	t.Cleanup(s.Close)
	s.StartRenderLoop(cfg)

	if !s.renderLoopRunning() {
		t.Fatal("the render loop did not record itself as the fingerprint evaluator")
	}

	rlAnnounce(t, s, "front", rlServing(), nil)
	rlAnnounce(t, s, "gpu", rlServing(), []AnnounceModel{
		{ID: "embed-x", State: "ready", FlagsSHA256: strings.Repeat("0", 64)},
	})
	waitUntil(t, func() bool { return len(s.FingerprintMismatches()) == 1 })
	got := s.FingerprintMismatches()[0]
	if got.Cell != "gpu" || got.Model != "embed-x" || got.Mode != "strict" || got.Got != strings.Repeat("0", 64) {
		t.Fatalf("mismatch = %+v", got)
	}

	// The cell re-renders and announces the hash fleetd expects: the next
	// pass must clear the entry, or a fixed drift alarms forever.
	cmd, err := router.ModelCmd(strictDef, router.Options{Hosts: hosts})
	if err != nil {
		t.Fatal(err)
	}
	expected, err := router.FlagsSHA256(cmd)
	if err != nil {
		t.Fatal(err)
	}
	s.recordAnnounce(&AnnounceRequest{
		V: AnnounceVersion, Cell: "gpu", Seq: 2, Intent: rlServing(),
		Models: []AnnounceModel{{ID: "embed-x", State: "ready", FlagsSHA256: expected}},
	})
	waitUntil(t, func() bool { return len(s.FingerprintMismatches()) == 0 })
}

// ─── the away/home fleet scope ──────────────────────────────────────────────

// TestNotifyScope_AwayExpiresByItselfAtTheDeclaredInstant: a forgotten
// "away" must not mute the fleet past the date the operator themselves
// declared.
func TestNotifyScope_AwayExpiresByItselfAtTheDeclaredInstant(t *testing.T) {
	s := notifyServer(t, []Cell{{Name: "front", URL: "http://127.0.0.1:1", Class: "always_on"}})
	if _, err := s.SetNotifyScope(ScopeAway, "vacation", "1h", "test"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	sc := s.NotifyScopeAt(now)
	if !sc.awayAt(now) {
		t.Fatal("away did not take effect")
	}
	if sc.awayAt(now.Add(2 * time.Hour)) {
		t.Fatal("away survived its own until")
	}
}

func TestNotifyScope_RejectsAnAbsurdOrPastWindow(t *testing.T) {
	s := notifyServer(t, []Cell{{Name: "front", URL: "http://127.0.0.1:1", Class: "always_on"}})
	for _, until := range []string{"-1h", "0s", "10000h", "not-a-time"} {
		if _, err := s.SetNotifyScope(ScopeAway, "vacation", until, "test"); err == nil {
			t.Errorf("until %q was accepted", until)
		}
	}
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	if _, err := s.SetNotifyScope(ScopeAway, "vacation", past, "test"); err == nil {
		t.Error("an until in the past was accepted")
	}
}

// TestNotifyScope_SurvivesARestartAndIsNotACellIntent pins where the
// declaration lives: its own file, never a key in the cell-keyed intent
// store (whose readers would hold it as a pending request forever).
func TestNotifyScope_SurvivesARestartAndIsNotACellIntent(t *testing.T) {
	dir := t.TempDir()
	cells := []Cell{{Name: "front", URL: "http://127.0.0.1:1", Class: "always_on"}}
	opts := Options{IntentPath: dir + "/intent.json", NotifyScopePath: dir + "/notify-scope.json"}

	s := New(cells, dir+"/hist.json", testDaemonInfo, opts)
	if _, err := s.SetNotifyScope(ScopeAway, "vacation", "48h", "test"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	again := New(cells, dir+"/hist.json", testDaemonInfo, opts)
	defer again.Close()
	sc := again.NotifyScopeAt(time.Now())
	if sc == nil || !sc.awayAt(time.Now()) || sc.Reason != "vacation" {
		t.Fatalf("scope did not survive the restart: %+v", sc)
	}
	again.mu.Lock()
	n := len(again.intents)
	again.mu.Unlock()
	if n != 0 {
		t.Fatalf("the fleet scope leaked into the cell intent store: %d entries", n)
	}
}

// TestNotifyStatus_ShowsSuppressionAndNamesTheFingerprintEvaluator: the
// two things fleet_status must say for "away" to be a deferral and for a
// zero-drift report to be trustworthy.
func TestNotifyStatus_ShowsSuppressionAndNamesTheFingerprintEvaluator(t *testing.T) {
	s := notifyServer(t, []Cell{{Name: "front", URL: "http://127.0.0.1:1", Class: "always_on"}})
	sink := &captureSink{}
	s.StartNotifyLoop(NotifyLoopConfig{Sink: sink, Interval: time.Hour,
		Policy: fleetnotify.Policy{Dwell: map[fleetnotify.Kind]time.Duration{fleetnotify.KindCellAbsent: 0}}})
	if _, err := s.SetNotifyScope(ScopeAway, "vacation", "48h", "test"); err != nil {
		t.Fatal(err)
	}

	s.evalNotify(context.Background(), s.notifyRunnerForTest())
	rep := s.notifyReport()
	if rep == nil || rep.Scope != ScopeAway {
		t.Fatalf("report = %+v", rep)
	}
	if rep.Suppressed != 1 || len(rep.Alarms) != 1 {
		t.Fatalf("a suppressed alarm is invisible in fleet_status: %+v", rep)
	}
	if len(sink.sent()) != 0 {
		t.Fatalf("away delivered %d notifications", len(sink.sent()))
	}
	if !strings.Contains(rep.FingerprintSource, "unavailable") {
		t.Fatalf("fingerprint source does not name the missing evaluator: %q", rep.FingerprintSource)
	}
}

// TestNotifyStatus_NeverCarriesTheWebhookURL: the status document is
// served to every authed reader and rendered by the page. The endpoint
// appears only in its redacted form.
func TestNotifyStatus_NeverCarriesTheWebhookURL(t *testing.T) {
	const secret = "https://ntfy.example.invalid/vibe-fleet-SECRET"
	s := notifyServer(t, []Cell{{Name: "front", URL: "http://127.0.0.1:1", Class: "always_on"}})
	sink, err := fleetnotify.NewWebhookSink(fleetnotify.WebhookConfig{URL: secret})
	if err != nil {
		t.Fatal(err)
	}
	s.StartNotifyLoop(NotifyLoopConfig{Sink: sink, Interval: time.Hour})

	data, err := json.Marshal(s.notifyReport())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "vibe-fleet-SECRET") || strings.Contains(string(data), secret) {
		t.Fatalf("the status document leaked the webhook credential: %s", data)
	}
	if !strings.Contains(string(data), "ntfy.example.invalid") {
		t.Fatalf("the status document lost the useful half of the endpoint: %s", data)
	}
}

// TestNotifySend_IsDeliveredWhileAway covers the explicit path end to
// end: a human asking for a message now is not an alarm.
func TestNotifySend_IsDeliveredWhileAway(t *testing.T) {
	s := notifyServer(t, []Cell{{Name: "front", URL: "http://127.0.0.1:1", Class: "always_on"}})
	sink := &captureSink{}
	s.StartNotifyLoop(NotifyLoopConfig{Sink: sink, Interval: time.Hour})
	if _, err := s.SetNotifyScope(ScopeAway, "vacation", "48h", "test"); err != nil {
		t.Fatal(err)
	}
	if err := s.SendNotification("vibe fleet: test", "hello"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return len(sink.sent()) == 1 })
	if got := sink.sent()[0]; got.State != fleetnotify.StateExplicit {
		t.Fatalf("state = %q, want explicit", got.State)
	}
}

// TestNotifyRoutes_ScopeAndSendOverHTTP exercises the two routes the
// CLI, the MCP facade and the page all reach through.
func TestNotifyRoutes_ScopeAndSendOverHTTP(t *testing.T) {
	s := notifyServer(t, []Cell{{Name: "front", URL: "http://127.0.0.1:1", Class: "always_on"}})
	mux := http.NewServeMux()
	s.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// No sink yet: send is a 503, not a lie.
	resp, err := http.Post(srv.URL+"/api/fleet/notify/send", "application/json", strings.NewReader(`{"message":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("send without a sink: HTTP %d, want 503", resp.StatusCode)
	}

	resp, err = http.Post(srv.URL+"/api/fleet/notify/scope", "application/json",
		strings.NewReader(`{"scope":"away","reason":"vacation","until":"48h"}`))
	if err != nil {
		t.Fatal(err)
	}
	var scope NotifyScope
	if err := json.NewDecoder(resp.Body).Decode(&scope); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if scope.Scope != ScopeAway || scope.Until == nil {
		t.Fatalf("scope = %+v", scope)
	}

	resp, err = http.Post(srv.URL+"/api/fleet/notify/scope", "application/json", strings.NewReader(`{"scope":"asleep"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unknown scope: HTTP %d, want 400", resp.StatusCode)
	}
}

// TestNotifySend_RejectsAControlCharacterTitleButAcceptsAMultilineBody:
// the title becomes an HTTP header at the sink, so it gets the same
// hygiene every other display-feeding ingest gets — while a message body
// may legitimately contain newlines.
func TestNotifySend_RejectsAControlCharacterTitleButAcceptsAMultilineBody(t *testing.T) {
	s := notifyServer(t, []Cell{{Name: "front", URL: "http://127.0.0.1:1", Class: "always_on"}})
	s.StartNotifyLoop(NotifyLoopConfig{Sink: &captureSink{}, Interval: time.Hour})
	mux := http.NewServeMux()
	s.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	post := func(body string) int {
		resp, err := http.Post(srv.URL+"/api/fleet/notify/send", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := post(`{"title":"a\nb: injected","message":"x"}`); got != http.StatusBadRequest {
		t.Fatalf("a control-character title: HTTP %d, want 400", got)
	}
	if got := post(`{"title":"fleet","message":"line one\nline two"}`); got != http.StatusOK {
		t.Fatalf("a multiline message: HTTP %d, want 200", got)
	}
}

// TestNotifyLoop_CloseReturnsPromptlyWhileTheWebhookHangs is the
// shutdown gate: a notifier that wedges must not take fleetd with it.
func TestNotifyLoop_CloseReturnsPromptlyWhileTheWebhookHangs(t *testing.T) {
	block := make(chan struct{})
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	// Release the handler BEFORE closing the server: httptest.Close waits
	// on live connections, and an aborted client leaves this one active.
	defer func() { close(block); hung.Close() }()

	s := notifyServer(t, []Cell{{Name: "front", URL: "http://127.0.0.1:1", Class: "always_on"}})
	sink, err := fleetnotify.NewWebhookSink(fleetnotify.WebhookConfig{URL: hung.URL + "/topic", Timeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	s.StartNotifyLoop(NotifyLoopConfig{Sink: sink, Interval: time.Hour,
		Deliverer: fleetnotify.DelivererConfig{QueueSize: 4, Attempts: 8, Backoff: time.Hour}})
	for range 8 {
		_ = s.SendNotification("t", "m")
	}
	// Give the worker time to get stuck in the hanging request.
	time.Sleep(50 * time.Millisecond)

	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close blocked on a hung webhook")
	}
}

// TestNotifyLoop_StatusAndEvaluationDoNotDeadlock: the status block is
// rendered from INSIDE probeSnapshot and takes the tracker lock, while
// the evaluator calls Snapshot — holding that lock across the snapshot
// would deadlock the whole state surface. Two writers, one reader, one
// round each.
func TestNotifyLoop_StatusAndEvaluationDoNotDeadlock(t *testing.T) {
	s := notifyServer(t, []Cell{{Name: "front", URL: "http://127.0.0.1:1", Class: "always_on"}})
	s.StartNotifyLoop(NotifyLoopConfig{Sink: &captureSink{}, Interval: time.Hour,
		Policy: fleetnotify.Policy{Dwell: map[fleetnotify.Kind]time.Duration{fleetnotify.KindCellAbsent: 0}}})
	r := s.notifyRunnerForTest()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 20 {
			s.evalNotify(context.Background(), r)
		}
	}()
	for range 20 {
		if rep := s.Snapshot(context.Background()).Notify; rep == nil {
			t.Error("snapshot carried no notify block")
		}
	}
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("evaluation and status rendering deadlocked")
	}
}

// captureSink records deliveries without any I/O.
type captureSink struct {
	mu   sync.Mutex
	msgs []fleetnotify.Notification
}

func (c *captureSink) Send(_ context.Context, n fleetnotify.Notification) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, n)
	return nil
}

func (c *captureSink) Endpoint() string { return "capture://sink" }

func (c *captureSink) sent() []fleetnotify.Notification {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]fleetnotify.Notification{}, c.msgs...)
}

// notifyRunnerForTest exposes the wired runner so a test can drive one
// evaluation round synchronously instead of waiting on the ticker.
func (s *Server) notifyRunnerForTest() *notifyRunner {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	return s.notify
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within 3s")
}
