package fleetapi

// fleet-control C21, adversarial review. Three properties of renderPass
// the feature commit left unpinned: the fingerprint overlay is the OTHER
// exclusion C21 claims to fix and nothing exercised it; a collision error
// must not take C9's drift evaluation down with it; and an alias that
// leaves the catalog has to be readable somewhere.

import (
	"bytes"
	"log/slog"
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

// c21rDef is c21Def plus the fingerprint mode, so a def can be both an
// alias claimant and a strict-enforcement target.
func c21rDef(name, cell, fingerprint string, owner bool, aliases ...string) *profile.BackendDef {
	d := c21Def(name, cell, owner, aliases...)
	d.Fingerprint = fingerprint
	return d
}

// c21rFleet is c21Fleet with the third cell always_on rather than
// roaming: the exclusion under test is the fingerprint one, and a roaming
// cell would confound it with the prune.
func c21rFleet(t *testing.T, defs []*profile.BackendDef) (*Server, *c21Writes, *fleetcfg.File) {
	t.Helper()
	cells := []Cell{
		{Name: "front", URL: "http://127.0.0.1:1", Class: "always_on"},
		{Name: "gpu", URL: "http://127.0.0.1:2", Class: "always_on"},
		{Name: "bravo", URL: "http://127.0.0.1:3", Class: "always_on"},
	}
	hosts := &fleetcfg.File{Cells: map[string]fleetcfg.Cell{
		"front": {URL: "http://127.0.0.1:1", Class: fleetcfg.ClassAlwaysOn},
		"gpu":   {URL: "http://127.0.0.1:2", Class: fleetcfg.ClassAlwaysOn},
		"bravo": {URL: "http://127.0.0.1:3", Class: fleetcfg.ClassAlwaysOn},
	}}
	w := &c21Writes{ch: make(chan string, 64)}
	s := New(cells, filepath.Join(t.TempDir(), "hist.json"), testDaemonInfo, Options{})
	t.Cleanup(s.Close)
	s.StartRenderLoop(RenderLoopConfig{
		Hosts:             hosts,
		FrontConfigPath:   filepath.Join(t.TempDir(), "front.yaml"),
		FullWaveTimeout:   30 * time.Second,
		RenderMinInterval: time.Millisecond,
		MinHealthyStreak:  3,
		LoadDefs:          func(string) ([]*profile.BackendDef, error) { return defs, nil },
		Render:            router.Render,
		WriteFile:         w.writeFile,
	})
	return s, w, hosts
}

// TestRenderLoop_StrictFingerprintExclusionDoesNotTransferTheAlias is C21
// §6's trigger 2, which the phase claims to fix and never exercised. The
// strict-mode owner is excluded from the render for drifted flags; its
// alias must leave the catalog with it rather than falling to the
// co-claimant on the other cell — the mechanism that exists to stop
// silent retrieval damage would otherwise cause a silent repoint.
func TestRenderLoop_StrictFingerprintExclusionDoesNotTransferTheAlias(t *testing.T) {
	owner := c21rDef("embed-gpu", "gpu", "strict", true, "best-embed")
	rival := c21rDef("embed-bravo", "bravo", "", false, "best-embed")
	s, w, _ := c21rFleet(t, []*profile.BackendDef{owner, rival})

	rlAnnounce(t, s, "front", rlServing(), nil)
	rlAnnounce(t, s, "bravo", rlServing(), nil)
	// The drifted hash: 64 zeroes is never what ModelCmd+FlagsSHA256 yields.
	rlAnnounce(t, s, "gpu", rlServing(), []AnnounceModel{
		{ID: "embed-gpu", State: "ready", FlagsSHA256: strings.Repeat("0", 64)},
	})

	out := w.await(t, "has excluded the strict-mismatched def", func(out string) bool {
		_, ok := c21IDs(t, out)["embed-gpu"]
		return !ok
	})
	ids := c21IDs(t, out)
	if got, ok := ids["best-embed"]; ok {
		t.Errorf("REPOINT: the strict exclusion handed best-embed to cell %q:\n%s", got, out)
	}
	if ids["embed-bravo"] != "bravo" {
		t.Errorf("the co-claimant stopped serving its own id: ids = %v\n%s", ids, out)
	}
}

// TestRenderLoop_AliasCollisionStillEvaluatesTheFingerprintSet: the
// collision error aborts the pass, and applyFingerprints is the ONLY
// thing that evaluates C9's persistent-drift set. Resolving aliases
// before the overlays meant one unresolvable alias froze that set on its
// last value forever while notify's fingerprint_source kept reporting the
// evaluator as live — a repaired mismatch pages forever, a new one is
// never seen.
func TestRenderLoop_AliasCollisionStillEvaluatesTheFingerprintSet(t *testing.T) {
	// Two claimants, no alias_owner: every pass fails at resolution.
	drifted := c21rDef("embed-gpu", "gpu", "strict", false, "contested")
	rival := c21rDef("embed-bravo", "bravo", "", false, "contested")
	s, w, _ := c21rFleet(t, []*profile.BackendDef{drifted, rival})

	rlAnnounce(t, s, "front", rlServing(), nil)
	rlAnnounce(t, s, "bravo", rlServing(), nil)
	rlAnnounce(t, s, "gpu", rlServing(), []AnnounceModel{
		{ID: "embed-gpu", State: "ready", FlagsSHA256: strings.Repeat("0", 64)},
	})

	waitUntil(t, func() bool { return len(s.FingerprintMismatches()) == 1 })
	if got := s.FingerprintMismatches()[0]; got.Cell != "gpu" || got.Model != "embed-gpu" {
		t.Fatalf("mismatch = %+v", got)
	}
	// And the collision still refuses to render anything.
	w.silent(t, 200*time.Millisecond, "an unresolvable collision must render nothing")
	if got := s.RenderCount(); got != 0 {
		t.Fatalf("RenderCount = %d, want 0", got)
	}
}

// logCapture swaps the default slog handler for the duration of a test.
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *logCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func captureLogs(t *testing.T) *logCapture {
	t.Helper()
	c := &logCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(c, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return c
}

// TestRenderLoop_NamesTheAliasThatLeftTheCatalog: the prune's own line
// names a CELL. An operator whose harness is pinned to `best-coder` sees
// "pruning roaming cell laptop", a co-claimant's box that is still up,
// and an id that stopped resolving — with nothing connecting the three.
// C21's whole argument is that a mechanism is loud in proportion to what
// a reader can act on, so the pass names the ids it removed.
func TestRenderLoop_NamesTheAliasThatLeftTheCatalog(t *testing.T) {
	logs := captureLogs(t)
	s, w := c21Fleet(t, []*profile.BackendDef{
		c21Def("laptop-coder", "laptop", true, "best-coder"),
		c21Def("gpu-coder", "gpu", false, "best-coder"),
	})
	w.await(t, "carries the laptop's models", func(out string) bool {
		_, ok := c21IDs(t, out)["laptop-coder"]
		return ok
	})
	if got := logs.String(); strings.Contains(got, "alias left with its declared owner") {
		t.Fatalf("a steady fleet must not log an orphaned alias:\n%s", got)
	}

	markStale(t, s, "laptop")
	w.await(t, "has pruned the laptop", func(out string) bool {
		_, ok := c21IDs(t, out)["laptop-coder"]
		return !ok
	})
	got := logs.String()
	for _, want := range []string{"alias left with its declared owner", "best-coder", "laptop-coder"} {
		if !strings.Contains(got, want) {
			t.Errorf("the prune said nothing about %q leaving the catalog:\n%s", want, got)
		}
	}
}

// TestRenderLoop_UnaliasedPruneStaysQuiet is the inert half of the check
// above: the orphan warning must key on the def's WINNING aliases, not on
// "a def was dropped", or every roaming prune grows a line about nothing.
func TestRenderLoop_UnaliasedPruneStaysQuiet(t *testing.T) {
	logs := captureLogs(t)
	s, w := c21Fleet(t, []*profile.BackendDef{
		c21Def("laptop-coder", "laptop", false),
		c21Def("gpu-coder", "gpu", true, "best-coder"),
	})
	w.await(t, "carries the laptop's models", func(out string) bool {
		_, ok := c21IDs(t, out)["laptop-coder"]
		return ok
	})
	markStale(t, s, "laptop")
	after := w.await(t, "has pruned the laptop", func(out string) bool {
		_, ok := c21IDs(t, out)["laptop-coder"]
		return !ok
	})
	if got := logs.String(); strings.Contains(got, "alias left with its declared owner") {
		t.Errorf("pruning a def that owns no alias must log nothing about aliases:\n%s", got)
	}
	// And the surviving owner keeps its alias through the prune.
	if got := c21IDs(t, after)["best-coder"]; got != "gpu" {
		t.Errorf("best-coder resolves to %q, want gpu:\n%s", got, after)
	}
	if got := c21Parse(t, after).Peers["gpu"].Models; !slices.Equal(got, []string{"gpu-coder", "best-coder"}) {
		t.Errorf("gpu peer models = %v, want [gpu-coder best-coder]:\n%s", got, after)
	}
}
