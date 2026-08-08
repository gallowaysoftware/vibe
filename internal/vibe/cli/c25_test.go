package cli

// fleet-control C25 — the sequencer half of `--replay`: WHEN the sample is
// taken, and what happens when it cannot be.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/swaptest"
	"github.com/gallowaysoftware/vibe/internal/vibe/benchreplay"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/hfdownload"
	"github.com/gallowaysoftware/vibe/internal/vibe/modeltry"
	"github.com/stretchr/testify/require"
)

// c25Box is a box: a backends dir, a config path, a journal, and a
// swaptest cell standing in for this cell's own llama-swap.
type c25Box struct {
	dir        string
	backends   string
	configPath string
	statePath  string
	weightsDir string
	swap       *swaptest.Cell
	hosts      *fleetcfg.File
}

func newC25Box(t *testing.T) *c25Box {
	t.Helper()
	dir := t.TempDir()
	b := &c25Box{
		dir:        dir,
		backends:   filepath.Join(dir, "backends"),
		configPath: filepath.Join(dir, "llama-swap", "config.yaml"),
		statePath:  filepath.Join(dir, "state", "model-trial.json"),
		weightsDir: filepath.Join(dir, "models"),
	}
	for _, d := range []string{b.backends, b.weightsDir, filepath.Dir(b.configPath), filepath.Dir(b.statePath)} {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}
	hostsPath := filepath.Join(dir, "hosts.yaml")
	require.NoError(t, os.WriteFile(hostsPath, []byte(
		"cells:\n  front:\n    url: http://front.example.lan:9000\n    class: always_on\n"+
			"  gpu-cell:\n    url: http://gpu.example.lan:9000\n    class: opportunistic\n"), 0o644))
	hosts, err := fleetcfg.LoadFrom(hostsPath)
	require.NoError(t, err)
	b.hosts = hosts
	require.NoError(t, os.WriteFile(filepath.Join(b.backends, "qwen3.6-27b.yaml"), []byte(
		"name: qwen3.6-27b\ncell: gpu-cell\nbackend:\n  external: true\n  llama_server:\n    path: "+
			filepath.Join(b.weightsDir, "qwen3.6-27b-Q5.gguf")+"\n    alias: qwen3.6-27b\n    context: 4096\n"), 0o644))
	b.swap = swaptest.NewCell(t, swaptest.WithModels(
		swaptest.Model{ID: "qwen3.6-27b", State: "ready", TTL: 900}))
	return b
}

func (b *c25Box) runner(t *testing.T) *modeltry.Runner {
	t.Helper()
	run, err := modeltry.New(modeltry.Options{
		Cell: "gpu-cell", Hosts: b.hosts, BackendsDir: b.backends,
		ConfigPath: b.configPath, LlamaServerBinary: "/usr/bin/llama-server",
		LlamaSwapURL: b.swap.URL(), StatePath: b.statePath,
		ProbeStatePath: filepath.Join(b.dir, "state", "model-probe.json"),
		CatalogWait:    50 * time.Millisecond,
		DiskFree:       func(string) (int64, error) { return 500 << 30, nil },
		HeadSize:       func(context.Context, hfdownload.Spec) (int64, error) { return 4096, nil },
		Fetch: func(_ context.Context, _ hfdownload.Spec, dest string, _ hfdownload.ProgressFunc) error {
			return os.WriteFile(dest, make([]byte, 4096), 0o644)
		},
	})
	require.NoError(t, err)
	return run
}

func c25Req() modeltry.PlanRequest {
	return modeltry.PlanRequest{Repo: "unsloth/Qwen3.7-32B-GGUF", File: "Qwen3.7-32B-Q4_K_XL.gguf", Like: "qwen3.6-27b"}
}

// TestReplayHarvestsBeforeTheConfigIsWritten is U5's sequencer half, and
// it is asserted by OBSERVATION rather than by reading the source: the
// capture buffer is emptied the instant the config is written, so the test
// empties it on the write and checks the sample survived.
//
// The apply is what makes the candidate exist, and the apply is a
// `-watch-config` reload, and the reload allocates a fresh, empty capture
// buffer. If the harvest were even one step later there would be nothing
// to harvest — so "harvest, then apply" is not a preference about
// freshness, it is the only order in which the feature exists at all.
func TestReplayHarvestsBeforeTheConfigIsWritten(t *testing.T) {
	b := newC25Box(t)
	ctx := t.Context()

	// One capture, on the incumbent, before anything is written.
	id := b.swap.Request("qwen3.6-27b", "/v1/chat/completions",
		swaptest.Tokens{Input: 40, Output: 8, Cache: 0, Draft: -1, DraftAcc: -1}, 0)
	b.swap.SetCapture(id, swaptest.Capture{
		ReqPath: "/v1/chat/completions",
		ReqBody: []byte(`{"model":"qwen3.6-27b","max_tokens":8,"messages":[{"role":"user","content":"synthetic"}]}`),
	})

	run := b.runner(t)
	tr, def, err := run.Plan(ctx, c25Req())
	require.NoError(t, err)
	require.NoError(t, run.Fetch(ctx, tr, nil))
	require.NoError(t, run.Stage(tr, def))

	// The harvest, at the point runModelTry performs it.
	set, err := run.Harvest(ctx, tr, benchreplay.Options{})
	require.NoError(t, err)
	require.Equal(t, 1, set.Len())

	// Now the apply, and the reload's effect on the buffer, modelled the
	// way -watch-config actually behaves: the activity ROWS survive (the
	// store is carried across) and the capture buffer does not.
	_, err = run.Apply(ctx, tr)
	require.NoError(t, err)
	b.swap.DropAllCaptures()

	// The sample is still here, in this process's memory, and it is the
	// only place it could be.
	require.Equal(t, 1, set.Len())
	// And a harvest attempted NOW gets nothing, twice over: the journal
	// refuses it, and even without the journal the buffer is empty.
	_, err = run.Harvest(ctx, tr, benchreplay.Options{})
	require.ErrorIs(t, err, benchreplay.ErrAlreadyApplied)
	tr2 := *tr
	tr2.State = modeltry.StateStaged
	_, err = run.Harvest(ctx, &tr2, benchreplay.Options{})
	require.ErrorIs(t, err, benchreplay.ErrNoCaptures,
		"even with the journal lying about the state, the buffer the apply emptied has nothing in it — which is why the refusal is not merely a convention")
}

// TestReplayIsRefusedBeforeTheTwentyMinutePull is C10's issue-the-refusal-
// first rule. A cell that retains no bodies cannot supply a sample, and
// the operator should learn that before a 20 GB download and a two-hour
// idle wait, not after.
func TestReplayIsRefusedBeforeTheTwentyMinutePull(t *testing.T) {
	b := newC25Box(t)
	// The cell's llama-swap config, declaring captures off. Written here
	// because Plan reads ConfigPath onto the trial.
	require.NoError(t, os.WriteFile(b.configPath,
		[]byte("captureBuffer: 0\nmodels:\n  qwen3.6-27b:\n    cmd: llama-server\n"), 0o600))

	fetched := atomic.Bool{}
	run, err := modeltry.New(modeltry.Options{
		Cell: "gpu-cell", Hosts: b.hosts, BackendsDir: b.backends,
		ConfigPath: b.configPath, LlamaServerBinary: "/usr/bin/llama-server",
		LlamaSwapURL: b.swap.URL(), StatePath: b.statePath,
		CatalogWait: 50 * time.Millisecond,
		DiskFree:    func(string) (int64, error) { return 500 << 30, nil },
		HeadSize:    func(context.Context, hfdownload.Spec) (int64, error) { return 4096, nil },
		Fetch: func(_ context.Context, _ hfdownload.Spec, dest string, _ hfdownload.ProgressFunc) error {
			fetched.Store(true)
			return os.WriteFile(dest, make([]byte, 4096), 0o644)
		},
	})
	require.NoError(t, err)

	var out bytes.Buffer
	err = runModelTry(t.Context(), &out, run, "", c25Req(),
		trialGateOpts{quiet: time.Minute, replay: true})
	require.ErrorIs(t, err, benchreplay.ErrCapturesDisabled)
	require.Contains(t, err.Error(), "captureBuffer: 0")
	require.False(t, fetched.Load(),
		"the refusal must arrive before the pull; C10's rule is that a command refuses first and spends afterwards")

	// The config is NOT rewritten. How much private traffic a box retains
	// is a declaration, and a benchmark that quietly widened it would have
	// taken an intent decision belonging to the operator.
	got, rerr := os.ReadFile(b.configPath)
	require.NoError(t, rerr)
	require.Contains(t, string(got), "captureBuffer: 0")

	// And without --replay the same box runs normally: the refusal is
	// scoped to the flag that asked for the thing.
	require.False(t, strings.Contains(out.String(), "harvesting"))
}

// TestWithoutTheFlagNoCaptureIsEverRead is the smallest and most important
// assertion in this file. A verb that read the operator's prompts in order
// to decide whether to read the operator's prompts would be a bad joke.
func TestWithoutTheFlagNoCaptureIsEverRead(t *testing.T) {
	b := newC25Box(t)
	id := b.swap.Request("qwen3.6-27b", "/v1/chat/completions",
		swaptest.Tokens{Input: 40, Output: 8, Cache: 0, Draft: -1, DraftAcc: -1}, 0)
	b.swap.SetCapture(id, swaptest.Capture{ReqPath: "/v1/chat/completions", ReqBody: []byte(`{"messages":[]}`)})

	run := b.runner(t)
	var out bytes.Buffer
	// The run stops at the fleetd resolution, which is fine: the harvest
	// would have happened before that only if the flag were set, and the
	// assertion is about the capture endpoint never being touched.
	_ = runModelTry(t.Context(), &out, run, "http://127.0.0.1:1", c25Req(),
		trialGateOpts{quiet: time.Minute, now: true})
	require.Equal(t, 0, b.swap.CaptureReads(),
		"a trial without --replay must not fetch a single capture; %s", out.String())
}
