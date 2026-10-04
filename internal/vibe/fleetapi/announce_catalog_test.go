// fleet-control (post-C27): the front peer list intersects with the
// announced catalog. These tests run the REAL renderer through the
// presence loop against the heavy-rotation shape that motivated the gate:
// three defs on one cell, of which the cell announces exactly one. Before
// the gate, the front advertised all three and two of them 404'd at the
// cell on request.
package fleetapi

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
	"github.com/gallowaysoftware/vibe/internal/vibe/router"
)

// heavyDef builds the def shape the heavy cell actually ships: an
// externally managed vllm engine (the DSpark/GLM recipes) pinned to its
// cell, plus one plain llama_server def to prove the gate is kind-blind.
func heavyDef(name string) *profile.BackendDef {
	return &profile.BackendDef{
		Name: name,
		Cell: "heavy",
		// External mirrors what LoadBackendFrom's normalize() forces for
		// every def that reaches a real renderer; these are built inline.
		Backend: profile.Backend{
			External: true,
			VLLM:     &profile.VLLMBackend{URL: "http://127.0.0.1:8888"},
		},
	}
}

func gpuDef(name string) *profile.BackendDef {
	return &profile.BackendDef{
		Name: name,
		Cell: "gpu",
		Backend: profile.Backend{
			External:    true,
			LlamaServer: &profile.LlamaServerBackend{Path: "/models/" + name + ".gguf"},
		},
	}
}

func rotationDefs() []*profile.BackendDef {
	return []*profile.BackendDef{
		heavyDef("dsv4flash"),
		heavyDef("glm53flash"),
		heavyDef("qwen38fn"),
		gpuDef("gpu-coder"),
	}
}

func catalogFleet(t *testing.T, defs []*profile.BackendDef, wave time.Duration) (*Server, *c21Writes) {
	t.Helper()
	cells := []Cell{
		{Name: "front", URL: "http://127.0.0.1:1", Class: "always_on"},
		{Name: "heavy", URL: "http://127.0.0.1:4", Class: "always_on"},
		{Name: "gpu", URL: "http://127.0.0.1:3", Class: "always_on"},
	}
	hosts := &fleetcfg.File{Cells: map[string]fleetcfg.Cell{
		"front": {URL: "http://127.0.0.1:1", Class: fleetcfg.ClassAlwaysOn},
		"heavy": {URL: "http://127.0.0.1:4", Class: fleetcfg.ClassAlwaysOn},
		"gpu":   {URL: "http://127.0.0.1:3", Class: fleetcfg.ClassAlwaysOn},
	}}
	w := &c21Writes{ch: make(chan string, 64)}
	s := New(cells, filepath.Join(t.TempDir(), "hist.json"), testDaemonInfo, Options{})
	t.Cleanup(s.Close)
	s.StartRenderLoop(RenderLoopConfig{
		Hosts:             hosts,
		FrontConfigPath:   filepath.Join(t.TempDir(), "front.yaml"),
		FullWaveTimeout:   wave,
		RenderMinInterval: time.Millisecond,
		LoadDefs:          func(string) ([]*profile.BackendDef, error) { return defs, nil },
		Render:            router.Render,
		WriteFile:         w.writeFile,
	})
	return s, w
}

// catalogAnnounce is one heartbeat carrying an explicit catalog-read
// verdict. known=false models the announcer whose llama-swap did not
// answer gatherModels; the announce still goes out (presence is heartbeat
// evidence) but claims nothing about the catalog.
func catalogAnnounce(t *testing.T, s *Server, cell string, seq uint64, known bool, ids ...string) {
	t.Helper()
	req := &AnnounceRequest{
		V:           AnnounceVersion,
		Cell:        cell,
		Seq:         seq,
		Intent:      rlServing(),
		ModelsKnown: known,
	}
	for _, id := range ids {
		req.Models = append(req.Models, AnnounceModel{ID: id, State: "stopped"})
	}
	s.recordAnnounce(req)
}

// TestRenderLoop_FrontCatalogFollowsTheAnnouncedCatalog: the cell says it
// serves glm53flash, so glm53flash is the only heavy id the front
// advertises — no matter how many heavy defs the shared checkout carries.
func TestRenderLoop_FrontCatalogFollowsTheAnnouncedCatalog(t *testing.T) {
	s, w := catalogFleet(t, rotationDefs(), 30*time.Second)
	catalogAnnounce(t, s, "front", 1, true)
	catalogAnnounce(t, s, "gpu", 1, true, "gpu-coder")
	catalogAnnounce(t, s, "heavy", 1, true, "glm53flash")

	out := w.await(t, "advertises only the announced heavy id", func(out string) bool {
		ids := c21IDs(t, out)
		_, hasDSV := ids["dsv4flash"]
		_, hasQwen := ids["qwen38fn"]
		return ids["glm53flash"] == "heavy" && !hasDSV && !hasQwen && ids["gpu-coder"] == "gpu"
	})
	if models := c21Parse(t, out).Peers["heavy"].Models; len(models) != 1 || models[0] != "glm53flash" {
		t.Errorf("heavy peer models = %v, want exactly [glm53flash]:\n%s", models, out)
	}
}

// TestRenderLoop_UnreadCatalogHoldsTheDefs: the cell's llama-swap did not
// answer its announcer, so the announce carries presence but claims
// NOTHING about the catalog. Holding is the only honest reading — pruning
// would act on absence of evidence, and a llama-swap blip would flap the
// whole front catalog.
func TestRenderLoop_UnreadCatalogHoldsTheDefs(t *testing.T) {
	s, w := catalogFleet(t, rotationDefs(), 30*time.Second)
	catalogAnnounce(t, s, "front", 1, true)
	catalogAnnounce(t, s, "gpu", 1, true, "gpu-coder")
	catalogAnnounce(t, s, "heavy", 1, false)

	out := w.await(t, "holds every heavy def while the catalog is unread", func(out string) bool {
		ids := c21IDs(t, out)
		return ids["dsv4flash"] == "heavy" && ids["glm53flash"] == "heavy" && ids["qwen38fn"] == "heavy"
	})
	_ = out
}

// TestRenderLoop_KnownEmptyCatalogPrunesTheCell: a READ catalog that lists
// nothing is a positive claim — the cell serves nothing, so its defs have
// no business in the front peer list.
func TestRenderLoop_KnownEmptyCatalogPrunesTheCell(t *testing.T) {
	s, w := catalogFleet(t, rotationDefs(), 30*time.Second)
	catalogAnnounce(t, s, "front", 1, true)
	catalogAnnounce(t, s, "gpu", 1, true, "gpu-coder")
	catalogAnnounce(t, s, "heavy", 1, true)

	out := w.await(t, "prunes the cell whose read catalog is empty", func(out string) bool {
		_, hasHeavy := c21Parse(t, out).Peers["heavy"]
		_, hasGPU := c21IDs(t, out)["gpu-coder"]
		return !hasHeavy && hasGPU
	})
	_ = out
}

// TestRenderLoop_NeverAnnouncedCellHoldsItsDefs: presence-derived
// rendering only applies once presence exists (the same rule
// applyClassPolicy states for the roaming prune). A cell that has never
// announced is a fleet whose table is not whole; its defs stay rendered
// until the wave completes and its announces arrive.
func TestRenderLoop_NeverAnnouncedCellHoldsItsDefs(t *testing.T) {
	s, w := catalogFleet(t, rotationDefs(), 50*time.Millisecond)
	catalogAnnounce(t, s, "front", 1, true)
	catalogAnnounce(t, s, "gpu", 1, true, "gpu-coder")
	// heavy never announces; the cold-start hold lapses via the short
	// FullWaveTimeout and the pass still renders every heavy def.

	out := w.await(t, "renders the never-announced cell's defs", func(out string) bool {
		ids := c21IDs(t, out)
		return ids["dsv4flash"] == "heavy" && ids["gpu-coder"] == "gpu"
	})
	_ = out
	_ = s
}

// TestRenderLoop_RotationFollowsTheAnnounce: the model-set-change render
// trigger carries a rotation end to end — the retired id leaves the front
// and the new one arrives, with no fleetd-side def edit.
func TestRenderLoop_RotationFollowsTheAnnounce(t *testing.T) {
	s, w := catalogFleet(t, rotationDefs(), 30*time.Second)
	catalogAnnounce(t, s, "front", 1, true)
	catalogAnnounce(t, s, "gpu", 1, true, "gpu-coder")
	catalogAnnounce(t, s, "heavy", 1, true, "glm53flash")
	w.await(t, "serves glm53flash", func(out string) bool {
		return c21IDs(t, out)["glm53flash"] == "heavy"
	})

	catalogAnnounce(t, s, "heavy", 2, true, "dsv4flash")

	// The model-set-change trigger fires ONE pass whose write carries both
	// halves of the transition, and no further write follows — so the
	// assertion is a single await over both, never two sequential awaits
	// (the second would wait on a write that never comes).
	after := w.await(t, "swaps the rotated ids", func(out string) bool {
		ids := c21IDs(t, out)
		_, gone := ids["glm53flash"]
		return ids["dsv4flash"] == "heavy" && !gone
	})
	if ids := c21IDs(t, after); ids["qwen38fn"] != "" {
		t.Errorf("the rotation set member that was never announced still renders: %v\n%s", ids, after)
	}
}

// TestRenderLoop_GateExclusionTakesTheAliasOutOfTheCatalog: a def the gate
// drops owned an alias. The alias must leave the front catalog — not fall
// to a co-claimant on another cell (C21: an exclusion removes an id, it
// never re-points one).
func TestRenderLoop_GateExclusionTakesTheAliasOutOfTheCatalog(t *testing.T) {
	// A llama_server def owns the alias: a vllm def cannot — its ids are
	// reserved canonical ids and router: is a load error on the kind.
	owner := gpuDef("rotated-out")
	owner.Cell = "heavy"
	owner.Router = &profile.RouterOpts{Aliases: []string{"best-coder"}, AliasOwner: true}
	defs := []*profile.BackendDef{
		owner,
		heavyDef("glm53flash"),
		func() *profile.BackendDef {
			d := gpuDef("gpu-coder")
			d.Router = &profile.RouterOpts{Aliases: []string{"best-coder"}}
			return d
		}(),
	}
	s, w := catalogFleet(t, defs, 30*time.Second)
	catalogAnnounce(t, s, "front", 1, true)
	catalogAnnounce(t, s, "gpu", 1, true, "gpu-coder")
	catalogAnnounce(t, s, "heavy", 1, true, "glm53flash")

	after := w.await(t, "drops the unannounced owner", func(out string) bool {
		_, ok := c21IDs(t, out)["rotated-out"]
		return !ok
	})
	if ids := c21IDs(t, after); ids["best-coder"] != "" {
		t.Errorf("REPOINT: the gate-dropped owner's alias landed on cell %q — an exclusion must remove an alias from the catalog, never move it:\n%s", ids["best-coder"], after)
	}
}

// TestRenderLoop_NamespacedCatalogIdKeepsTheDef: a cell's llama-swap
// namespaces its OWN peer ids as peerID/model (drift's MTPLX stack
// announces "qwen38fn-mlx/qwen38fn-mlx"). The gate must match the base of
// a namespaced id, or the cell's def is pruned from the front on its first
// known catalog and a client-facing id the front advertised 404s with "no
// router" — the live defect of 2026-10-04. Paired with the prune case
// below: same shape, id genuinely gone.
func TestRenderLoop_NamespacedCatalogIdKeepsTheDef(t *testing.T) {
	s, w := catalogFleet(t, rotationDefs(), 30*time.Second)
	catalogAnnounce(t, s, "front", 1, true)
	catalogAnnounce(t, s, "gpu", 1, true, "gpu-coder")
	catalogAnnounce(t, s, "heavy", 1, true, "heavy/glm53flash")

	out := w.await(t, "keeps the def behind a namespaced catalog id", func(out string) bool {
		models := c21Parse(t, out).Peers["heavy"].Models
		return len(models) > 0 && models[0] == "glm53flash"
	})
	if models := c21Parse(t, out).Peers["heavy"].Models; len(models) != 1 {
		t.Errorf("heavy peer models = %v, want [glm53flash] only:\n%s", models, out)
	}
}
