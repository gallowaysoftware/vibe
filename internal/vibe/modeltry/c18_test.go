package modeltry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/swaptest"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/hfdownload"
	"github.com/gallowaysoftware/vibe/internal/vibe/modelprobe"
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
	t.Run("a DIFFERENT trial while one is in flight", func(t *testing.T) {
		r := smallRig(t)
		run := r.runner()
		if _, _, err := run.Plan(context.Background(), planReq()); err != nil {
			t.Fatal(err)
		}
		other := planReq()
		other.Repo = "someone/Other-Model-GGUF"
		if _, _, err := run.Plan(context.Background(), other); err == nil || !strings.Contains(err.Error(), "already in flight") {
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
	if _, err := run.Measure(ctx, tr, nil); err == nil || !strings.Contains(err.Error(), "not in this cell's llama-swap catalog") {
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
	cmp, err := run.Measure(ctx, tr, nil)
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
	if _, err := run.Measure(ctx, tr, nil); err == nil || !strings.Contains(err.Error(), "apply it first") {
		t.Fatalf("measuring before applying must be refused, got %v", err)
	}
}

// ── U13: the review pass's findings ─────────────────────────────────────

// TestReRunningResumesTheSameTrial. The phase promises "re-run to
// continue" and Plan is the only entry point, so a Plan that refused on
// an open journal made that promise unreachable — the fetch is the long
// step and the apply is the disruptive one.
func TestReRunningResumesTheSameTrial(t *testing.T) {
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
	// A fresh process, same command line.
	again, def2, err := r.runner().Plan(ctx, planReq())
	if err != nil {
		t.Fatalf("re-running must resume, not refuse: %v", err)
	}
	if again.State != StateStaged || again.Def != tr.Def || again.WeightsPath != tr.WeightsPath {
		t.Fatalf("the resume lost the journal: %+v", again)
	}
	if def2 == nil || def2.Name != tr.Def || !def2.Trial {
		t.Fatalf("the resume did not re-derive the def: %+v", def2)
	}
	if !anyLogContains(again.Log, "resumed at state staged") {
		t.Fatalf("a resume must be visible in the journal: %v", again.Log)
	}
	// And it is idempotent: staging again must not fail or rewrite.
	if err := r.runner().Stage(again, def2); err != nil {
		t.Fatalf("re-staging a staged trial must be a no-op: %v", err)
	}
}

// TestJournalFromAnotherCellOrConfigIsRefused. Both are reachable by
// editing the daemon config mid-trial, and both would make `end` restore
// a banked config over a file it never banked.
func TestJournalFromAnotherCellOrConfigIsRefused(t *testing.T) {
	r := smallRig(t)
	ctx := context.Background()
	tr, _, err := r.runner().Plan(ctx, planReq())
	if err != nil {
		t.Fatal(err)
	}
	moved := r.runner(func(o *Options) { o.ConfigPath = filepath.Join(r.dir, "elsewhere.yaml") })
	if _, _, err := moved.Plan(ctx, planReq()); err == nil || !strings.Contains(err.Error(), "never banked") {
		t.Fatalf("want a config-path refusal, got %v", err)
	}
	if _, err := moved.End(ctx, tr, false); err == nil || !strings.Contains(err.Error(), "never banked") {
		t.Fatalf("end must refuse too, got %v", err)
	}
	other := r.runner(func(o *Options) { o.Cell = "other-cell" })
	if _, err := other.End(ctx, tr, false); err == nil || !strings.Contains(err.Error(), "fleet.cell") {
		t.Fatalf("want a cell refusal on end, got %v", err)
	}
}

// TestFailedRollbackKeepsTheJournalAndTheBackup. Deleting them because
// the function is returning turns a recoverable half-rollback into a
// permanent one, and `status` would then report no trial on a box still
// serving it.
func TestFailedRollbackKeepsTheJournalAndTheBackup(t *testing.T) {
	r := smallRig(t)
	run := r.runner()
	ctx := context.Background()
	if _, err := run.render(true); err != nil {
		t.Fatal(err)
	}
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
	// Break the re-render AND the restore: a broken def plus an
	// unwritable config directory.
	r.writeDef("broken", "this: is: not: yaml:\n  - [\n")
	if err := os.Chmod(filepath.Dir(r.configPath), 0o500); err != nil {
		t.Skipf("cannot make the config dir read-only here: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(r.configPath), 0o755) })

	rep, err := run.End(ctx, tr, false)
	if err == nil {
		t.Fatalf("a rollback that could not run must be an error; steps were %v", rep.Steps)
	}
	if _, statErr := os.Stat(r.statePath); statErr != nil {
		t.Fatal("the journal was deleted after a FAILED rollback — the retry has nothing to work from")
	}
	if _, statErr := os.Stat(tr.BackupPath); statErr != nil {
		t.Fatal("the banked config was deleted after a FAILED rollback — the only copy of the pre-trial config is gone")
	}
	if !anyContains(rep.Steps, "KEPT the journal") {
		t.Fatalf("the report must say what it kept: %v", rep.Steps)
	}
}

func anyLogContains(entries []LogEntry, sub string) bool {
	for _, e := range entries {
		if strings.Contains(e.Note, sub) {
			return true
		}
	}
	return false
}

// ── the adversarial-review pass's findings ──────────────────────────────

// TestTrialVRAMEstimateIsNotTheIncumbentsNumber. estimated_vram_gb is a
// hand-measured claim about the INCUMBENT's weights, and the report puts
// it in a column headed by the candidate's name. Inheriting it makes the
// two sides equal by construction on the one row that answers "and how
// much bigger is it" — the sentence the resource half of the comparison
// exists for.
func TestTrialVRAMEstimateIsNotTheIncumbentsNumber(t *testing.T) {
	r := smallRig(t)
	inc := mustDef(t, r, incName)
	if inc.EstimatedVRAMGB <= 0 {
		t.Fatalf("the fixture must declare one for this test to mean anything: %v", inc.EstimatedVRAMGB)
	}
	got := deriveDef(inc, "trial-x", "/models/new.gguf", testCell)
	if got.EstimatedVRAMGB != 0 {
		t.Fatalf("the trial inherited the incumbent's VRAM estimate (%v): the report would print a number nobody measured under the candidate's name",
			got.EstimatedVRAMGB)
	}
	m := Measurement{Model: "trial-x", VRAMEstimateGB: got.EstimatedVRAMGB}
	if vram(m) != "not declared" {
		t.Fatalf("an undeclared estimate must render as undeclared, got %q", vram(m))
	}
}

// TestEndWaitsForLlamaSwapToDropTheTrial. `-watch-config` polls on its
// own 2 s cadence, so a single catalog read taken microseconds after the
// re-render finds the trial still listed on EVERY healthy rollback, and
// told the operator to restart llama-swap — a warning that fires on the
// success path is one nobody reads on the failure path.
func TestEndWaitsForLlamaSwapToDropTheTrial(t *testing.T) {
	r := smallRig(t)
	run := r.runner(func(o *Options) { o.CatalogWait = 10 * time.Second })
	ctx := context.Background()
	if _, err := run.render(true); err != nil {
		t.Fatal(err)
	}
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
	// A real -watch-config cell drops the id a poll interval AFTER the
	// write, not during it.
	go func() {
		time.Sleep(300 * time.Millisecond)
		r.swap.RemoveModel(tr.Def)
	}()
	rep, err := run.End(ctx, tr, false)
	if err != nil {
		t.Fatalf("end: %v (%v)", err, rep.Steps)
	}
	if rep.StillInCatalog {
		t.Fatalf("end reported the trial still in the catalog after llama-swap dropped it: %q", rep.Note)
	}
	if rep.Note != "" {
		t.Fatalf("a clean rollback must print no restart warning, got %q", rep.Note)
	}
}

// TestEndStillReportsACatalogThatNeverLetGo is the other half: a cell
// whose llama-swap does not watch its config keeps serving the trial, and
// that must survive the poll rather than be waited into silence.
func TestEndStillReportsACatalogThatNeverLetGo(t *testing.T) {
	r := smallRig(t)
	run := r.runner(func(o *Options) { o.CatalogWait = 50 * time.Millisecond })
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
	r.swap.SetModelState(tr.Def, "stopped")
	if _, err := run.Apply(ctx, tr); err != nil {
		t.Fatal(err)
	}
	rep, err := run.End(ctx, tr, false)
	if err != nil {
		t.Fatalf("end: %v", err)
	}
	if !rep.StillInCatalog || !strings.Contains(rep.Note, "restart it") {
		t.Fatalf("a llama-swap that never reloaded must still be reported: still=%v note=%q", rep.StillInCatalog, rep.Note)
	}
}

// TestEndRerendersAStagedTrialTheConfigStillNames. `staged` promises the
// TRIAL never wrote the config — not that nobody else ran
// `vibe router render` between the staging and the rollback. After that,
// removing the def alone leaves the cell serving a trial `end` has just
// reported closed.
func TestEndRerendersAStagedTrialTheConfigStillNames(t *testing.T) {
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
	// Somebody runs `vibe router render` while the trial is staged.
	if _, err := run.render(true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readOrEmpty(t, r.configPath), tr.Def) {
		t.Fatal("the setup did not put the trial in the config")
	}
	if tr.State != StateStaged {
		t.Fatalf("state %q", tr.State)
	}
	if _, err := run.End(ctx, tr, false); err != nil {
		t.Fatal(err)
	}
	got := readOrEmpty(t, r.configPath)
	if strings.Contains(got, tr.Def) {
		t.Fatalf("`end` reported the trial closed while the cell's config still serves it:\n%s", got)
	}
	if got != before {
		t.Fatalf("the config was not restored:\nwant:\n%s\ngot:\n%s", before, got)
	}
}

// TestEndDoesNotWriteAConfigTheTrialNeverTouched is the fail-closed half
// of the same fix: on the ordinary staged rollback the trial never wrote
// the config, and `end` must not become an implicit `vibe router render`
// that creates one.
func TestEndDoesNotWriteAConfigTheTrialNeverTouched(t *testing.T) {
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
	if _, err := run.End(ctx, tr, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.configPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("`end` created a llama-swap config on a box that had none: %v", err)
	}
}

// TestResumeRefusesADifferentRevisionOrDest. The identity check decides
// whether a re-run RESUMES; a revision or a --dest it ignores is one the
// operator typed and the trial silently did not use.
func TestResumeRefusesADifferentRevisionOrDest(t *testing.T) {
	base := planReq()
	base.Revision = "abc123"
	r := smallRig(t)
	if _, _, err := r.runner().Plan(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	moved := base
	moved.Revision = "def456"
	if _, _, err := r.runner().Plan(context.Background(), moved); err == nil || !strings.Contains(err.Error(), "already in flight") {
		t.Fatalf("a different --revision must not resume the old one, got %v", err)
	}
	elsewhere := base
	elsewhere.Dest = filepath.Join(r.dir, "other-models")
	if _, _, err := r.runner().Plan(context.Background(), elsewhere); err == nil || !strings.Contains(err.Error(), "already in flight") {
		t.Fatalf("a different --dest must not resume weights at the old path, got %v", err)
	}
	same := base
	if again, _, err := r.runner().Plan(context.Background(), same); err != nil || again == nil {
		t.Fatalf("the identical request must still resume: %v", err)
	}
}

// TestResumeRestagesADefTheIncumbentChanged. Resuming re-derives the def
// from the CURRENT incumbent precisely so an edit made mid-trial is
// picked up; a Stage that returned early at `staged` discarded that and
// kept serving the derivation from before the edit.
func TestResumeRestagesADefTheIncumbentChanged(t *testing.T) {
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
	if got := readOrEmpty(t, tr.DefPath); !strings.Contains(got, "context: 65536") {
		t.Fatalf("the fixture's context did not reach the def:\n%s", got)
	}
	// The operator halves the incumbent's context budget between the
	// staging and the apply, which is exactly the edit REV-1's
	// re-derivation exists to pick up.
	r.writeDef(incName, strings.Replace(mustRead(t, filepath.Join(r.backends, incName+".yaml")), "context: 65536", "context: 32768", 1))

	again, def2, err := r.runner().Plan(ctx, planReq())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.runner().Stage(again, def2); err != nil {
		t.Fatal(err)
	}
	got := readOrEmpty(t, again.DefPath)
	if !strings.Contains(got, "context: 32768") {
		t.Fatalf("the staged def still carries the pre-edit derivation:\n%s", got)
	}
	if again.State != StateStaged {
		t.Fatalf("state %q", again.State)
	}
}

// TestWarmErrorsAreStrippedOfControlCharacters. Everything llama-swap
// says lands in a Note that is SAVED to the journal and re-printed by
// every later `vibe model try status`; an escape sequence there is an
// injection into the operator's terminal, replayed forever. Same rule as
// cli.termSafe, held at the point the string is stored.
func TestWarmErrorsAreStrippedOfControlCharacters(t *testing.T) {
	r := smallRig(t, swaptest.WithModels(swaptest.Model{ID: incName, State: "ready"}))
	hostile := "\x1b[2Jcleared your scrollback\x07"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, hostile, http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	run := r.runner(func(o *Options) { o.LlamaSwapURL = srv.URL })
	err := run.warm(context.Background(), incName, "chat")
	if err == nil {
		t.Fatal("want a warm failure")
	}
	if strings.ContainsAny(err.Error(), "\x1b\x07") {
		t.Fatalf("a control character from llama-swap reached the journal and the terminal: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "cleared your scrollback") {
		t.Fatalf("the readable half of the message must survive: %q", err.Error())
	}
}

// ── the independent adversarial pass's findings ─────────────────────────

// TestEndRemovesTheConfigTheTrialCreated is REV3-2. `bankConfig` records
// the absence of a config explicitly, and its own comment says End
// "removes the file it created rather than restoring bytes that never
// existed" — End re-rendered instead, so a box that had no llama-swap
// config at this path was left with one after `end` reported "trial
// closed". Observed on a real fleetlab cell, whose llama-swap is started
// with a different `-config`: the leftover named every model on the cell
// and llama-swap's default upstream range (5800, production's on this
// box), at a path nothing reads and nobody knows to delete.
func TestEndRemovesTheConfigTheTrialCreated(t *testing.T) {
	r := smallRig(t)
	run := r.runner()
	ctx := context.Background()
	// Deliberately NO seed render: this box has no config at r.configPath.
	if _, err := os.Stat(r.configPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the fixture already has a config: %v", err)
	}
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
	if _, err := os.Stat(r.configPath); err != nil {
		t.Fatalf("the apply did not write a config at all: %v", err)
	}
	if !tr.ConfigCreated {
		t.Fatal("the journal did not record that this trial created the config; a later process cannot know to remove it")
	}
	// It survives a process death: the rollback is usually a later one.
	reloaded, err := r.runner().Load()
	if err != nil || reloaded == nil || !reloaded.ConfigCreated {
		t.Fatalf("config_created did not reach the journal: %+v (%v)", reloaded, err)
	}
	rep, err := r.runner().End(ctx, reloaded, false)
	if err != nil {
		t.Fatalf("end: %v (%v)", err, rep.Steps)
	}
	if _, err := os.Stat(r.configPath); !errors.Is(err, os.ErrNotExist) {
		body := readOrEmpty(t, r.configPath)
		t.Fatalf("`end` left behind a llama-swap config on a box that had none — the box is not in its prior state:\n%s", body)
	}
	if !anyContains(rep.Steps, "had no llama-swap config there before") {
		t.Fatalf("the report must say what it removed and why: %v", rep.Steps)
	}
}

// TestEndStillRestoresAConfigThatAlreadyExisted is the fail-closed half:
// the removal fires ONLY when the trial created the file. A cell with a
// real config must get it back, not lose it.
func TestEndStillRestoresAConfigThatAlreadyExisted(t *testing.T) {
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
	if tr.ConfigCreated {
		t.Fatal("the trial claims it created a config that was already there; `end` would delete the cell's real one")
	}
	if _, err := run.End(ctx, tr, false); err != nil {
		t.Fatal(err)
	}
	if got := readOrEmpty(t, r.configPath); got != before {
		t.Fatalf("the pre-existing config was not restored:\nwant:\n%s\ngot:\n%s", before, got)
	}
}

// TestApplyRefusesToDeleteAHandWrittenSection is REV3-6, and it is the
// finding the corrected live rig surfaced the moment L4 could run at all.
//
// `router.Render` merges hand-written sections from an extras file, and
// `mergeExtras` treats a MISSING extras file as "no extras, no error".
// C18 rendered with the XDG default path and no flag to say otherwise, so
// on a cell whose renders are driven by `--extras <file>` — which is what
// scripts/fleetlab writes and what the reference deploy does — the apply
// silently deleted whatever that file contributes:
//
//   - `apiKeys:`, C15's llama-swap credential. The cell stops demanding a
//     key at all, which is strictly worse than the 401 C15 exists to fix,
//     and is what C19's renderPass refuses on by name for the front.
//   - `store:`, the SQLite activity log C7a's ledger and C7b's savings
//     screen are computed from.
//
// Neither appeared anywhere in the trial's output, and `end`'s re-render
// reproduced the loss, so the rollback did not put them back either.
// Observed on a real lab cell: after `end` reported "trial closed", the
// config had lost its `store:` block permanently.
func TestApplyRefusesToDeleteAHandWrittenSection(t *testing.T) {
	r := smallRig(t)
	extras := filepath.Join(r.dir, "router-extras.yaml")
	if err := os.WriteFile(extras, []byte("apiKeys:\n  - the-cells-key\nstore:\n  path: "+
		filepath.Join(r.dir, "activity.db")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// The cell's real config, rendered WITH its extras — the state a
	// llama-swap started with `-config` is actually serving.
	withExtras := r.runner(func(o *Options) { o.ExtrasPath = extras })
	if _, err := withExtras.render(true); err != nil {
		t.Fatal(err)
	}
	before := readOrEmpty(t, r.configPath)
	for _, want := range []string{"apiKeys", "store"} {
		if !strings.Contains(before, want) {
			t.Fatalf("the fixture's config has no %s: section:\n%s", want, before)
		}
	}

	// The trial renders with the DEFAULT extras path, which does not exist.
	run := r.runner()
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
	_, err = run.Apply(ctx, tr)
	if err == nil {
		t.Fatalf("the apply deleted the cell's hand-written sections and said nothing:\n%s", readOrEmpty(t, r.configPath))
	}
	for _, want := range []string{"apiKeys:", "store:", "--extras"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal must name what it would have deleted and the flag that fixes it (%q): %v", want, err)
		}
	}
	if got := readOrEmpty(t, r.configPath); got != before {
		t.Fatalf("the refusal still wrote:\n%s", got)
	}
	if tr.State != StateStaged {
		t.Fatalf("state advanced past a refused apply: %q", tr.State)
	}

	// With the right extras the same apply proceeds, and both sections
	// survive the trial AND the rollback.
	ok := r.runner(func(o *Options) { o.ExtrasPath = extras })
	r.swap.SetModelState(tr.Def, "stopped")
	if _, err := ok.Apply(ctx, tr); err != nil {
		t.Fatalf("the apply must proceed once it renders the cell's own extras: %v", err)
	}
	applied := readOrEmpty(t, r.configPath)
	if !strings.Contains(applied, "apiKeys") || !strings.Contains(applied, tr.Def) {
		t.Fatalf("the applied config lost a section or the trial:\n%s", applied)
	}
	if _, err := ok.End(ctx, tr, false); err != nil {
		t.Fatal(err)
	}
	if got := readOrEmpty(t, r.configPath); got != before {
		t.Fatalf("the rollback did not restore the cell's config:\nwant:\n%s\ngot:\n%s", before, got)
	}
}

// TestUnparseableConfigIsARefusalNotNothingToLose. The dropped-section
// check answers "what would this write delete", and a config it cannot
// parse is the one state where it has no answer. Reading that as "nothing
// to lose" is the absent-evidence-as-a-healthy-value class this repo has
// shipped six times, on the guard that stands in front of the cell's
// credential.
func TestUnparseableConfigIsARefusalNotNothingToLose(t *testing.T) {
	r := smallRig(t)
	ctx := context.Background()
	if err := os.WriteFile(r.configPath, []byte("models:\n  a: [\n   unterminated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := readOrEmpty(t, r.configPath)
	run := r.runner()
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
	if _, err := run.Apply(ctx, tr); err == nil || !strings.Contains(err.Error(), "refusing rather than guessing") {
		t.Fatalf("want a refusal on an unreadable config, got %v", err)
	}
	if got := readOrEmpty(t, r.configPath); got != before {
		t.Fatalf("the refusal still wrote:\n%s", got)
	}
}

// TestEndFallsBackToTheBankWhenTheRerenderWouldDropASection is the same
// rule on the way out. `end` is the surface that tells an operator the box
// is back where it started, so a re-render that would lose a hand-written
// section must lose to the byte-exact bank instead — and say which path
// ran.
func TestEndFallsBackToTheBankWhenTheRerenderWouldDropASection(t *testing.T) {
	r := smallRig(t)
	extras := filepath.Join(r.dir, "router-extras.yaml")
	if err := os.WriteFile(extras, []byte("apiKeys:\n  - the-cells-key\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	run := r.runner(func(o *Options) { o.ExtrasPath = extras })
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
	// The extras file goes away between the apply and the rollback (an
	// unmounted volume, a moved checkout). The re-render would succeed and
	// silently strip apiKeys:.
	if err := os.Remove(extras); err != nil {
		t.Fatal(err)
	}
	rep, err := run.End(ctx, tr, false)
	if err != nil {
		t.Fatalf("end: %v (%v)", err, rep.Steps)
	}
	if !rep.RestoredFromBackup {
		t.Fatalf("the rollback re-rendered without the extras and reported success; steps were %v", rep.Steps)
	}
	if got := readOrEmpty(t, r.configPath); got != before {
		t.Fatalf("the cell lost apiKeys: across a rollback that said it was done:\n%s", got)
	}
}

// TestResumeReChecksTheDisk is REV3-3. §7's refusals lived only in Plan's
// FRESH branch, and `resume` returns before them — so the commonest resume
// of all (a killed twenty-minute pull, journal at `planned`, a .partial on
// disk) re-entered Fetch with no headroom check and no unknown-size
// refusal. Observed live: a re-run with --min-free 100000 and no
// --skip-disk-check went straight to the download.
func TestResumeReChecksTheDisk(t *testing.T) {
	r := smallRig(t)
	ctx := context.Background()
	tr, _, err := r.runner().Plan(ctx, planReq())
	if err != nil {
		t.Fatal(err)
	}
	if tr.State != StatePlanned {
		t.Fatalf("state %q", tr.State)
	}
	// The library filesystem filled while the operator was asleep.
	full := r.runner(func(o *Options) {
		o.DiskFree = func(string) (int64, error) { return 1 << 30, nil }
	})
	if _, _, err := full.Plan(ctx, planReq()); err == nil || !strings.Contains(err.Error(), "would leave") {
		t.Fatalf("a resume must re-check the headroom, got %v", err)
	}
	// And the unreadable-filesystem refusal, which is the same rule.
	blind := r.runner(func(o *Options) {
		o.DiskFree = func(string) (int64, error) { return 0, errors.New("statfs: permission denied") }
	})
	if _, _, err := blind.Plan(ctx, planReq()); err == nil || !strings.Contains(err.Error(), "cannot read free space") {
		t.Fatalf("a resume must re-check that the filesystem is readable, got %v", err)
	}
	// --skip-disk-check is still the override, and a resume past `planned`
	// has nothing left to download.
	skip := planReq()
	skip.SkipDiskCheck = true
	if _, _, err := full.Plan(ctx, skip); err != nil {
		t.Fatalf("the override must still work on a resume: %v", err)
	}
	if err := r.runner().Fetch(ctx, tr, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := full.Plan(ctx, planReq()); err != nil {
		t.Fatalf("a resume at `fetched` has nothing left to pull and must not be refused for disk: %v", err)
	}
}

// TestNotLiveNamesTheConfigItWroteAndBothCauses is REV3-4's display half.
// The note named exactly one cause — a llama-swap started without
// -watch-config — and prescribed a restart. On a cell started with a
// DIFFERENT `-config` (every deploy that does not use the XDG default,
// including scripts/fleetlab and the reference fleet's containerised
// front) the restart comes back reading the same file it always read, and
// the operator has been sent to evict every resident model for nothing.
func TestNotLiveNamesTheConfigItWroteAndBothCauses(t *testing.T) {
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
	res, err := run.Apply(ctx, tr) // the double never learns the id
	if err != nil {
		t.Fatal(err)
	}
	if res.Live {
		t.Fatal("live with a catalog that never listed the trial")
	}
	for _, want := range []string{r.configPath, "-watch-config", "different `-config`", "--config"} {
		if !strings.Contains(res.Note, want) {
			t.Fatalf("the not-live note does not say %q — it is the only diagnosis the operator gets:\n%s", want, res.Note)
		}
	}
}

// TestProbeNotesAreStrippedOfControlCharacters is REV3-5. REV2-11 scrubbed
// `warm`'s 512-byte snippet at the point of capture and stopped there.
// `measureOne` also stores modelprobe's note, and modelprobe's postJSON
// embeds up to 4096 RAW bytes of llama-swap's body — the same untrusted
// socket, eight times the budget — into a Measurement that is saved to the
// journal and re-printed by `Comparison.Render` and by every later
// `vibe model try status`.
func TestProbeNotesAreStrippedOfControlCharacters(t *testing.T) {
	hostile := "\x1b[2Jcleared your scrollback\x07"
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/running" {
			// Resident, so C8's cardinal rule lets the probe through and the
			// hostile body is reached.
			_, _ = w.Write([]byte(`{"running":[{"model":"` + incName + `","state":"ready"}]}`))
			return
		}
		// The WARM succeeds and the PROBE fails: `warm`'s snippet is
		// already scrubbed (REV2-11), so a server that failed both would
		// return on the warm and never exercise the path under test.
		if posts.Add(1) == 1 {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
			return
		}
		http.Error(w, hostile, http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	r := smallRig(t)
	run := r.runner(func(o *Options) { o.LlamaSwapURL = srv.URL })
	defs, err := router.LoadDefs(r.backends)
	if err != nil {
		t.Fatal(err)
	}
	specs := modelprobe.SpecsFromDefs(defs, "/usr/bin/llama-server")
	prober, err := modelprobe.New(modelprobe.Config{
		LlamaSwapURL: srv.URL, ReadOnly: true,
		Specs: func(m string) modelprobe.Spec { return specs[m] },
	})
	if err != nil {
		t.Fatal(err)
	}
	m := run.measureOne(context.Background(), prober, specs, defs, incName)
	if m.Note == "" {
		t.Fatal("the hostile side produced no note at all; the test is not reaching the path")
	}
	if strings.ContainsAny(m.Note, "\x1b\x07") {
		t.Fatalf("a control character from llama-swap was stored in the journal and re-printed by `status`: %q", m.Note)
	}
	if !strings.Contains(m.Note, "cleared your scrollback") {
		t.Fatalf("the readable half of the diagnosis must survive: %q", m.Note)
	}
}

// TestPlanRefusesControlCharactersInTheCoordinate is REV3-8. The
// coordinate is interpolated verbatim into the scaffolded def's header —
// a COMMENT block, where a newline ends the comment and the rest is YAML —
// and into the journal, which `status` re-prints to a terminal forever.
func TestPlanRefusesControlCharactersInTheCoordinate(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*PlanRequest)
	}{
		{"repo", func(q *PlanRequest) { q.Repo = "owner/repo\ntrial: false" }},
		{"file", func(q *PlanRequest) { q.File = "x.gguf\x1b[2J" }},
		{"revision", func(q *PlanRequest) { q.Revision = "main\nname: qwen3.6-27b" }},
		{"dest", func(q *PlanRequest) { q.Dest = "/models\n" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := smallRig(t)
			req := planReq()
			tc.mut(&req)
			_, _, err := r.runner().Plan(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), "control character") {
				t.Fatalf("want a field-hygiene refusal, got %v", err)
			}
			if _, serr := os.Stat(r.statePath); !errors.Is(serr, os.ErrNotExist) {
				t.Fatal("a refused plan opened a journal")
			}
		})
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestMeasureNeverWritesTheCellsSharedProbeState. C8's baseline file is a
// whole-file rewrite from in-memory state, and until C18 the cell daemon's
// prober was its only writer. A trial is a SECOND process holding it for
// the twenty minutes two cold loads take: writing it back deletes every
// baseline sample and every budget entry the daemon recorded in that
// window, silently, from the record whose whole value is being the one
// multi-sample number the report prints.
func TestMeasureNeverWritesTheCellsSharedProbeState(t *testing.T) {
	r := smallRig(t)
	ctx := context.Background()
	probePath := filepath.Join(r.dir, "state", "model-probe.json")

	// Seed it the way the cell daemon does: a writable prober, several
	// probes, an advancing clock. Building the key by hand would test the
	// test's idea of the key rather than C8's.
	defs, err := router.LoadDefs(r.backends)
	if err != nil {
		t.Fatal(err)
	}
	specs := modelprobe.SpecsFromDefs(defs, "/usr/bin/llama-server")
	base, tick := time.Now().Add(-24*time.Hour), 0
	daemonProber, err := modelprobe.New(modelprobe.Config{
		LlamaSwapURL: r.swap.URL(), StatePath: probePath,
		Specs: func(m string) modelprobe.Spec { return specs[m] },
		Now:   func() time.Time { tick++; return base.Add(time.Duration(tick) * 10 * time.Minute) },
	})
	if err != nil {
		t.Fatal(err)
	}
	r.swap.SetModelState(incName, "ready")
	for n := 0; n < 6; n++ {
		if res := daemonProber.Run(ctx, incName, false); res != nil && res.Note != "" {
			t.Fatalf("seed probe %d: %s", n, res.Note)
		}
	}
	seeded := readOrEmpty(t, probePath)
	if !strings.Contains(seeded, incName) {
		t.Fatalf("the seed did not record the incumbent:\n%s", seeded)
	}

	run := r.runner(func(o *Options) { o.ProbeStatePath = probePath })
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
	r.swap.SetModelState(tr.Def, "ready")
	if _, err := run.Apply(ctx, tr); err != nil {
		t.Fatal(err)
	}
	cmp, err := run.Measure(ctx, tr, nil)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	// It was READ — the daemon's rolling window is on the report, which is
	// why the trial holds this file at all.
	if cmp.Incumbent.Samples <= 0 || cmp.Incumbent.BaselineP50 <= 0 {
		t.Fatalf("the daemon's baseline did not reach the report (samples=%d p50=%v; note %q)",
			cmp.Incumbent.Samples, cmp.Incumbent.BaselineP50, cmp.Incumbent.Note)
	}
	// And it was not written: byte-for-byte what the daemon left.
	if got := readOrEmpty(t, probePath); got != seeded {
		t.Fatalf("the trial rewrote the cell daemon's probe state, discarding whatever it recorded meanwhile:\nwant:\n%s\ngot:\n%s", seeded, got)
	}
}
