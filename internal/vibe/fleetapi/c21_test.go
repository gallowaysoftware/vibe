package fleetapi

import (
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
	"github.com/gallowaysoftware/vibe/internal/vibe/router"
)

// c21Catalog is the front's rendered catalog reduced to the only thing
// these tests are about: which ids each peer answers to.
type c21Catalog struct {
	Peers map[string]struct {
		Models []string `yaml:"models"`
	} `yaml:"peers"`
}

func c21Parse(t *testing.T, out string) c21Catalog {
	t.Helper()
	var c c21Catalog
	if err := yaml.Unmarshal([]byte(out), &c); err != nil {
		t.Fatalf("rendered front config is not YAML: %v\n%s", err, out)
	}
	return c
}

// c21IDs is every catalog id the front serves, with the cell behind it.
func c21IDs(t *testing.T, out string) map[string]string {
	t.Helper()
	ids := map[string]string{}
	for cell, p := range c21Parse(t, out).Peers {
		for _, m := range p.Models {
			ids[m] = cell
		}
	}
	return ids
}

// fleet-control C21. The visible-repoint alias tier is REJECTED, and these
// run the REAL renderer through the presence loop to pin the rejection
// where the loop could reach it: an alias whose owning cell is pruned
// leaves the front catalog. It does not quietly start naming a model on
// another cell, which is what the shipped code did.

// c21Writes records every front config the loop writes, with the real
// router.Render in the seam so the assertion is about catalog CONTENT
// rather than about which def names a fake renderer was handed.
type c21Writes struct {
	mu   sync.Mutex
	ch   chan string
	path string
}

func (w *c21Writes) writeFile(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	w.mu.Lock()
	w.path = path
	w.mu.Unlock()
	w.ch <- string(data)
	return nil
}

func (w *c21Writes) await(t *testing.T, what string, match func(string) bool) string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case got := <-w.ch:
			if match(got) {
				return got
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a front config that %s", what)
		}
	}
}

func (w *c21Writes) silent(t *testing.T, dur time.Duration, what string) {
	t.Helper()
	select {
	case got := <-w.ch:
		t.Fatalf("%s: unexpected write:\n%s", what, got)
	case <-time.After(dur):
	}
}

func c21Def(name, cell string, owner bool, aliases ...string) *profile.BackendDef {
	return &profile.BackendDef{
		Name:   name,
		Cell:   cell,
		Router: &profile.RouterOpts{Aliases: aliases, AliasOwner: owner},
		Backend: profile.Backend{
			External:    true,
			LlamaServer: &profile.LlamaServerBackend{Path: "/models/" + name + ".gguf"},
		},
	}
}

// c21Fleet is the roaming-best-node fleet from the futures doc: the good
// coder model lives on the laptop, a second one lives on the gpu box, and
// both defs claim the same alias with the laptop declared owner.
func c21Fleet(t *testing.T, defs []*profile.BackendDef) (*Server, *c21Writes) {
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
	rlAnnounce(t, s, "front", rlServing(), nil)
	rlAnnounce(t, s, "gpu", rlServing(), nil)
	rlAnnounce(t, s, "laptop", rlServing(), nil)
	return s, w
}

func TestRenderLoop_PrunedRoamingOwnerTakesItsAliasOutOfTheCatalog(t *testing.T) {
	s, w := c21Fleet(t, []*profile.BackendDef{
		c21Def("laptop-coder", "laptop", true, "best-coder"),
		c21Def("gpu-coder", "gpu", false, "best-coder"),
	})

	first := w.await(t, "carries the laptop's models", func(out string) bool {
		_, ok := c21IDs(t, out)["laptop-coder"]
		return ok
	})
	if got := c21IDs(t, first)["best-coder"]; got != "laptop" {
		t.Fatalf("best-coder resolves to cell %q, want the declared owner's cell laptop:\n%s", got, first)
	}

	markStale(t, s, "laptop")

	after := w.await(t, "has pruned the laptop", func(out string) bool {
		_, ok := c21IDs(t, out)["laptop-coder"]
		return !ok
	})
	ids := c21IDs(t, after)
	if got, ok := ids["best-coder"]; ok {
		t.Errorf("REPOINT: best-coder outlived its owner's prune and now names a model on cell %q:\n%s", got, after)
	}
	if ids["gpu-coder"] != "gpu" {
		t.Errorf("the prune took more than the roaming cell: ids = %v\n%s", ids, after)
	}
}

// TestRenderLoop_UnresolvableAliasCollisionDoesNotHealByPruning: two
// claimants and no declared owner is a render error, and the error must
// survive the departure of one claimant. Resolving over the survivors made
// the misconfiguration fix ITSELF by handing the alias to whoever was
// left — the error going away was the repoint.
func TestRenderLoop_UnresolvableAliasCollisionDoesNotHealByPruning(t *testing.T) {
	s, w := c21Fleet(t, []*profile.BackendDef{
		c21Def("laptop-coder", "laptop", false, "best-coder"),
		c21Def("gpu-coder", "gpu", false, "best-coder"),
	})

	w.silent(t, 300*time.Millisecond, "an unresolvable collision must render nothing")
	markStale(t, s, "laptop")
	w.silent(t, 300*time.Millisecond, "pruning one claimant must not resolve the collision")

	if got := s.RenderCount(); got != 0 {
		t.Fatalf("RenderCount = %d, want 0 (every pass should have failed)", got)
	}
}

// TestRenderLoop_AliasFollowsTheDeclaredOwnerBackNotThePruneOrder: the
// re-add half. When the owner returns, the alias returns with it and the
// co-claimant still never holds it.
func TestRenderLoop_AliasFollowsTheDeclaredOwnerBackNotThePruneOrder(t *testing.T) {
	s, w := c21Fleet(t, []*profile.BackendDef{
		c21Def("laptop-coder", "laptop", true, "best-coder"),
		c21Def("gpu-coder", "gpu", false, "best-coder"),
	})
	w.await(t, "carries the laptop's models", func(out string) bool {
		_, ok := c21IDs(t, out)["laptop-coder"]
		return ok
	})
	markStale(t, s, "laptop")
	w.await(t, "has pruned the laptop", func(out string) bool {
		_, ok := c21IDs(t, out)["laptop-coder"]
		return !ok
	})

	for i := range 3 {
		s.recordAnnounce(&AnnounceRequest{V: AnnounceVersion, Cell: "laptop", Seq: uint64(2 + i), Intent: rlServing()})
	}
	back := w.await(t, "has re-added the laptop", func(out string) bool {
		_, ok := c21IDs(t, out)["laptop-coder"]
		return ok
	})
	if got := c21IDs(t, back)["best-coder"]; got != "laptop" {
		t.Errorf("after the re-add best-coder resolves to cell %q, want laptop:\n%s", got, back)
	}
	if got := c21Parse(t, back).Peers["gpu"].Models; !slices.Equal(got, []string{"gpu-coder"}) {
		t.Errorf("gpu peer models = %v, want just its own def name:\n%s", got, back)
	}
}
