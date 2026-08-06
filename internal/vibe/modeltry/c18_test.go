package modeltry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/swaptest"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/hfdownload"
	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
	"github.com/gallowaysoftware/vibe/internal/vibe/router"
)

// fleet-control C18. Every test here drives the REAL sequence — the real
// loader, the real renderer, the real prober against a real llama-swap
// double — because the failures this phase can produce are all failures
// of ORDER, and a test that stubs the order proves nothing about it.

const (
	testCell = "gpu-cell"
	incName  = "qwen3.6-27b"
)

// rig is one box: a backends dir, a llama-swap config path, a journal, a
// swaptest cell standing in for this cell's own llama-swap.
type rig struct {
	t          *testing.T
	dir        string
	backends   string
	configPath string
	statePath  string
	weightsDir string
	swap       *swaptest.Cell
	hosts      *fleetcfg.File
	free       int64
	fetched    []hfdownload.Spec
	headSize   int64
	headErr    error
}

func newRig(t *testing.T, opts ...swaptest.Option) *rig {
	t.Helper()
	dir := t.TempDir()
	r := &rig{
		t:          t,
		dir:        dir,
		backends:   filepath.Join(dir, "backends"),
		configPath: filepath.Join(dir, "llama-swap", "config.yaml"),
		statePath:  filepath.Join(dir, "state", "model-trial.json"),
		weightsDir: filepath.Join(dir, "models"),
		free:       500 << 30,
		headSize:   8 << 30,
	}
	if len(opts) == 0 {
		opts = []swaptest.Option{swaptest.WithModels(swaptest.Model{ID: incName, State: "stopped"})}
	}
	r.swap = swaptest.NewCell(t, opts...)
	for _, d := range []string{r.backends, r.weightsDir, filepath.Dir(r.configPath), filepath.Dir(r.statePath)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r.hosts = mustHosts(t, dir)
	r.writeIncumbent()
	return r
}

// mustHosts writes a reference-fleet hosts.yaml (example values only —
// the boundary rule) and parses it through the real loader.
func mustHosts(t *testing.T, dir string) *fleetcfg.File {
	t.Helper()
	path := filepath.Join(dir, "hosts.yaml")
	body := `cells:
  front:
    url: http://front.example.lan:9000
    class: always_on
  ` + testCell + `:
    url: http://gpu.example.lan:9000
    class: opportunistic
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := fleetcfg.LoadFrom(path)
	if err != nil {
		t.Fatalf("load hosts.yaml: %v", err)
	}
	return f
}

func (r *rig) writeIncumbent() {
	r.t.Helper()
	r.writeDef(incName, `name: `+incName+`
cell: `+testCell+`
estimated_vram_gb: 19
lifecycle:
  ttl: 2h
  preload: true
router:
  aliases: [qwen-coder]
  alias_owner: true
fingerprint: strict
backend:
  external: true
  llama_server:
    path: `+filepath.Join(r.weightsDir, "qwen3.6-27b-Q5.gguf")+`
    huggingface:
      repo: unsloth/Qwen3.6-27B-GGUF
      file: Qwen3.6-27B-Q5_K_XL.gguf
    alias: qwen3.6-27b-legacy
    context: 65536
    gpu_layers: 99
    flash_attn: true
    cache_type_k: q8_0
    mmproj: `+filepath.Join(r.weightsDir, "mmproj.gguf")+`
    extra_args: ["--threads", "8"]
`)
}

func (r *rig) writeDef(name, body string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.backends, name+".yaml"), []byte(body), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) runner(mut ...func(*Options)) *Runner {
	r.t.Helper()
	opt := Options{
		Cell:              testCell,
		Hosts:             r.hosts,
		BackendsDir:       r.backends,
		ConfigPath:        r.configPath,
		LlamaServerBinary: "/usr/bin/llama-server",
		LlamaSwapURL:      r.swap.URL(),
		StatePath:         r.statePath,
		ProbeStatePath:    filepath.Join(r.dir, "state", "model-probe.json"),
		CatalogWait:       200 * time.Millisecond,
		DiskFree:          func(string) (int64, error) { return r.free, nil },
		HeadSize: func(context.Context, hfdownload.Spec) (int64, error) {
			return r.headSize, r.headErr
		},
		Fetch: func(_ context.Context, spec hfdownload.Spec, dest string, _ hfdownload.ProgressFunc) error {
			r.fetched = append(r.fetched, spec)
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return err
			}
			return os.WriteFile(dest, make([]byte, r.headSize), 0o644)
		},
	}
	for _, m := range mut {
		m(&opt)
	}
	run, err := New(opt)
	if err != nil {
		r.t.Fatalf("New: %v", err)
	}
	return run
}

func planReq() PlanRequest {
	return PlanRequest{Repo: "unsloth/Qwen3.7-32B-GGUF", File: "Qwen3.7-32B-Q4_K_XL.gguf", Like: incName}
}

// smallRig shrinks the fake file so the tests are not writing gigabytes.
func smallRig(t *testing.T, opts ...swaptest.Option) *rig {
	r := newRig(t, opts...)
	r.headSize = 4096
	return r
}

// ── U1: the trial never becomes fleet catalog ───────────────────────────

// TestFrontRenderExcludesTrialDefsAndNamesThem is the invariant test for
// this phase: the front's peer map is what makes an id routable
// fleet-wide, so a trial in it would be a silent catalog change. The
// same def on its OWN cell's render is included — otherwise the
// exclusion would be indistinguishable from the def not working at all.
func TestFrontRenderExcludesTrialDefsAndNamesThem(t *testing.T) {
	r := smallRig(t)
	trial := deriveDef(mustDef(t, r, incName), "trial-x", filepath.Join(r.weightsDir, "x.gguf"), testCell)
	defs := []*profile.BackendDef{mustDef(t, r, incName), trial}

	var warns []string
	front, err := router.Render(defs, router.Options{
		Cell: fleetcfg.FrontCell, Hosts: r.hosts, LlamaServerBinary: "/usr/bin/llama-server",
		Warnf: func(f string, a ...any) { warns = append(warns, strings.TrimSpace(sprintf(f, a...))) },
	})
	if err != nil {
		t.Fatalf("front render: %v", err)
	}
	if strings.Contains(front, "trial-x") {
		t.Fatalf("the front render carries the trial id — it is routable fleet-wide:\n%s", front)
	}
	if !strings.Contains(front, incName) {
		t.Fatalf("the front render lost the incumbent too; the exclusion is too wide:\n%s", front)
	}
	if !anyContains(warns, "trial-x") || !anyContains(warns, "trial: true") {
		t.Fatalf("the exclusion was silent; warnings were %q. A def that vanishes from the catalog without a sentence is the bug, not the fix", warns)
	}

	cell, err := router.Render(defs, router.Options{
		Cell: testCell, Hosts: r.hosts, LlamaServerBinary: "/usr/bin/llama-server",
	})
	if err != nil {
		t.Fatalf("cell render: %v", err)
	}
	if !strings.Contains(cell, "trial-x") {
		t.Fatalf("the trial is missing from its OWN cell's render, so it could never be measured:\n%s", cell)
	}
}

// TestTrialDefValidation pins what `trial: true` requires. Both rungs are
// structural: an unassigned trial renders wherever a render runs (its
// weights are on one box), and a non-external trial is not served by the
// router at all, so the front-render exclusion would not apply to it.
func TestTrialDefValidation(t *testing.T) {
	dir := t.TempDir()
	// A real file: without cell: the loader stats the weights path, and a
	// missing file would refuse for a reason that has nothing to do with
	// the rungs under test.
	weights := filepath.Join(dir, "x.gguf")
	if err := os.WriteFile(weights, []byte("gguf"), 0o644); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	base := `backend:
  external: true
  llama_server:
    path: ` + weights + `
    alias: x
    context: 4096
`
	write("ok", "cell: "+testCell+"\ntrial: true\n"+base)
	if _, err := profile.LoadBackendFrom(dir, "ok"); err != nil {
		t.Fatalf("a well-formed trial def must load: %v", err)
	}
	write("nocell", "trial: true\n"+base)
	if _, err := profile.LoadBackendFrom(dir, "nocell"); err == nil || !strings.Contains(err.Error(), "requires cell:") {
		t.Fatalf("an unassigned trial def must be refused, got %v", err)
	}
	write("notexternal", "cell: "+testCell+`
trial: true
backend:
  llama_server:
    path: `+weights+`
    alias: x
    context: 4096
`)
	if _, err := profile.LoadBackendFrom(dir, "notexternal"); err == nil || !strings.Contains(err.Error(), "backend.external") {
		t.Fatalf("a non-external trial def must be refused, got %v", err)
	}
	write("cloud", "cell: "+testCell+`
trial: true
backend:
  cloud_peer:
    base_url: https://api.example.com
    models: [some-model]
`)
	if _, err := profile.LoadBackendFrom(dir, "cloud"); err == nil || !strings.Contains(err.Error(), "cloud_peer") {
		t.Fatalf("a cloud_peer trial must be refused, got %v", err)
	}
}

// ── U2: the derivation drops every claim on the fleet ────────────────────

// TestDeriveDefKeepsFlagsAndDropsFleetClaims names each dropped field and
// why. Every one of them is a way a candidate could quietly change what
// the fleet already serves.
func TestDeriveDefKeepsFlagsAndDropsFleetClaims(t *testing.T) {
	r := smallRig(t)
	inc := mustDef(t, r, incName)
	got := deriveDef(inc, "trial-x", "/models/new.gguf", testCell)

	ls := got.Backend.LlamaServer
	if ls.Context != 65536 || ls.GPULayers != 99 || !ls.FlashAttn || ls.CacheTypeK != "q8_0" {
		t.Fatalf("the serving flags did not transfer: %+v", ls)
	}
	if len(ls.ExtraArgs) != 2 || ls.ExtraArgs[0] != "--threads" {
		t.Fatalf("extra_args did not transfer: %v", ls.ExtraArgs)
	}
	if ls.Path != "/models/new.gguf" {
		t.Fatalf("the weights were not replaced: %q", ls.Path)
	}
	if ls.Alias != "trial-x" {
		t.Fatalf("alias %q: inheriting the incumbent's alias silently re-points every client already using it", ls.Alias)
	}
	if ls.Huggingface != nil {
		t.Fatal("the huggingface block survived: a later `vibe pull` would re-fetch the INCUMBENT's file under the trial's name")
	}
	if ls.MMProj != "" || ls.DraftModel != "" {
		t.Fatal("mmproj/draft_model survived: they are paths to another model's companion files")
	}
	if got.Router != nil {
		t.Fatal("the router block survived: its aliases collide with the incumbent's, and alias_owner would take them")
	}
	if got.Fingerprint != "" {
		t.Fatal("fingerprint: strict survived: a candidate's hash drift must not yank models out of the front render")
	}
	if got.Lifecycle == nil || got.Lifecycle.Preload {
		t.Fatalf("lifecycle: preload must not transfer (it loads the candidate at every llama-swap start), got %+v", got.Lifecycle)
	}
	if got.Lifecycle.TTL == nil {
		t.Fatal("lifecycle.ttl describes how this box behaves and should transfer")
	}
	if !got.Trial || got.Cell != testCell {
		t.Fatalf("the trial marking is missing: trial=%v cell=%q", got.Trial, got.Cell)
	}
}

// ── U3: the refusals ────────────────────────────────────────────────────

func TestPlanRefusals(t *testing.T) {
	t.Run("the front cell has no models to try against", func(t *testing.T) {
		r := smallRig(t)
		run := r.runner(func(o *Options) { o.Cell = fleetcfg.FrontCell })
		if _, _, err := run.Plan(context.Background(), planReq()); err == nil || !strings.Contains(err.Error(), "peers-only") {
			t.Fatalf("want a front refusal, got %v", err)
		}
	})
	t.Run("a cell this box is not", func(t *testing.T) {
		r := smallRig(t)
		err := r.runner().RefuseRemote("other-cell")
		if err == nil || !strings.Contains(err.Error(), "not this box's cell") {
			t.Fatalf("want a remote refusal, got %v", err)
		}
		if err := r.runner().RefuseRemote(testCell); err != nil {
			t.Fatalf("the local cell must be allowed: %v", err)
		}
	})
	t.Run("--like must be external", func(t *testing.T) {
		r := smallRig(t)
		r.writeDef("local-only", `cell: `+testCell+`
backend:
  llama_server:
    path: /models/a.gguf
    alias: a
    context: 4096
`)
		req := planReq()
		req.Like = "local-only"
		if _, _, err := r.runner().Plan(context.Background(), req); err == nil || !strings.Contains(err.Error(), "not external") {
			t.Fatalf("want a non-external refusal, got %v", err)
		}
	})
	t.Run("--like must be on this cell", func(t *testing.T) {
		r := smallRig(t)
		r.writeDef("elsewhere", `cell: front
backend:
  external: true
  llama_server:
    path: /models/a.gguf
    alias: a
    context: 4096
`)
		req := planReq()
		req.Like = "elsewhere"
		if _, _, err := r.runner().Plan(context.Background(), req); err == nil || !strings.Contains(err.Error(), "another box's GPU") {
			t.Fatalf("want a wrong-cell refusal, got %v", err)
		}
	})
	t.Run("a name that already serves something", func(t *testing.T) {
		r := smallRig(t)
		req := planReq()
		req.As = incName
		if _, _, err := r.runner().Plan(context.Background(), req); err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("want a collision refusal, got %v", err)
		}
	})
	t.Run("weights that another def already serves", func(t *testing.T) {
		r := smallRig(t)
		req := planReq()
		req.File = "qwen3.6-27b-Q5.gguf" // the incumbent's own file
		if _, _, err := r.runner().Plan(context.Background(), req); err == nil || !strings.Contains(err.Error(), "already serves") {
			t.Fatalf("want a weights-path refusal, got %v", err)
		}
	})
	t.Run("a second trial while one is in flight", func(t *testing.T) {
		r := smallRig(t)
		run := r.runner()
		if _, _, err := run.Plan(context.Background(), planReq()); err != nil {
			t.Fatal(err)
		}
		if _, _, err := run.Plan(context.Background(), planReq()); err == nil || !strings.Contains(err.Error(), "already in flight") {
			t.Fatalf("want a one-trial-at-a-time refusal, got %v", err)
		}
	})
}

// TestDiskRefusals pins the rule this repo has been bitten by six times:
// absent evidence must never read as a healthy value. An unknown file
// size is not a small one, and an unreadable filesystem is not an empty
// one.
func TestDiskRefusals(t *testing.T) {
	t.Run("an unknown size is not a small one", func(t *testing.T) {
		r := smallRig(t)
		r.headSize, r.headErr = 0, errors.New("no network")
		_, _, err := r.runner().Plan(context.Background(), planReq())
		if err == nil || !strings.Contains(err.Error(), "not a small one") {
			t.Fatalf("want a refusal on unknown size, got %v", err)
		}
	})
	t.Run("an unreadable filesystem is not an empty one", func(t *testing.T) {
		r := smallRig(t)
		run := r.runner(func(o *Options) {
			o.DiskFree = func(string) (int64, error) { return 0, errors.New("statfs: permission denied") }
		})
		_, _, err := run.Plan(context.Background(), planReq())
		if err == nil || !strings.Contains(err.Error(), "cannot read free space") {
			t.Fatalf("want a refusal on an unreadable filesystem, got %v", err)
		}
	})
	t.Run("the headroom is enforced with numbers", func(t *testing.T) {
		r := smallRig(t)
		r.headSize = 30 << 30
		run := r.runner(func(o *Options) {
			o.DiskFree = func(string) (int64, error) { return 35 << 30, nil }
			o.MinFreeBytes = 20 << 30
		})
		_, _, err := run.Plan(context.Background(), planReq())
		if err == nil || !strings.Contains(err.Error(), "would leave") {
			t.Fatalf("want a headroom refusal, got %v", err)
		}
	})
	t.Run("--skip-disk-check is the override", func(t *testing.T) {
		r := smallRig(t)
		r.headSize, r.headErr = 0, errors.New("no network")
		req := planReq()
		req.SkipDiskCheck = true
		if _, _, err := r.runner().Plan(context.Background(), req); err != nil {
			t.Fatalf("the override must work: %v", err)
		}
	})
}

// ── U4: staging is atomic against a render that cannot succeed ───────────

// TestStageLeavesNothingBehindWhenTheRenderFails is the failure mode that
// would break the box: router.LoadDefs treats ONE invalid or
// unrenderable def as a hard error for the whole directory, so a trial
// def that survives a failed render breaks every later render and every
// announce on this cell — including the ones that would end the trial.
func TestStageLeavesNothingBehindWhenTheRenderFails(t *testing.T) {
	r := smallRig(t)
	// A def that claims the trial's future name as an alias. resolveAliases
	// makes that a render error ("def names are canonical model ids").
	r.writeDef("squatter", `cell: `+testCell+`
router:
  aliases: [trial-qwen3.7-32b]
backend:
  external: true
  llama_server:
    path: /models/sq.gguf
    alias: sq
    context: 4096
`)
	run := r.runner()
	t0, def, err := run.Plan(context.Background(), planReq())
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Fetch(context.Background(), t0, nil); err != nil {
		t.Fatal(err)
	}
	before := readOrEmpty(t, r.configPath)
	err = run.Stage(t0, def)
	if err == nil || !strings.Contains(err.Error(), "nothing on this box changed") {
		t.Fatalf("want a staging refusal that names its own cleanup, got %v", err)
	}
	if _, statErr := os.Stat(t0.DefPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("the unrenderable def survived: every later LoadDefs on this box now fails, including the rollback's")
	}
	if got := readOrEmpty(t, r.configPath); got != before {
		t.Fatal("the llama-swap config changed on a failed stage")
	}
	if t0.State != StateFetched {
		t.Fatalf("state advanced past the failure: %q", t0.State)
	}
}

// ── U5: the full sequence, applied and rolled back ──────────────────────

// TestTrialRoundTripRestoresTheConfigByteForByte is the rollback gate. It
// runs the real sequence against a real llama-swap double and asserts the
// property the phase promises: after `end`, the box is where it started.
func TestTrialRoundTripRestoresTheConfigByteForByte(t *testing.T) {
	r := smallRig(t)
	run := r.runner()
	ctx := context.Background()

	// A pre-existing config, exactly as `vibe router render` would leave it.
	if _, err := run.render(true); err != nil {
		t.Fatalf("seed render: %v", err)
	}
	before := readOrEmpty(t, r.configPath)
	if !strings.Contains(before, incName) {
		t.Fatalf("the seed render is empty:\n%s", before)
	}

	tr, def, err := run.Plan(ctx, planReq())
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Fetch(ctx, tr, nil); err != nil {
		t.Fatal(err)
	}
	if len(r.fetched) != 1 || r.fetched[0].Repo != "unsloth/Qwen3.7-32B-GGUF" {
		t.Fatalf("the fetch did not happen as planned: %+v", r.fetched)
	}
	if err := run.Stage(tr, def); err != nil {
		t.Fatal(err)
	}
	if tr.State != StateStaged {
		t.Fatalf("state %q", tr.State)
	}

	// llama-swap adopts the config (what -watch-config does).
	r.swap.SetModelState(tr.Def, "stopped")

	res, err := run.Apply(ctx, tr)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Live {
		t.Fatalf("the trial never appeared in the cell's catalog: %s", res.Note)
	}
	applied := readOrEmpty(t, r.configPath)
	if !strings.Contains(applied, tr.Def) {
		t.Fatalf("the applied config does not carry the trial:\n%s", applied)
	}
	if tr.BackupPath == "" {
		t.Fatal("no config was banked; the rollback has nothing to fall back to")
	}

	rep, err := run.End(ctx, tr, false)
	if err != nil {
		t.Fatalf("end: %v (%v)", err, rep.Steps)
	}
	if got := readOrEmpty(t, r.configPath); got != before {
		t.Fatalf("the config was not restored.\nwant:\n%s\ngot:\n%s", before, got)
	}
	if _, err := os.Stat(tr.DefPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the trial def survived the rollback")
	}
	if _, err := os.Stat(tr.WeightsPath); err != nil {
		t.Fatal("the weights were deleted without --purge")
	}
	if _, err := os.Stat(r.statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the journal survived; `status` would report a trial that no longer exists")
	}
	if again, err := run.Load(); err != nil || again != nil {
		t.Fatalf("a closed trial must load as absent: %v %v", again, err)
	}
}

// TestEndPurgeDeletesTheWeightsAndPlainEndDoesNot pins the one thing the
// rollback deliberately does NOT undo.
func TestEndPurgeDeletesTheWeightsAndPlainEndDoesNot(t *testing.T) {
	for _, purge := range []bool{false, true} {
		r := smallRig(t)
		run := r.runner()
		tr, _, err := run.Plan(context.Background(), planReq())
		if err != nil {
			t.Fatal(err)
		}
		if err := run.Fetch(context.Background(), tr, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := run.End(context.Background(), tr, purge); err != nil {
			t.Fatal(err)
		}
		_, statErr := os.Stat(tr.WeightsPath)
		gone := errors.Is(statErr, os.ErrNotExist)
		if gone != purge {
			t.Fatalf("purge=%v: weights gone=%v", purge, gone)
		}
	}
}

// TestEndRestoresFromTheBankedBytesWhenTheRenderCannotRun is the
// second rollback path, and it is the one that matters at 2 a.m.: a
// render can fail for reasons that have nothing to do with the trial
// (someone else's def edit, a broken extras file), and the operator still
// needs their router config back.
func TestEndRestoresFromTheBankedBytesWhenTheRenderCannotRun(t *testing.T) {
	r := smallRig(t)
	run := r.runner()
	ctx := context.Background()
	if _, err := run.render(true); err != nil {
		t.Fatal(err)
	}
	before := readOrEmpty(t, r.configPath)

	tr, def, err := run.Plan(ctx, planReq())
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Fetch(ctx, tr, nil); err != nil {
		t.Fatal(err)
	}
	if err := run.Stage(tr, def); err != nil {
		t.Fatal(err)
	}
	r.swap.SetModelState(tr.Def, "stopped")
	if _, err := run.Apply(ctx, tr); err != nil {
		t.Fatal(err)
	}
	// Someone else breaks a def while the trial is running.
	r.writeDef("broken", "this: is: not: yaml:\n  - [\n")

	rep, err := run.End(ctx, tr, false)
	if err != nil {
		t.Fatalf("end must still restore: %v", err)
	}
	if !rep.RestoredFromBackup {
		t.Fatalf("the fallback did not fire; steps were %v", rep.Steps)
	}
	if got := readOrEmpty(t, r.configPath); got != before {
		t.Fatalf("the banked bytes were not restored:\n%s", got)
	}
}

// ── U6: applied but not live ────────────────────────────────────────────

// TestAppliedButNotLiveIsItsOwnAnswer. A llama-swap started without
// -watch-config never adopts the write, and that is neither a failure of
// the trial nor a success: the config IS written, so the rollback is
// still owed, and the measurement is refused rather than run against a
// model nothing is serving.
func TestAppliedButNotLiveIsItsOwnAnswer(t *testing.T) {
	r := smallRig(t)
	run := r.runner()
	ctx := context.Background()
	tr, def, err := run.Plan(ctx, planReq())
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Fetch(ctx, tr, nil); err != nil {
		t.Fatal(err)
	}
	if err := run.Stage(tr, def); err != nil {
		t.Fatal(err)
	}
	// Deliberately do NOT tell the double about the new model.
	res, err := run.Apply(ctx, tr)
	if err != nil {
		t.Fatal(err)
	}
	if res.Live {
		t.Fatal("live with a catalog that never listed the trial")
	}
	if !strings.Contains(res.Note, "-watch-config") {
		t.Fatalf("the note must name the cause an operator can act on: %q", res.Note)
	}
	if tr.State != StateApplied {
		t.Fatalf("state %q: the config is written, so the rollback is owed", tr.State)
	}
	if _, err := run.Measure(ctx, tr); err == nil || !strings.Contains(err.Error(), "not in this cell's llama-swap catalog") {
		t.Fatalf("a measurement against a model nothing serves must be refused, got %v", err)
	}
}

// ── U7: the measurement and its caveats ─────────────────────────────────

// TestMeasureProbesBothSidesAndSaysWhatItIsNot drives the real prober
// against the real double.
func TestMeasureProbesBothSidesAndSaysWhatItIsNot(t *testing.T) {
	r := smallRig(t)
	run := r.runner()
	ctx := context.Background()
	tr, def, err := run.Plan(ctx, planReq())
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Fetch(ctx, tr, nil); err != nil {
		t.Fatal(err)
	}
	if err := run.Stage(tr, def); err != nil {
		t.Fatal(err)
	}
	// swaptest answers completions but does not model llama-swap's
	// JIT-load, so /running would never show either model ready and C8's
	// cardinal rule (a probe never loads a model) would refuse both. Set
	// the post-warm state the real cell reaches by answering the warm.
	r.swap.SetModelState(tr.Def, "ready")
	r.swap.SetModelState(incName, "ready")
	if _, err := run.Apply(ctx, tr); err != nil {
		t.Fatal(err)
	}
	cmp, err := run.Measure(ctx, tr)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if cmp.Trial.Model != tr.Def || cmp.Incumbent.Model != incName {
		t.Fatalf("wrong sides: %+v", cmp)
	}
	if cmp.Trial.Value <= 0 {
		t.Fatalf("the trial produced no rate: %+v (note %q)", cmp.Trial, cmp.Trial.Note)
	}
	if cmp.Incumbent.Value <= 0 {
		t.Fatalf("the incumbent produced no rate: %+v (note %q)", cmp.Incumbent, cmp.Incumbent.Note)
	}
	if cmp.Trial.WeightsBytes <= 0 {
		t.Fatal("the resource half of the comparison is missing; tok/s alone cannot say a bigger model won")
	}
	if len(cmp.Caveats) < 3 {
		t.Fatalf("a number with no caveats: %v", cmp.Caveats)
	}
	var b strings.Builder
	cmp.Render(&b)
	out := b.String()
	for _, want := range []string{"one sample each", "measured SECOND", "Throughput is not quality", tr.Def, incName} {
		if !strings.Contains(out, want) {
			t.Fatalf("the report does not say %q:\n%s", want, out)
		}
	}
	if tr.State != StateMeasured {
		t.Fatalf("state %q", tr.State)
	}
}

// TestDeltaOnlyWhenTheMetricsMatch. decode_tok_s excludes queueing and
// e2e_tok_s does not; putting them under one percentage would be the
// same mistake C8 avoids by keeping the metric in the baseline key.
func TestDeltaOnlyWhenTheMetricsMatch(t *testing.T) {
	same := &Comparison{
		Trial:     Measurement{Model: "t", Metric: "decode_tok_s", Value: 44},
		Incumbent: Measurement{Model: "i", Metric: "decode_tok_s", Value: 40},
	}
	if d := same.Delta(); !strings.Contains(d, "10% faster") {
		t.Fatalf("want a delta, got %q", d)
	}
	slower := &Comparison{
		Trial:     Measurement{Model: "t", Metric: "decode_tok_s", Value: 30},
		Incumbent: Measurement{Model: "i", Metric: "decode_tok_s", Value: 40},
	}
	if d := slower.Delta(); !strings.Contains(d, "slower") {
		t.Fatalf("want a slower delta, got %q", d)
	}
	mixed := &Comparison{
		Trial:     Measurement{Model: "t", Metric: "decode_tok_s", Value: 44},
		Incumbent: Measurement{Model: "i", Metric: "e2e_tok_s", Value: 40},
	}
	if d := mixed.Delta(); d != "" {
		t.Fatalf("two different quantities must not produce a ratio, got %q", d)
	}
	mixed.Caveats = caveats(mixed)
	if !anyContains(mixed.Caveats, "not comparable") {
		t.Fatalf("the mismatch must be stated: %v", mixed.Caveats)
	}
}

// ── U8: the journal ─────────────────────────────────────────────────────

// TestCorruptJournalIsNeverReadAsNoTrial. The journal is the only record
// of what a rollback has to undo; a parse failure that returned "no
// trial" would leave an applied config permanent and invisible.
func TestCorruptJournalIsNeverReadAsNoTrial(t *testing.T) {
	r := smallRig(t)
	if err := os.WriteFile(r.statePath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	tr, err := r.runner().Load()
	if err == nil {
		t.Fatalf("a corrupt journal must be an error, got trial=%v", tr)
	}
	if !strings.Contains(err.Error(), "rollback") {
		t.Fatalf("the error must say why it is not \"no trial\": %v", err)
	}
}

// TestFetchRefusesAShortFile. hfdownload's own staging protects the
// direct path, but the hf CLI writes through a different mechanism and
// the size is the only thing this side can check.
func TestFetchRefusesAShortFile(t *testing.T) {
	r := smallRig(t)
	run := r.runner(func(o *Options) {
		o.Fetch = func(_ context.Context, _ hfdownload.Spec, dest string, _ hfdownload.ProgressFunc) error {
			return os.WriteFile(dest, []byte("truncated"), 0o644)
		}
	})
	tr, _, err := run.Plan(context.Background(), planReq())
	if err != nil {
		t.Fatal(err)
	}
	err = run.Fetch(context.Background(), tr, nil)
	if err == nil || !strings.Contains(err.Error(), "refusing to serve a short file") {
		t.Fatalf("want a short-file refusal, got %v", err)
	}
	if tr.State != StatePlanned {
		t.Fatalf("state advanced past a bad download: %q", tr.State)
	}
}

// TestJournalSurvivesAProcessDeath: the state on disk after each step is
// what a fresh Runner reads, which is what makes a killed `try`
// resumable and reversible from a later process.
func TestJournalSurvivesAProcessDeath(t *testing.T) {
	r := smallRig(t)
	ctx := context.Background()
	tr, def, err := r.runner().Plan(ctx, planReq())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.runner().Fetch(ctx, tr, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.runner().Stage(tr, def); err != nil {
		t.Fatal(err)
	}
	reloaded, err := r.runner().Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded == nil || reloaded.State != StateStaged || reloaded.Def != tr.Def || reloaded.DefPath != tr.DefPath {
		t.Fatalf("a fresh process cannot resume: %+v", reloaded)
	}
	if len(reloaded.Log) < 3 {
		t.Fatalf("the log lost steps: %v", reloaded.Log)
	}
	var b strings.Builder
	reloaded.RenderStatus(&b)
	status := b.String()
	for _, want := range []string{"vibe model try end", "trial: true", "not in the front's catalog"} {
		if !strings.Contains(status, want) {
			t.Fatalf("status must name %q:\n%s", want, status)
		}
	}
}

// TestStagedDefRoundTripsThroughTheRealLoader proves the bytes written
// are bytes profile.LoadBackendFrom accepts — the scaffolded def is
// input to every render and every announce on this box.
func TestStagedDefRoundTripsThroughTheRealLoader(t *testing.T) {
	r := smallRig(t)
	run := r.runner()
	ctx := context.Background()
	tr, def, err := run.Plan(ctx, planReq())
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Fetch(ctx, tr, nil); err != nil {
		t.Fatal(err)
	}
	if err := run.Stage(tr, def); err != nil {
		t.Fatal(err)
	}
	loaded, err := profile.LoadBackendFrom(r.backends, tr.Def)
	if err != nil {
		t.Fatalf("the staged def does not load: %v", err)
	}
	if !loaded.Trial {
		t.Fatal("the staged def lost trial: true — it would render into the front's peer map")
	}
	body, err := os.ReadFile(tr.DefPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "vibe model try end") {
		t.Fatalf("the file does not tell a reader how to roll it back:\n%s", body)
	}
	preview, err := PreviewDef(tr, def)
	if err != nil {
		t.Fatal(err)
	}
	if preview != string(body) {
		t.Fatal("--dry-run previews something other than what Stage writes")
	}
}

// ── helpers ─────────────────────────────────────────────────────────────

func mustDef(t *testing.T, r *rig, name string) *profile.BackendDef {
	t.Helper()
	def, err := profile.LoadBackendFrom(r.backends, name)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	if def.Name == "" {
		def.Name = name
	}
	return def
}

func readOrEmpty(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func anyContains(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }

// TestStepsRefuseOutOfOrder. The states name what is TRUE on disk, so a
// step that ran against the wrong one would write a def for weights that
// are not there, or measure a config that was never applied.
func TestStepsRefuseOutOfOrder(t *testing.T) {
	r := smallRig(t)
	run := r.runner()
	ctx := context.Background()
	tr, def, err := run.Plan(ctx, planReq())
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Stage(tr, def); err == nil || !strings.Contains(err.Error(), "weights are not on disk") {
		t.Fatalf("staging before the download must be refused, got %v", err)
	}
	if _, err := run.Apply(ctx, tr); err == nil || !strings.Contains(err.Error(), "cannot apply") {
		t.Fatalf("applying before staging must be refused, got %v", err)
	}
	if _, err := run.Measure(ctx, tr); err == nil || !strings.Contains(err.Error(), "apply it first") {
		t.Fatalf("measuring before applying must be refused, got %v", err)
	}
}
