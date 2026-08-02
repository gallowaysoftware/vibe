package fleetapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
	"github.com/gallowaysoftware/vibe/internal/vibe/router"
)

// renderProbe sits on the RenderLoopConfig seams and records everything
// the loop would do: which defs each render pass received and what each
// write carried. Render output is derived from the received def names
// so a pruned def is visible in the written config itself.
type renderProbe struct {
	mu      sync.Mutex
	defs    []*profile.BackendDef
	version string // distinguishes render outputs across passes

	passCh  chan []string // def names per Render call
	writeCh chan string   // content per WriteFile call
}

func newRenderProbe(defs ...*profile.BackendDef) *renderProbe {
	return &renderProbe{
		defs:    defs,
		version: "v1",
		passCh:  make(chan []string, 64),
		writeCh: make(chan string, 64),
	}
}

func (p *renderProbe) loadDefs(dir string) ([]*profile.BackendDef, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.defs), nil
}

func (p *renderProbe) render(defs []*profile.BackendDef, opts router.Options) (string, error) {
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		names = append(names, d.Name)
	}
	slices.Sort(names)
	p.mu.Lock()
	version := p.version
	p.mu.Unlock()
	p.passCh <- names
	return "peers: " + strings.Join(names, ",") + "\n" + version + "\n", nil
}

func (p *renderProbe) writeFile(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	p.writeCh <- string(data)
	return nil
}

func (p *renderProbe) setVersion(v string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.version = v
}

// waitPass pulls render passes until one satisfies match, failing after
// 5s. Predicates let a test wait for the pass that reflects ITS action
// instead of racing a stale one.
func (p *renderProbe) waitPass(t *testing.T, what string, match func([]string) bool) []string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case names := <-p.passCh:
			if match(names) {
				return names
			}
		case <-deadline:
			t.Fatalf("timed out waiting for render pass: %s", what)
		}
	}
}

func (p *renderProbe) waitWrite(t *testing.T, what string) string {
	t.Helper()
	select {
	case content := <-p.writeCh:
		return content
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for config write: %s", what)
		return ""
	}
}

// drain empties pending pass/write signals so a subsequent silence
// assertion can't trip on an earlier render.
func (p *renderProbe) drain() {
	for {
		select {
		case <-p.passCh:
		case <-p.writeCh:
		default:
			return
		}
	}
}

func (p *renderProbe) assertSilent(t *testing.T, dur time.Duration, what string) {
	t.Helper()
	select {
	case names := <-p.passCh:
		t.Fatalf("%s: unexpected render pass with defs %v", what, names)
	case content := <-p.writeCh:
		t.Fatalf("%s: unexpected config write %q", what, content)
	case <-time.After(dur):
	}
}

// renderLoopFixture wires a Server with the render loop running over
// the probe seams. The URLs are unroutable: no watchers run in these
// tests, presence is driven through recordAnnounce directly.
func renderLoopFixture(t *testing.T, cells []Cell, hosts *fleetcfg.File, cfg RenderLoopConfig, probe *renderProbe) *Server {
	t.Helper()
	cfg.Hosts = hosts
	cfg.FrontConfigPath = filepath.Join(t.TempDir(), "front.yaml")
	cfg.LoadDefs = probe.loadDefs
	cfg.Render = probe.render
	cfg.WriteFile = probe.writeFile
	s := New(cells, filepath.Join(t.TempDir(), "hist.json"), testDaemonInfo, Options{})
	t.Cleanup(s.Close)
	s.StartRenderLoop(cfg)
	return s
}

func rlAnnounce(t *testing.T, s *Server, cell string, intent *AnnounceIntent, models []AnnounceModel) {
	t.Helper()
	s.recordAnnounce(&AnnounceRequest{
		V:      AnnounceVersion,
		Cell:   cell,
		Seq:    1,
		Intent: intent,
		Models: models,
	})
}

func rlServing() *AnnounceIntent { return &AnnounceIntent{State: "serving", Since: time.Now()} }

// markStale forces a cell's presence stale, mimicking what the
// staleness loop does when an announce gap exceeds stale_after. The
// loop's 5s tick is too coarse for unit tests, so the transition is
// driven directly under the hub mutex.
func markStale(t *testing.T, s *Server, cell string) {
	t.Helper()
	s.mu.Lock()
	p := s.presence[cell]
	if p == nil {
		s.mu.Unlock()
		t.Fatalf("cell %q has no presence entry", cell)
	}
	p.Stale = true
	p.HealthyStreak = 0
	s.mu.Unlock()
	s.noteRenderTrigger(cell)
}

func llmDef(name, cell, fingerprint string) *profile.BackendDef {
	return &profile.BackendDef{
		Name:        name,
		Cell:        cell,
		Fingerprint: fingerprint,
		Backend: profile.Backend{
			External:    true,
			LlamaServer: &profile.LlamaServerBackend{Path: "/models/" + name + ".gguf", Alias: name},
		},
	}
}

// threeCellFleet is the standard test fleet: the front plus a roaming
// laptop and an always_on gpu cell, each owning one def, plus one
// unassigned def that must survive every overlay.
func threeCellFleet(t *testing.T) ([]Cell, *fleetcfg.File, *renderProbe) {
	t.Helper()
	cells := []Cell{
		{Name: "front", URL: "http://127.0.0.1:1", Class: "always_on"},
		{Name: "laptop", URL: "http://127.0.0.1:2", Class: "roaming"},
		{Name: "gpu", URL: "http://127.0.0.1:3", Class: "always_on"},
	}
	hosts := &fleetcfg.File{Cells: map[string]fleetcfg.Cell{
		"front":  {URL: "http://127.0.0.1:1", Class: fleetcfg.ClassAlwaysOn},
		"laptop": {URL: "http://127.0.0.1:2", Class: fleetcfg.ClassRoaming},
		"gpu":    {URL: "http://127.0.0.1:3", Class: fleetcfg.ClassAlwaysOn},
	}}
	probe := newRenderProbe(
		llmDef("laptop-model", "laptop", ""),
		llmDef("gpu-model", "gpu", ""),
		llmDef("shared-model", "", ""),
	)
	return cells, hosts, probe
}

func allDefs(names []string) bool {
	return slices.Contains(names, "laptop-model") && slices.Contains(names, "gpu-model") && slices.Contains(names, "shared-model")
}

// TestRenderLoopColdStartHold pins the fleetd-reboot invariant: with an
// empty presence table the loop renders nothing until the full wave
// lands or the hold times out, and the first write can never be an
// empty-peers config.
func TestRenderLoopColdStartHold(t *testing.T) {
	t.Run("full wave ends the hold", func(t *testing.T) {
		cells, hosts, probe := threeCellFleet(t)
		s := renderLoopFixture(t, cells, hosts, RenderLoopConfig{
			FullWaveTimeout: 30 * time.Second, RenderMinInterval: time.Millisecond,
		}, probe)

		// A partial wave (one cell of three) fires triggers but must
		// not release the hold — no pass, no write.
		rlAnnounce(t, s, "laptop", rlServing(), nil)
		probe.assertSilent(t, 200*time.Millisecond, "partial wave during cold start")

		rlAnnounce(t, s, "gpu", rlServing(), nil)
		rlAnnounce(t, s, "front", rlServing(), nil)
		names := probe.waitPass(t, "render after full wave", allDefs)
		if !allDefs(names) {
			t.Fatalf("first render pruned defs on fresh presence: %v", names)
		}
		content := probe.waitWrite(t, "first write after full wave")
		for _, want := range []string{"laptop-model", "gpu-model", "shared-model"} {
			if !strings.Contains(content, want) {
				t.Fatalf("first write missing %q (empty-peers class of bug): %q", want, content)
			}
		}
	})

	t.Run("timeout ends the hold", func(t *testing.T) {
		cells, hosts, probe := threeCellFleet(t)
		s := renderLoopFixture(t, cells, hosts, RenderLoopConfig{
			FullWaveTimeout: 150 * time.Millisecond, RenderMinInterval: time.Millisecond,
		}, probe)

		// A trigger during the hold is owed a render when the hold
		// expires, but nothing renders before that.
		rlAnnounce(t, s, "laptop", rlServing(), nil)
		probe.assertSilent(t, 80*time.Millisecond, "cold-start hold before timeout")
		names := probe.waitPass(t, "render after hold timeout", allDefs)
		if !allDefs(names) {
			t.Fatalf("post-timeout render pruned never-stale cells: %v", names)
		}
	})
}

// TestRenderLoopClassPolicy covers design doc §4: roaming prunes on
// withdraw and on staleness; always_on holds through staleness.
func TestRenderLoopClassPolicy(t *testing.T) {
	newServingFleet := func(t *testing.T) (*Server, *renderProbe) {
		cells, hosts, probe := threeCellFleet(t)
		s := renderLoopFixture(t, cells, hosts, RenderLoopConfig{
			FullWaveTimeout: 30 * time.Second, RenderMinInterval: time.Millisecond,
			MinHealthyStreak: 3,
		}, probe)
		rlAnnounce(t, s, "front", rlServing(), nil)
		rlAnnounce(t, s, "gpu", rlServing(), nil)
		rlAnnounce(t, s, "laptop", rlServing(), nil)
		probe.waitPass(t, "initial full-wave render", allDefs)
		probe.waitWrite(t, "initial write")
		return s, probe
	}

	t.Run("roaming prunes on withdraw", func(t *testing.T) {
		s, probe := newServingFleet(t)
		rlAnnounce(t, s, "laptop", &AnnounceIntent{State: "withdrawing", Since: time.Now()}, nil)
		names := probe.waitPass(t, "render after withdraw", func(n []string) bool {
			return !slices.Contains(n, "laptop-model")
		})
		if !slices.Contains(names, "gpu-model") || !slices.Contains(names, "shared-model") {
			t.Fatalf("withdraw pruned more than the roaming cell: %v", names)
		}
		probe.waitWrite(t, "write reflecting the prune")
	})

	t.Run("roaming prunes on stale", func(t *testing.T) {
		s, probe := newServingFleet(t)
		markStale(t, s, "laptop")
		names := probe.waitPass(t, "render after stale", func(n []string) bool {
			return !slices.Contains(n, "laptop-model")
		})
		if !slices.Contains(names, "gpu-model") {
			t.Fatalf("stale pruned more than the roaming cell: %v", names)
		}
	})

	t.Run("always_on holds on stale", func(t *testing.T) {
		s, probe := newServingFleet(t)
		markStale(t, s, "gpu")
		names := probe.waitPass(t, "render after always_on stale", func(n []string) bool {
			return allDefs(n)
		})
		if !allDefs(names) {
			t.Fatalf("always_on cell pruned on stale (must hold): %v", names)
		}
		// Held means unchanged content: the pass wrote nothing and the
		// counter stays frozen at the initial write.
		probe.assertSilent(t, 100*time.Millisecond, "held cell must not churn the config")
		if got := s.RenderCount(); got != 1 {
			t.Fatalf("RenderCount = %d after held renders, want 1", got)
		}
	})
}

// TestRenderLoopReaddHysteresis pins the slow re-add: a pruned roaming
// cell returns only after MinHealthyStreak consecutive fresh announces.
func TestRenderLoopReaddHysteresis(t *testing.T) {
	cells, hosts, probe := threeCellFleet(t)
	s := renderLoopFixture(t, cells, hosts, RenderLoopConfig{
		FullWaveTimeout: 30 * time.Second, RenderMinInterval: time.Millisecond,
		MinHealthyStreak: 3,
	}, probe)
	rlAnnounce(t, s, "front", rlServing(), nil)
	rlAnnounce(t, s, "gpu", rlServing(), nil)
	rlAnnounce(t, s, "laptop", rlServing(), nil)
	probe.waitPass(t, "initial render", allDefs)

	rlAnnounce(t, s, "laptop", &AnnounceIntent{State: "withdrawing", Since: time.Now()}, nil)
	probe.waitPass(t, "prune render", func(n []string) bool { return !slices.Contains(n, "laptop-model") })

	// Two fresh serving announces clear the withdrawn flag but not the
	// streak threshold: passes still exclude the cell.
	pruned := func(n []string) bool { return !slices.Contains(n, "laptop-model") }
	rlAnnounce(t, s, "laptop", rlServing(), nil)
	probe.waitPass(t, "render at streak 1", pruned)
	rlAnnounce(t, s, "laptop", rlServing(), nil)
	probe.waitPass(t, "render at streak 2 (still pruned)", pruned)

	// The third consecutive fresh announce crosses MinHealthyStreak.
	rlAnnounce(t, s, "laptop", rlServing(), nil)
	names := probe.waitPass(t, "re-add render at streak 3", allDefs)
	if !allDefs(names) {
		t.Fatalf("cell not re-added after MinHealthyStreak announces: %v", names)
	}
}

// TestRenderLoopRenderCap pins the trailing-edge throttle: a burst of
// triggers inside the window coalesces into a single render, and
// RenderCount counts writes, not passes.
func TestRenderLoopRenderCap(t *testing.T) {
	cells, hosts, probe := threeCellFleet(t)
	s := renderLoopFixture(t, cells, hosts, RenderLoopConfig{
		FullWaveTimeout: 30 * time.Second, RenderMinInterval: 200 * time.Millisecond,
	}, probe)
	rlAnnounce(t, s, "front", rlServing(), nil)
	rlAnnounce(t, s, "gpu", rlServing(), nil)
	rlAnnounce(t, s, "laptop", rlServing(), nil)
	probe.waitPass(t, "initial render", allDefs)
	probe.waitWrite(t, "initial write")
	if got := s.RenderCount(); got != 1 {
		t.Fatalf("RenderCount = %d after first write, want 1", got)
	}

	probe.drain()
	for i := 0; i < 10; i++ {
		s.noteRenderTrigger("laptop")
	}
	// Exactly one trailing render when the window expires — then silence.
	probe.waitPass(t, "coalesced trailing render", func(n []string) bool { return true })
	probe.assertSilent(t, 450*time.Millisecond, "burst must coalesce to <=1 render per interval")
	// Content never changed, so the trailing render wrote nothing and
	// the counter reflects writes only.
	if got := s.RenderCount(); got != 1 {
		t.Fatalf("RenderCount = %d after unchanged re-render, want 1 (writes only)", got)
	}
}

// TestRenderLoopUnchangedNoWrite pins the -watch-config churn guard:
// identical render output never touches the file.
func TestRenderLoopUnchangedNoWrite(t *testing.T) {
	cells, hosts, probe := threeCellFleet(t)
	s := renderLoopFixture(t, cells, hosts, RenderLoopConfig{
		FullWaveTimeout: 30 * time.Second, RenderMinInterval: time.Millisecond,
	}, probe)
	rlAnnounce(t, s, "front", rlServing(), nil)
	rlAnnounce(t, s, "gpu", rlServing(), nil)
	rlAnnounce(t, s, "laptop", rlServing(), nil)
	probe.waitPass(t, "initial render", allDefs)
	probe.waitWrite(t, "initial write")

	// A genuine transition that doesn't change the rendered content
	// (always_on stale is held) still runs a pass — but no write.
	markStale(t, s, "gpu")
	probe.waitPass(t, "held re-render", func(n []string) bool { return true })
	probe.assertSilent(t, 100*time.Millisecond, "unchanged content must not write")
	if got := s.RenderCount(); got != 1 {
		t.Fatalf("RenderCount = %d, want 1 (unchanged content is not a write)", got)
	}

	// Sanity: when the output DOES change, the write lands.
	probe.setVersion("v2")
	s.noteRenderTrigger("gpu")
	probe.waitWrite(t, "write after content change")
	if got := s.RenderCount(); got != 2 {
		t.Fatalf("RenderCount = %d after changed render, want 2", got)
	}
}

// TestRenderLoopFingerprint covers C3 §5: mismatches always publish the
// loud event (with defs provenance); strict excludes the def from the
// render, advisory keeps it.
// The cross-cell canary: an announce from a cell that does NOT own a
// strict def, carrying a garbage hash for it, must NOT exclude it or
// even raise the event — enforcement is bound to the def's home cell,
// or any registered name could yank any strict def from the render.
func TestRenderLoopFingerprintCrossCellCannotYank(t *testing.T) {
	strictDef := llmDef("embed-x", "gpu", "strict")
	unassignedDef := llmDef("chat-z", "", "strict")
	cells := []Cell{
		{Name: "front", URL: "http://127.0.0.1:1", Class: "always_on"},
		{Name: "gpu", URL: "http://127.0.0.1:3", Class: "always_on"},
		{Name: "laptop", URL: "http://127.0.0.1:4", Class: "roaming"},
	}
	hosts := &fleetcfg.File{Cells: map[string]fleetcfg.Cell{
		"front":  {URL: "http://127.0.0.1:1", Class: fleetcfg.ClassAlwaysOn},
		"gpu":    {URL: "http://127.0.0.1:3", Class: fleetcfg.ClassAlwaysOn},
		"laptop": {URL: "http://127.0.0.1:4", Class: fleetcfg.ClassRoaming},
	}}
	probe := newRenderProbe(strictDef, unassignedDef)

	cfg := RenderLoopConfig{FullWaveTimeout: 30 * time.Second, RenderMinInterval: time.Millisecond}
	cfg.Hosts = hosts
	cfg.FrontConfigPath = filepath.Join(t.TempDir(), "front.yaml")
	cfg.LoadDefs = probe.loadDefs
	cfg.Render = probe.render
	cfg.WriteFile = probe.writeFile
	s := New(cells, filepath.Join(t.TempDir(), "hist.json"), testDaemonInfo, Options{})
	t.Cleanup(s.Close)
	s.StartRenderLoop(cfg)

	// The owning cell announces healthily; the cross-cell announce
	// carries a garbage hash for gpu's strict def and for an unassigned
	// strict def. Neither must exclude or fire.
	rlAnnounce(t, s, "front", rlServing(), nil)
	rlAnnounce(t, s, "gpu", rlServing(), nil)
	garbage := strings.Repeat("0", 64)
	rlAnnounce(t, s, "laptop", rlServing(), []AnnounceModel{
		{ID: "embed-x", State: "ready", FlagsSHA256: garbage},
		{ID: "chat-z", State: "ready", FlagsSHA256: garbage},
	})

	names := probe.waitPass(t, "render ignores cross-cell hashes", func(n []string) bool {
		return slices.Contains(n, "embed-x") && slices.Contains(n, "chat-z")
	})
	_ = names
}

func TestRenderLoopFingerprint(t *testing.T) {
	strictDef := llmDef("embed-x", "gpu", "strict")
	advisoryDef := llmDef("chat-y", "gpu", "")
	cells := []Cell{
		{Name: "front", URL: "http://127.0.0.1:1", Class: "always_on"},
		{Name: "gpu", URL: "http://127.0.0.1:3", Class: "always_on"},
	}
	hosts := &fleetcfg.File{Cells: map[string]fleetcfg.Cell{
		"front": {URL: "http://127.0.0.1:1", Class: fleetcfg.ClassAlwaysOn},
		"gpu":   {URL: "http://127.0.0.1:3", Class: fleetcfg.ClassAlwaysOn},
	}}
	probe := newRenderProbe(strictDef, advisoryDef)

	cfg := RenderLoopConfig{FullWaveTimeout: 30 * time.Second, RenderMinInterval: time.Millisecond}
	cfg.Hosts = hosts
	cfg.FrontConfigPath = filepath.Join(t.TempDir(), "front.yaml")
	cfg.LoadDefs = probe.loadDefs
	cfg.Render = probe.render
	cfg.WriteFile = probe.writeFile
	s := New(cells, filepath.Join(t.TempDir(), "hist.json"), testDaemonInfo, Options{})
	t.Cleanup(s.Close)

	// The mismatch event must be observable on the fleet stream, so the
	// subscriber attaches before any announce can trigger a render.
	mux := http.NewServeMux()
	s.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch := subscribeEvents(t, ctx, ts)

	s.StartRenderLoop(cfg)

	// The expected hash is fleetd's own render of the def — the test
	// derives it through the same production helpers, so only a real
	// computation bug (not a hardcoded value) can pass silently.
	strictCmd, err := router.ModelCmd(strictDef, router.Options{Hosts: hosts})
	if err != nil {
		t.Fatal(err)
	}
	strictExpected, err := router.FlagsSHA256(strictCmd)
	if err != nil {
		t.Fatal(err)
	}
	wrong := strings.Repeat("0", 64)
	if wrong == strictExpected {
		t.Fatal("hash collision with the canary value")
	}

	rlAnnounce(t, s, "front", rlServing(), nil)
	s.recordAnnounce(&AnnounceRequest{
		V: AnnounceVersion, Cell: "gpu", Seq: 1, Intent: rlServing(),
		Models: []AnnounceModel{
			{ID: "embed-x", State: "ready", FlagsSHA256: wrong},
			{ID: "chat-y", State: "ready", FlagsSHA256: wrong},
		},
		Versions: &AnnounceVersions{DefsSHA: "abc123", DefsDirty: true},
	})

	// Strict: excluded from the render; advisory: kept.
	names := probe.waitPass(t, "render excluding strict mismatch", func(n []string) bool {
		return !slices.Contains(n, "embed-x")
	})
	if !slices.Contains(names, "chat-y") {
		t.Fatalf("advisory mismatch excluded (must stay fail-open): %v", names)
	}

	type payload struct {
		Model     string `json:"model"`
		Cell      string `json:"cell"`
		Expected  string `json:"expected"`
		Got       string `json:"got"`
		Mode      string `json:"mode"`
		DefsSHA   string `json:"defs_sha"`
		DefsDirty bool   `json:"defs_dirty"`
	}
	decode := func(ev Event) payload {
		var p payload
		if err := json.Unmarshal(ev.Data, &p); err != nil {
			t.Fatalf("fingerprint event payload: %v", err)
		}
		return p
	}

	strictEv := waitFor(t, ch, "strict fingerprint event", func(ev Event) bool {
		if ev.Type != EventFingerprint {
			return false
		}
		var p payload
		return json.Unmarshal(ev.Data, &p) == nil && p.Mode == "strict"
	})
	sp := decode(strictEv)
	if sp.Model != "embed-x" || sp.Cell != "gpu" || sp.Expected != strictExpected || sp.Got != wrong {
		t.Fatalf("strict event payload wrong: %+v", sp)
	}
	if sp.DefsSHA != "abc123" || !sp.DefsDirty {
		t.Fatalf("strict event missing defs provenance: %+v", sp)
	}

	advisoryEv := waitFor(t, ch, "advisory fingerprint event", func(ev Event) bool {
		if ev.Type != EventFingerprint {
			return false
		}
		var p payload
		return json.Unmarshal(ev.Data, &p) == nil && p.Mode == "advisory"
	})
	ap := decode(advisoryEv)
	if ap.Model != "chat-y" || ap.Expected == "" || ap.Got != wrong {
		t.Fatalf("advisory event payload wrong: %+v", ap)
	}
}

// TestRenderLoopNeverAnnouncedRoaming pins the probe-era rule: presence
// policy applies only once presence exists. A roaming cell that has
// never announced keeps its defs (config truth stands).
func TestRenderLoopNeverAnnouncedRoaming(t *testing.T) {
	cells := []Cell{
		{Name: "front", URL: "http://127.0.0.1:1", Class: "always_on"},
		{Name: "laptop", URL: "http://127.0.0.1:2", Class: "roaming"},
		{Name: "ghost", URL: "http://127.0.0.1:4", Class: "roaming"},
	}
	hosts := &fleetcfg.File{Cells: map[string]fleetcfg.Cell{
		"front":  {URL: "http://127.0.0.1:1", Class: fleetcfg.ClassAlwaysOn},
		"laptop": {URL: "http://127.0.0.1:2", Class: fleetcfg.ClassRoaming},
		"ghost":  {URL: "http://127.0.0.1:4", Class: fleetcfg.ClassRoaming},
	}}
	probe := newRenderProbe(
		llmDef("laptop-model", "laptop", ""),
		llmDef("ghost-model", "ghost", ""),
	)
	// The ghost never announces, so the full wave can never complete —
	// the hold timeout is what releases the first render.
	s := renderLoopFixture(t, cells, hosts, RenderLoopConfig{
		FullWaveTimeout: 150 * time.Millisecond, RenderMinInterval: time.Millisecond,
	}, probe)
	rlAnnounce(t, s, "front", rlServing(), nil)
	rlAnnounce(t, s, "laptop", rlServing(), nil)

	names := probe.waitPass(t, "render after hold timeout", func(n []string) bool {
		return slices.Contains(n, "laptop-model")
	})
	if !slices.Contains(names, "ghost-model") {
		t.Fatalf("never-announced roaming cell pruned (config truth must stand): %v", names)
	}

	// Withdraw the ANNOUNCED roaming cell: pruned. The ghost still holds.
	rlAnnounce(t, s, "laptop", &AnnounceIntent{State: "withdrawing", Since: time.Now()}, nil)
	names = probe.waitPass(t, "render after laptop withdraw", func(n []string) bool {
		return !slices.Contains(n, "laptop-model")
	})
	if !slices.Contains(names, "ghost-model") {
		t.Fatalf("never-announced roaming cell pruned alongside withdrawn one: %v", names)
	}
}
