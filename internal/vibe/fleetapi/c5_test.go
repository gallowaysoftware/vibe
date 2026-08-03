package fleetapi

// C5 regressions: the adversarial-review pass C4 never got. Each test
// here pins a defect that shipped green — the render loop's crash and
// config-integrity holes, and the four ways the warm policy arrived at
// the pin/keep-warm behaviour the design explicitly rejected.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
)

// ─── §2 crash and config integrity ──────────────────────────────────────────

// TestRenderLoopNonLlamaDefWithAnnouncedHashDoesNotPanic pins B1: any
// token holder could crash fleetd by announcing a hash for a model id
// matching a cell-assigned non-llama/mlx def — router.ModelCmd
// dereferenced Backend.LlamaServer whenever MLXServer was nil. Under
// systemd that is a restart loop, since the same announce arrives again.
func TestRenderLoopNonLlamaDefWithAnnouncedHashDoesNotPanic(t *testing.T) {
	kinds := map[string]profile.Backend{
		"comfyui":     {ComfyUI: &profile.ComfyUIBackend{Dir: "/opt/comfy"}},
		"http_server": {HTTPServer: &profile.HTTPServerBackend{Binary: "/usr/bin/tts", Port: 8080}},
		"tabby_api":   {External: true, TabbyAPI: &profile.TabbyAPIBackend{ModelDir: "/models/x", Repo: "/opt/tabby"}},
		"cloud_peer":  {External: true, CloudPeer: &profile.CloudPeerBackend{BaseURL: "https://api.example.test", Models: []string{"peer-model"}}},
		// An mlx def whose snapshot has not been pulled: ModelCmd returns
		// an error rather than panicking, and the pass must survive that
		// too — converting the crash into an aborted pass would freeze
		// prune, re-add and enforcement fleet-wide forever.
		"mlx_unpulled": {External: true, MLXServer: &profile.MLXServerBackend{
			Alias:       "victim",
			Huggingface: &profile.HuggingfaceRepo{Repo: "org/model"},
		}},
	}
	for kind, backend := range kinds {
		t.Run(kind, func(t *testing.T) {
			victim := &profile.BackendDef{Name: "victim", Cell: "gpu", Backend: backend}
			survivor := llmDef("survivor", "gpu", "")
			cells := []Cell{
				{Name: "front", URL: "http://127.0.0.1:1", Class: "always_on"},
				{Name: "gpu", URL: "http://127.0.0.1:3", Class: "always_on"},
			}
			hosts := &fleetcfg.File{Cells: map[string]fleetcfg.Cell{
				"front": {URL: "http://127.0.0.1:1", Class: fleetcfg.ClassAlwaysOn},
				"gpu":   {URL: "http://127.0.0.1:3", Class: fleetcfg.ClassAlwaysOn},
			}}
			probe := newRenderProbe(victim, survivor)
			s := renderLoopFixture(t, cells, hosts, RenderLoopConfig{
				FullWaveTimeout: 30 * time.Second, RenderMinInterval: time.Millisecond,
			}, probe)

			rlAnnounce(t, s, "front", rlServing(), nil)
			rlAnnounce(t, s, "gpu", rlServing(), []AnnounceModel{
				{ID: "victim", State: "ready", FlagsSHA256: "deadbeef"},
				{ID: "survivor", State: "ready"},
			})

			names := probe.waitPass(t, "render pass survives the hostile announce", func(n []string) bool {
				return slices.Contains(n, "survivor")
			})
			if !slices.Contains(names, "victim") {
				t.Fatalf("unverifiable def yanked from the render (fail-open is the documented bias): %v", names)
			}
			probe.waitWrite(t, "config written despite the unverifiable def")

			// A later legitimate transition must still re-render: proves the
			// bad def did not permanently freeze the loop.
			probe.setVersion("v2")
			s.noteRenderTrigger("gpu")
			probe.waitWrite(t, "re-render after the hostile announce (no permanent freeze)")
		})
	}
}

// TestRenderLoopEmptyDefsRefusesToOverwriteFrontConfig pins M4: LoadDefs
// returns (nil, nil) for an empty dir, Render succeeds with a
// header-only config, and the pass used to write it over a good one. The
// shipped deploy makes this reachable — the fleetd README tells operators
// to mkdir the defs mount.
func TestRenderLoopEmptyDefsRefusesToOverwriteFrontConfig(t *testing.T) {
	cells := []Cell{{Name: "front", URL: "http://127.0.0.1:1", Class: "always_on"}}
	hosts := &fleetcfg.File{Cells: map[string]fleetcfg.Cell{
		"front": {URL: "http://127.0.0.1:1", Class: fleetcfg.ClassAlwaysOn},
	}}
	probe := newRenderProbe() // zero defs: the empty-mount case
	dir := t.TempDir()
	front := filepath.Join(dir, "front.yaml")
	good := "peers: laptop-model,gpu-model\nv1\n"
	if err := os.WriteFile(front, []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := RenderLoopConfig{
		FullWaveTimeout: 30 * time.Second, RenderMinInterval: time.Millisecond,
		Hosts: hosts, FrontConfigPath: front,
		LoadDefs: probe.loadDefs, Render: probe.render, WriteFile: probe.writeFile,
	}
	s := New(cells, filepath.Join(dir, "hist.json"), testDaemonInfo, Options{})
	t.Cleanup(s.Close)
	s.StartRenderLoop(cfg)

	rlAnnounce(t, s, "front", rlServing(), nil)
	probe.assertSilent(t, 300*time.Millisecond, "empty defs dir must not render or write")
	if got := s.RenderCount(); got != 0 {
		t.Errorf("RenderCount = %d, want 0", got)
	}
	current, err := os.ReadFile(front)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != good {
		t.Errorf("front config was overwritten: %q", string(current))
	}
}

// TestRenderLoopAllDefsExcludedStillWrites is the companion that proves
// the guard is INPUT-side: defs loaded but every one pruned by class
// policy is a legitimate empty render, and gating on the output would
// deadlock the empty-fleet case.
func TestRenderLoopAllDefsExcludedStillWrites(t *testing.T) {
	cells := []Cell{
		{Name: "front", URL: "http://127.0.0.1:1", Class: "always_on"},
		{Name: "laptop", URL: "http://127.0.0.1:2", Class: "roaming"},
	}
	hosts := &fleetcfg.File{Cells: map[string]fleetcfg.Cell{
		"front":  {URL: "http://127.0.0.1:1", Class: fleetcfg.ClassAlwaysOn},
		"laptop": {URL: "http://127.0.0.1:2", Class: fleetcfg.ClassRoaming},
	}}
	probe := newRenderProbe(llmDef("laptop-model", "laptop", ""))
	dir := t.TempDir()
	front := filepath.Join(dir, "front.yaml")
	if err := os.WriteFile(front, []byte("peers: laptop-model\nv1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := RenderLoopConfig{
		FullWaveTimeout: 30 * time.Second, RenderMinInterval: time.Millisecond,
		Hosts: hosts, FrontConfigPath: front,
		LoadDefs: probe.loadDefs, Render: probe.render, WriteFile: probe.writeFile,
	}
	s := New(cells, filepath.Join(dir, "hist.json"), testDaemonInfo, Options{})
	t.Cleanup(s.Close)
	s.StartRenderLoop(cfg)

	rlAnnounce(t, s, "front", rlServing(), nil)
	rlAnnounce(t, s, "laptop", rlServing(), nil)
	probe.waitPass(t, "initial render", func(n []string) bool { return slices.Contains(n, "laptop-model") })

	// Withdraw the only def-owning cell: every def is excluded, the render
	// is legitimately peerless, and the write MUST still land.
	rlAnnounce(t, s, "laptop", &AnnounceIntent{State: "withdrawing", Since: time.Now()}, nil)
	probe.waitPass(t, "render with every def excluded", func(n []string) bool { return len(n) == 0 })
	content := probe.waitWrite(t, "peerless render still writes")
	if strings.Contains(content, "laptop-model") {
		t.Errorf("pruned def survived the write: %q", content)
	}
}

// ─── §3 warm-target policy ──────────────────────────────────────────────────

func warmTarget(t *testing.T, cell, model string, after time.Duration) (*Server, *warmProbe, WarmTarget) {
	t.Helper()
	probe := &warmProbe{}
	s := newWarmServer(t, []Cell{{Name: cell, URL: "http://127.0.0.1:1", Class: "always_on"}})
	return s, probe, WarmTarget{Cell: cell, Model: model, RestoreAfterIdle: after}
}

// TestWarmTarget_SkipsDrainedCell pins M1. A drained cell keeps
// announcing an EMPTY model list by design, which the nothing-resident
// branch read as "restore the default" — and where the drain leaves
// llama-swap up, that warm succeeds, reloading the model onto the GPU the
// operator just reclaimed. The skip was asserted in four documents and
// implemented in none.
func TestWarmTarget_SkipsDrainedCell(t *testing.T) {
	t.Run("drain echoed by the cell", func(t *testing.T) {
		s, probe, target := warmTarget(t, "heavy", "default-model", time.Millisecond)
		s.startWarmLoopWithConfig(warmLoopConfig{
			targets: []WarmTarget{target}, frontURL: "http://front.test",
			warmFn: probe.warm, tick: 20 * time.Millisecond, emptyGrace: time.Millisecond,
		})
		s.recordAnnounce(&AnnounceRequest{
			V: AnnounceVersion, Cell: "heavy", Seq: 1,
			Intent: &AnnounceIntent{State: "drained", Since: time.Now().UTC()},
		})
		time.Sleep(250 * time.Millisecond)
		if got := probe.got(); len(got) != 0 {
			t.Fatalf("warmed a drained cell (reloads the GPU the operator just reclaimed): %v", got)
		}
		if got := warmTargetStateOf(s, 0); got.State != "skipped" || !strings.Contains(got.Detail, "drained") {
			t.Errorf("state = %s/%q, want skipped/drained", got.State, got.Detail)
		}
	})

	t.Run("drain requested through fleetd, not yet echoed", func(t *testing.T) {
		// The registry request alone must skip: an operator drain the cell
		// hasn't acked is still a drain in progress.
		s, probe, target := warmTarget(t, "heavy", "default-model", time.Millisecond)
		s.mu.Lock()
		s.intents["heavy"] = Intent{State: "drained", Since: time.Now().UTC()}
		s.mu.Unlock()
		s.startWarmLoopWithConfig(warmLoopConfig{
			targets: []WarmTarget{target}, frontURL: "http://front.test",
			warmFn: probe.warm, tick: 20 * time.Millisecond, emptyGrace: time.Millisecond,
		})
		time.Sleep(200 * time.Millisecond)
		if got := probe.got(); len(got) != 0 {
			t.Fatalf("warmed a cell with a pending drain request: %v", got)
		}
	})

	t.Run("newer serving echo resolves the drain request", func(t *testing.T) {
		// The C3 conflict rule: the box you're standing at is always right.
		// A resume performed locally must not leave the target skipped
		// forever.
		s, probe, target := warmTarget(t, "heavy", "default-model", time.Millisecond)
		s.mu.Lock()
		s.intents["heavy"] = Intent{State: "drained", Since: time.Now().UTC().Add(-time.Hour)}
		s.mu.Unlock()
		s.recordAnnounce(&AnnounceRequest{
			V: AnnounceVersion, Cell: "heavy", Seq: 1,
			Intent: &AnnounceIntent{State: "serving", Since: time.Now().UTC()},
			Models: []AnnounceModel{{ID: "challenger", State: "ready"}},
		})
		if in, ok := s.effectiveIntent("heavy"); ok && in.State == "drained" {
			t.Fatal("newer serving echo did not resolve the drain request")
		}
		s.startWarmLoopWithConfig(warmLoopConfig{
			targets: []WarmTarget{target}, frontURL: "http://front.test",
			warmFn: probe.warm, tick: 20 * time.Millisecond,
		})
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if len(probe.got()) > 0 {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("target never restored after the drain was resolved: %+v", warmTargetStateOf(s, 0))
	})
}

// TestWarmTarget_InFlightRequestBlocksRestore pins CC-1's first half: the
// eval never consulted the cell's in-flight count, so a generation longer
// than restore_after_idle read as idle and was evicted mid-stream.
func TestWarmTarget_InFlightRequestBlocksRestore(t *testing.T) {
	s, probe, target := warmTarget(t, "heavy", "default-model", 20*time.Millisecond)
	st := &warmTargetState{Cell: target.Cell, Model: target.Model}
	cfg := warmLoopConfig{targets: []WarmTarget{target}, frontURL: "http://front.test", warmFn: probe.warm}

	// One request starts on the challenger. No further frame arrives for
	// the whole generation.
	s.trackInFlight("heavy", inflightFrame(t, "challenger"))
	time.Sleep(60 * time.Millisecond) // well past restore_after_idle

	snap := CellSnapshot{Models: []ModelState{{ID: "challenger", State: "ready"}}}
	s.applyWarmEval(target, st, cfg, snap)
	if got := probe.got(); len(got) != 0 {
		t.Fatalf("evicted a model mid-generation: %v", got)
	}
	s.mu.Lock()
	detail := st.Detail
	s.mu.Unlock()
	if !strings.Contains(detail, "in-flight") {
		t.Errorf("detail = %q, want the busy reason", detail)
	}

	// The completion edge arrives: the idle window restarts from HERE, not
	// from when the request started.
	s.trackInFlight("heavy", inflightFrame(t))
	s.applyWarmEval(target, st, cfg, snap)
	if got := probe.got(); len(got) != 0 {
		t.Fatalf("window did not restart at completion: %v", got)
	}
	time.Sleep(40 * time.Millisecond)
	s.applyWarmEval(target, st, cfg, snap)
	if got := probe.got(); len(got) != 1 {
		t.Fatalf("no restore after a full idle window post-completion: %v", got)
	}
}

// TestWarmTarget_NoActivityEvidenceMeasuresFromFleetdStart pins M2: the
// unknown-activity branch claimed a fabricated hour of silence, so any
// restore_after_idle <= 1h fired on the first tick — on announce-only
// cells (the no-inbound-port case C3 exists for) and after every fleetd
// restart.
func TestWarmTarget_NoActivityEvidenceMeasuresFromFleetdStart(t *testing.T) {
	s, probe, target := warmTarget(t, "heavy", "default-model", time.Hour)
	st := &warmTargetState{Cell: target.Cell, Model: target.Model}
	cfg := warmLoopConfig{targets: []WarmTarget{target}, frontURL: "http://front.test", warmFn: probe.warm}

	snap := CellSnapshot{Models: []ModelState{{ID: "challenger", State: "ready"}}}
	s.applyWarmEval(target, st, cfg, snap)
	if got := probe.got(); len(got) != 0 {
		t.Fatalf("warmed on the first eval with zero activity evidence: %v", got)
	}
	s.mu.Lock()
	detail := st.Detail
	s.mu.Unlock()
	if !strings.Contains(detail, "no activity evidence") {
		t.Errorf("detail = %q, want the missing evidence named", detail)
	}
}

// TestSwapIdleForAboveTheHourFloor pins M3: swapIdleFor seeded its
// accumulator at the 1h floor and only ever lowered it, so any
// restore_after_idle above an hour was silently inert and the status
// reported a fabricated "idle 1h0m0s of 4h0m0s" forever.
func TestSwapIdleForAboveTheHourFloor(t *testing.T) {
	s := newWarmServer(t, []Cell{{Name: "heavy", URL: "http://127.0.0.1:1"}})
	s.mu.Lock()
	s.modelActivity["heavy\x00challenger"] = time.Now().Add(-3 * time.Hour)
	s.mu.Unlock()

	idle, _, unknown := s.swapIdleFor("heavy", []string{"challenger"})
	if len(unknown) != 0 {
		t.Fatalf("unknown = %v, want none", unknown)
	}
	if idle < 3*time.Hour || idle > 3*time.Hour+time.Minute {
		t.Fatalf("idle = %s, want ~3h (a 1h cap makes every window above an hour inert)", idle)
	}

	target := WarmTarget{Cell: "heavy", Model: "default-model", RestoreAfterIdle: 4 * time.Hour}
	probe := &warmProbe{}
	st := &warmTargetState{Cell: target.Cell, Model: target.Model}
	cfg := warmLoopConfig{targets: []WarmTarget{target}, frontURL: "http://front.test", warmFn: probe.warm}
	snap := CellSnapshot{Models: []ModelState{{ID: "challenger", State: "ready"}}}
	s.applyWarmEval(target, st, cfg, snap)
	if got := probe.got(); len(got) != 0 {
		t.Fatalf("warmed at 3h idle with a 4h window: %v", got)
	}

	s.mu.Lock()
	s.modelActivity["heavy\x00challenger"] = time.Now().Add(-5 * time.Hour)
	s.mu.Unlock()
	s.applyWarmEval(target, st, cfg, snap)
	if got := probe.got(); len(got) != 1 {
		t.Fatalf("did not warm at 5h idle with a 4h window: %v", got)
	}
}

// ─── §6 the idle-window input path ──────────────────────────────────────────

// inflightFrame builds a real llama-swap inflight frame: the SSE
// envelope's data is a JSON STRING containing JSON (double-encoded), and
// a test handing trackInFlight a bare object hits the first Unmarshal's
// early return and passes vacuously.
func inflightFrame(t *testing.T, models ...string) json.RawMessage {
	t.Helper()
	reqs := make([]string, 0, len(models))
	for _, m := range models {
		reqs = append(reqs, `{"model":`+strconv.Quote(m)+`}`)
	}
	return json.RawMessage(strconv.Quote(`{"requests":[` + strings.Join(reqs, ",") + `]}`))
}

// TestTrackInFlightParsesRealFrames pins M9: the whole idle-window INPUT
// path was untested — replacing trackInFlight's body with a no-op left
// the repo green, so a JSON shape drift in llama-swap's frame would
// silently convert the warm policy into "always evict".
func TestTrackInFlightParsesRealFrames(t *testing.T) {
	s := newWarmServer(t, []Cell{{Name: "heavy", URL: "http://127.0.0.1:1"}})

	if n, reported := s.InFlight("heavy"); reported || n != 0 {
		t.Fatalf("InFlight before any frame = (%d, %v), want (0, false)", n, reported)
	}
	s.trackInFlight("heavy", inflightFrame(t, "challenger", "challenger"))
	n, reported := s.InFlight("heavy")
	if !reported || n != 2 {
		t.Fatalf("InFlight = (%d, %v), want (2, true)", n, reported)
	}
	started, ok := s.modelLastActivity("heavy", "challenger")
	if !ok {
		t.Fatal("no per-model activity recorded; the frame parsed vacuously")
	}
	if time.Since(started) > time.Minute {
		t.Errorf("activity timestamp is stale: %v", started)
	}

	// The remove edge: the model leaves the list, and its activity must be
	// re-stamped — "last activity" means started OR finished.
	time.Sleep(5 * time.Millisecond)
	s.trackInFlight("heavy", inflightFrame(t))
	if n, _ := s.InFlight("heavy"); n != 0 {
		t.Fatalf("InFlight after the remove frame = %d, want 0", n)
	}
	finished, ok := s.modelLastActivity("heavy", "challenger")
	if !ok {
		t.Fatal("activity entry vanished on the remove frame")
	}
	if !finished.After(started) {
		t.Errorf("completion edge not stamped: start %v, end %v", started, finished)
	}

	// A frame that is NOT double-encoded must be ignored, not misparsed.
	s.trackInFlight("heavy", json.RawMessage(`{"requests":[{"model":"x"}]}`))
	if n, _ := s.InFlight("heavy"); n != 0 {
		t.Errorf("a bare (non-double-encoded) frame changed the count: %d", n)
	}
}

// ─── §4 the schedule guard ──────────────────────────────────────────────────

func schedFixture(t *testing.T) (*Server, *warmProbe, WarmScheduleEntry, cronSpec, *warmScheduleState, time.Time) {
	t.Helper()
	probe := &warmProbe{}
	s := newWarmServer(t, []Cell{{Name: "heavy", URL: "http://127.0.0.1:1", Class: "always_on"}})
	entry := WarmScheduleEntry{Cron: "* * * * *", Model: "default-model"}
	spec, err := parseCron(entry.Cron)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s.mu.Lock()
	s.schedStates = []*warmScheduleState{{Cron: entry.Cron, Model: entry.Model, NextFire: &now}}
	st := s.schedStates[0]
	s.mu.Unlock()
	return s, probe, entry, spec, st, now
}

// TestScheduleGuardSkipsWhenTheGuardCannotBeEvaluated pins CC-3: the
// entire guard sat inside `if cell != ""`, and production's cellOfModel
// collapsed a hard LoadDefs error into that same empty string — so ONE
// malformed YAML in the backends dir silently converted every scheduled
// warm into an unguarded one.
func TestScheduleGuardSkipsWhenTheGuardCannotBeEvaluated(t *testing.T) {
	t.Run("defs unreadable", func(t *testing.T) {
		s, probe, entry, spec, st, now := schedFixture(t)
		s.mu.Lock()
		s.inFlight["heavy"] = 3
		s.inFlightSeen["heavy"] = true
		s.mu.Unlock()
		boom := func(string) (string, error) { return "", os.ErrPermission }
		s.evalScheduleEntry(entry, spec, st, boom, time.UTC, probe.warm, "http://front.test", now)
		if got := probe.got(); len(got) != 0 {
			t.Fatalf("fired unguarded when the guard could not be evaluated: %v", got)
		}
		if note := schedStateOf(s, 0).LastNote; !strings.Contains(note, "cannot resolve cell") {
			t.Errorf("note = %q, want the resolve failure", note)
		}
	})

	t.Run("in-flight never reported", func(t *testing.T) {
		// Unknown in-flight is not zero in-flight.
		s, probe, entry, spec, st, now := schedFixture(t)
		cellOf := func(string) (string, error) { return "heavy", nil }
		s.evalScheduleEntry(entry, spec, st, cellOf, time.UTC, probe.warm, "http://front.test", now)
		if got := probe.got(); len(got) != 0 {
			t.Fatalf("fired into a cell with no in-flight signal: %v", got)
		}
		if note := schedStateOf(s, 0).LastNote; !strings.Contains(note, "in-flight unknown") {
			t.Errorf("note = %q, want the unknown-in-flight skip", note)
		}
	})

	t.Run("front-only alias stays warmable but is labelled", func(t *testing.T) {
		// A model with no backend def is legitimately warmable and
		// legitimately unguardable; a hard skip here would silently drop
		// every front-only alias's schedule.
		s, probe, entry, spec, st, now := schedFixture(t)
		noCell := func(string) (string, error) { return "", nil }
		s.evalScheduleEntry(entry, spec, st, noCell, time.UTC, probe.warm, "http://front.test", now)
		if got := probe.got(); len(got) != 1 {
			t.Fatalf("front-only alias did not warm: %v", got)
		}
		if note := schedStateOf(s, 0).LastNote; !strings.Contains(note, "unguarded") {
			t.Errorf("note = %q, want the unguarded label", note)
		}
	})
}

// TestScheduleLoopSkipsNeverFiringSpec pins CC-4: "0 0 30 2 *" parses
// (dom 1-31 and month 2 are each individually valid) but never fires, and
// the goroutine was started anyway — burning the full multi-year scan
// every minute forever.
func TestScheduleLoopSkipsNeverFiringSpec(t *testing.T) {
	probe := &warmProbe{}
	s := newWarmServer(t, []Cell{{Name: "heavy", URL: "http://127.0.0.1:1", Class: "always_on"}})
	cellOf := func(string) (string, error) { return "heavy", nil }
	s.startScheduleLoopWithConfig([]WarmScheduleEntry{
		{Cron: "0 0 30 2 *", Model: "never"},
	}, cellOf, time.UTC, probe.warm, "http://front.test")

	got := schedStateOf(s, 0)
	if got.NextFire != nil {
		t.Errorf("next_fire = %v for an unfireable spec, want none", got.NextFire)
	}
	if !strings.Contains(got.LastNote, "no fire time") {
		t.Errorf("note = %q, want the never-fires note", got.LastNote)
	}
	// The goroutine was never registered: Close returns without waiting on
	// a minute ticker.
	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked; a goroutine was started for an unfireable spec")
	}
}

// TestScheduleTerminalNoteKeepsBothFacts pins CC-4's second half: the
// !ok branch set LastNote and two statements later the per-fire note
// overwrote it, so an operator reading fleet_status saw "warmed" and no
// hint that the schedule would never run again.
func TestScheduleTerminalNoteKeepsBothFacts(t *testing.T) {
	probe := &warmProbe{}
	s := newWarmServer(t, []Cell{{Name: "heavy", URL: "http://127.0.0.1:1", Class: "always_on"}})
	// "0 0 30 2 *" parses (dom 30 and month 2 are each individually
	// valid) but matches no instant, so the re-park after the fire fails.
	entry := WarmScheduleEntry{Cron: "0 0 30 2 *", Model: "never-again"}
	spec, err := parseCron(entry.Cron)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s.mu.Lock()
	s.schedStates = []*warmScheduleState{{Cron: entry.Cron, Model: entry.Model, NextFire: &now}}
	st := s.schedStates[0]
	s.mu.Unlock()

	noCell := func(string) (string, error) { return "", nil }
	s.evalScheduleEntry(entry, spec, st, noCell, time.UTC, probe.warm, "http://front.test", now)
	got := schedStateOf(s, 0)
	if got.LastFire == nil {
		t.Fatal("fire not recorded")
	}
	if got.NextFire != nil {
		t.Errorf("next_fire = %v, want none", got.NextFire)
	}
	if !strings.Contains(got.LastNote, "warmed") || !strings.Contains(got.LastNote, "no further fire time") {
		t.Errorf("note = %q, want both the fire outcome and the terminal warning", got.LastNote)
	}
}

// ─── §7 the fleet page ──────────────────────────────────────────────────────

// TestFleetPageAttributesUseAttrEscaper pins PAGE-1: esc() is a TEXT
// escaper (it leaves " alone), so interpolating it into an attribute
// value lets a quote in operator config break out of the attribute.
// Attribute interpolation must go through attr().
func TestFleetPageAttributesUseAttrEscaper(t *testing.T) {
	data, err := fleetPageFS.ReadFile("fleet.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(data)
	for _, re := range []string{
		`=\s*"[^"\n]*\$\{\s*esc\(`,
		`=\s*'[^'\n]*\$\{\s*esc\(`,
	} {
		if m := regexp.MustCompile(re).FindString(page); m != "" {
			t.Errorf("esc() interpolated into an attribute value (%q); use attr()", m)
		}
	}
	if !strings.Contains(page, "function attr(") {
		t.Error("attr() helper missing from the page")
	}
	// The cell URL comes from hosts.yaml; a javascript: origin must not
	// become a clickable script link.
	if !strings.Contains(page, "function safeURL(") {
		t.Error("safeURL() gate missing from the page")
	}
}

// ─── §5 lifecycle ───────────────────────────────────────────────────────────

// TestCloseUnblocksAnInFlightWarm pins CC-2: both warm paths called
// warmFn synchronously under a 10-minute context with no link to s.done,
// on goroutines registered with s.wg — so Close() → wg.Wait() could hang
// for the full ten minutes on a warm against an unreachable front.
func TestCloseUnblocksAnInFlightWarm(t *testing.T) {
	s := New([]Cell{{Name: "heavy", URL: "http://127.0.0.1:1"}}, t.TempDir()+"/hist.json", testDaemonInfo, Options{})
	entered := make(chan struct{})
	var once bool
	blocking := func(ctx context.Context, frontURL, model string) error {
		if !once {
			once = true
			close(entered)
		}
		<-ctx.Done()
		return ctx.Err()
	}
	target := WarmTarget{Cell: "heavy", Model: "default-model", RestoreAfterIdle: time.Millisecond}
	s.startWarmLoopWithConfig(warmLoopConfig{
		targets: []WarmTarget{target}, frontURL: "http://front.test",
		warmFn: blocking, tick: 10 * time.Millisecond, emptyGrace: time.Millisecond,
	})
	s.recordAnnounce(&AnnounceRequest{
		V: AnnounceVersion, Cell: "heavy", Seq: 1,
		Intent: &AnnounceIntent{State: "serving", Since: time.Now().UTC()},
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("warm never fired")
	}

	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close blocked on an in-flight warm (the 10-minute hang)")
	}
}
