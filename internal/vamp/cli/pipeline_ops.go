package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/gallowaysoftware/vibe/internal/vamp"
	"github.com/gallowaysoftware/vibe/internal/vamp/cache"
	"github.com/gallowaysoftware/vibe/internal/vibeclient"
)

// Pipeline-bound operations (run / validate / viz / render) factored out of
// the cobra constructors so callers that already have a *Pipeline in hand —
// notably the Go-DSL `vamp.Main()` entry point — can drive the same code path
// the YAML-loading subcommands use, without re-deriving anything from a
// pipeline file on disk.

// RunOptions carries the per-invocation flags that the `run` subcommand
// exposes. Mirrors the cobra flag set; supplied as a struct so a Go-DSL
// pipeline binary can mount the same logic without depending on cobra
// internals.
type RunOptions struct {
	// Inputs is the raw `KEY=VALUE` slice from --input. Empty when the
	// pipeline declares no inputs or every input has a default.
	Inputs []string
	// RunDir overrides the timestamped default run directory. Empty when
	// resume / autogenerate is desired.
	RunDir string
	// APIURL overrides the vibe control-plane URL ($VIBE_API by default).
	APIURL string
	// Resume names a previous run directory to continue. When set the
	// run dir is taken from this value and the executor enters resume
	// mode (pipeline-drift check + on-disk-output skip).
	Resume string
	// ResumeForce bypasses the pipeline-drift safety check inside
	// Resume mode. Ignored when Resume is empty.
	ResumeForce bool
	// DryRun routes through Executor.DryRun instead of Executor.Run.
	DryRun bool
	// NoCache instructs the executor not to wire up a content-addressed
	// cache store. Per-stage / per-pipeline `cache: false` is honoured
	// inside the executor regardless of this flag.
	NoCache bool
	// InternalRunJob is the marker the detached worker process uses to
	// announce "I am the long-lived background process". Sets up
	// vamp.log + vamp.pid + signal handling. Never set by end users
	// directly; the spawnDetached helper injects it on re-exec.
	InternalRunJob bool
	// NoEnsureServices opts out of the pre-flight that probes each
	// p.RequireService URL and auto-runs `vibe start <name>` for any
	// that aren't reachable + have a parseable `vibe start ...` hint.
	// Default behaviour (false / opt-in) saves the operator from a
	// 3-second-retry-then-fail cascade when SearXNG / Kokoro / etc.
	// just happen to be down. Set true when you want the pipeline to
	// fail loudly without touching services (e.g. testing the
	// retry policy itself).
	NoEnsureServices bool
}

// RunPipeline executes an in-memory pipeline against vibe with the supplied
// options, writing token stream output to stdout. PipelineDir is the
// directory used to resolve relative prompt_file / body_template_file /
// workflow paths for YAML pipelines; Go-DSL pipelines that ship their
// assets via embed.FS may leave it empty. PipelineSource is the bytes that
// land in <run-dir>/pipeline.yaml.snapshot for resume-drift detection;
// callers without raw YAML may leave it nil and the executor falls back to
// marshalling the in-memory pipeline (Phase 6 wiring).
func RunPipeline(ctx context.Context, p *vamp.Pipeline, pipelineDir string, pipelineSource []byte, opts RunOptions, stdout, stderr io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	// Go-DSL pipelines arrive without raw YAML source; marshal the
	// in-memory pipeline back to YAML so the resume-drift snapshot
	// (<run-dir>/pipeline.yaml.snapshot) has a canonical comparison source.
	// The pipeline struct has yaml tags on every field that affects
	// execution; AssetFS is yaml:"-" so it stays out of the snapshot.
	if pipelineSource == nil {
		bytes, err := yaml.Marshal(p)
		if err != nil {
			return fmt.Errorf("marshal pipeline for snapshot: %w", err)
		}
		pipelineSource = bytes
	}
	caps, err := vamp.LoadCapabilities()
	if err != nil {
		return err
	}
	if err := vamp.CheckRunnable(p, pipelineDir, caps); err != nil {
		return err
	}
	inputs, err := parseInputs(opts.Inputs, p)
	if err != nil {
		return err
	}
	if opts.DryRun {
		if opts.Resume != "" || opts.ResumeForce {
			return fmt.Errorf("--dry-run is incompatible with --resume / --resume-force")
		}
		if opts.RunDir != "" {
			return fmt.Errorf("--dry-run is incompatible with --run-dir (no files are written)")
		}
	}
	if opts.Resume != "" && opts.RunDir != "" {
		return fmt.Errorf("--resume and --run-dir are mutually exclusive")
	}
	if opts.ResumeForce && opts.Resume == "" {
		return fmt.Errorf("--resume-force requires --resume")
	}
	if !opts.DryRun && !opts.NoEnsureServices {
		if err := ensureServicesUp(ctx, p, stdout); err != nil {
			return err
		}
	}
	runDir := opts.RunDir
	if opts.Resume != "" {
		runDir, err = filepath.Abs(opts.Resume)
		if err != nil {
			return err
		}
	} else if runDir == "" {
		runDir = filepath.Join(vamp.RunsDir(), time.Now().Local().Format("2006-01-02T15-04-05")+"_"+p.Name)
	}
	logOut := stdout
	var logCloser io.Closer
	if opts.InternalRunJob {
		if err := os.MkdirAll(runDir, 0o755); err != nil {
			return fmt.Errorf("create run dir: %w", err)
		}
		logPath := filepath.Join(runDir, vamp.LogFileName)
		lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return fmt.Errorf("open log file: %w", err)
		}
		logCloser = lf
		logOut = lf
		if err := vamp.WritePidFile(runDir, os.Getpid()); err != nil {
			_ = lf.Close()
			return err
		}
		defer func() {
			if rmErr := vamp.RemovePidFile(runDir); rmErr != nil && !os.IsNotExist(rmErr) {
				slog.Warn("remove pid file", "err", rmErr)
			}
			if logCloser != nil {
				_ = logCloser.Close()
			}
		}()
		var cancel context.CancelFunc
		ctx, cancel = signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
		defer cancel()
	}

	exec := &vamp.Executor{
		Pipeline:       p,
		PipelineDir:    pipelineDir,
		PipelineSource: pipelineSource,
		Capabilities:   caps,
		Vibe:           vibeclient.New(opts.APIURL),
		Inputs:         inputs,
		RunDir:         runDir,
		Log:            logOut,
	}
	if !opts.NoCache && !vamp.EnvCacheDisabled() {
		if store, err := cache.New(cache.DefaultRoot()); err == nil {
			exec.Cache = store
		} else {
			fmt.Fprintf(stderr, "cache: init failed: %v (continuing without cache)\n", err)
		}
	}
	if opts.Resume != "" {
		exec.ResumeDir = runDir
		exec.ResumeForce = opts.ResumeForce
	}
	if opts.DryRun {
		return exec.DryRun(ctx)
	}
	return exec.Run(ctx)
}

// ensureServicesUp probes each p.RequireService URL once with a short
// timeout. For any URL that doesn't answer, auto-runs `vibe start <name>`
// when the service's SetupHint has the `vibe start <name>` shape, then
// re-polls until the URL is reachable (or a 30s deadline expires).
// Services without a parseable hint are surfaced verbatim and the run
// fails — we can't guess how to start an arbitrary external service.
//
// Why: the alternative is a 3-second-retry-then-fail cascade in the
// first webhook stage when SearXNG / Kokoro just happen to be down,
// which forces the operator to learn the existence of `<pipeline>
// activate` after a confusing failure. This makes "first iitn next of
// the day" just work after a reboot.
//
// Cost: ~250ms per declared service when everything's already up
// (one HTTP roundtrip per service, in parallel).
func ensureServicesUp(ctx context.Context, p *vamp.Pipeline, stdout io.Writer) error {
	req := p.Requirements()
	if len(req.Services) == 0 {
		return nil
	}

	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	type result struct {
		idx       int
		reachable bool
	}
	results := make(chan result, len(req.Services))
	for i, svc := range req.Services {
		if svc.URL == "" {
			results <- result{idx: i, reachable: true}
			continue
		}
		go func(i int, url string) {
			results <- result{idx: i, reachable: probeServiceURL(probeCtx, url)}
		}(i, svc.URL)
	}
	reachable := make([]bool, len(req.Services))
	for range req.Services {
		r := <-results
		reachable[r.idx] = r.reachable
	}

	var missing []int
	for i, ok := range reachable {
		if !ok {
			missing = append(missing, i)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	var unstartable []string
	var startNames []string
	for _, i := range missing {
		svc := req.Services[i]
		name := parseVibeStartHint(svc.SetupHint)
		if name == "" {
			unstartable = append(unstartable, fmt.Sprintf("%s — %s", svc.Name, svc.SetupHint))
			continue
		}
		startNames = append(startNames, name)
	}
	if len(unstartable) > 0 {
		return fmt.Errorf("required service(s) unreachable with no `vibe start ...` hint:\n  %s",
			strings.Join(unstartable, "\n  "))
	}

	fmt.Fprintf(stdout, "ensure-services: %d service(s) not up — starting now\n", len(startNames))
	for _, name := range startNames {
		if err := vibeStart(stdout, name, false); err != nil {
			return fmt.Errorf("ensure-services: %w", err)
		}
	}

	readyCtx, readyCancel := context.WithTimeout(ctx, 30*time.Second)
	defer readyCancel()
	var bad []string
	for _, i := range missing {
		svc := req.Services[i]
		if svc.URL == "" {
			continue
		}
		if err := waitForURL(readyCtx, svc.URL); err != nil {
			bad = append(bad, fmt.Sprintf("%s (%s): %v", svc.Name, svc.URL, err))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("ensure-services: %s", strings.Join(bad, "; "))
	}
	return nil
}

// probeServiceURL is the single-shot read-only counterpart to
// waitForURL. Any non-5xx response (incl. 401 / 404) means the server
// is up — we're testing that something answers, not that the URL is
// semantically correct.
func probeServiceURL(ctx context.Context, url string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(r)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode < 500
}

// ValidatePipeline runs the same shape / capability / cross-cutting checks
// that the `vamp validate` subcommand performs against an already-loaded
// pipeline. Pipeline.Validate has already fired by the time you have a
// *Pipeline in hand; ValidatePipeline additionally runs CheckRunnable (which
// needs pipelineDir for prompt_file resolution and the capabilities config
// for the mapping check) and prints the canonical summary stdout line so
// callers can drive `validate` from cobra or from a Go-DSL Main.
func ValidatePipeline(p *vamp.Pipeline, pipelineDir string, skipCaps bool, stdout io.Writer) error {
	var caps *vamp.Capabilities
	if !skipCaps {
		c, err := vamp.LoadCapabilities()
		if err != nil {
			return fmt.Errorf("validate: %w (use --no-capability-check to skip)", err)
		}
		caps = c
	}
	if err := vamp.CheckRunnable(p, pipelineDir, caps); err != nil {
		return err
	}
	stageWord := "stages"
	if len(p.Stages) == 1 {
		stageWord = "stage"
	}
	fmt.Fprintf(stdout, "OK: %s — %d %s\n", p.Name, len(p.Stages), stageWord)
	for i, s := range p.Stages {
		fmt.Fprintf(stdout, "  %d. %s (capability: %s) -> %s\n", i+1, s.ID, s.Capability, s.Output)
	}
	return nil
}

// VizOptions mirrors the cobra flag set for the `viz` subcommand.
type VizOptions struct {
	OutFile    string
	Format     string
	ShowInputs bool
}

// VizPipeline renders a pipeline's Mermaid flowchart, writing to OutFile if
// non-empty or to stdout otherwise. Format is reserved for future formats;
// only "mermaid" (or empty, treated as mermaid) is supported today.
func VizPipeline(p *vamp.Pipeline, opts VizOptions, stdout io.Writer) error {
	switch opts.Format {
	case "", "mermaid":
		// ok
	case "dot":
		return fmt.Errorf("--format=dot is not implemented yet (Phase 1 supports only mermaid)")
	default:
		return fmt.Errorf("--format %q is not supported (allowed: mermaid)", opts.Format)
	}
	rendered := vamp.RenderMermaid(p, vamp.VizOptions{ShowInputs: opts.ShowInputs})
	if opts.OutFile == "" {
		_, err := fmt.Fprint(stdout, rendered)
		return err
	}
	if err := os.WriteFile(opts.OutFile, []byte(rendered), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", opts.OutFile, err)
	}
	return nil
}

// RenderStageOptions mirrors the cobra flag set for the `render` subcommand.
type RenderStageOptions struct {
	Inputs []string
	RunDir string
}

// RenderStageForPipeline renders a single stage's prompt template against
// the pipeline's inputs and optional prior-stage outputs from a run dir,
// writing the result to stdout. Mirrors `vamp render` but operates on an
// in-memory pipeline so Go-DSL binaries can mount the same code path.
func RenderStageForPipeline(p *vamp.Pipeline, pipelineDir, stageID string, opts RenderStageOptions, stdout io.Writer) error {
	var stage *vamp.Stage
	for i := range p.Stages {
		if p.Stages[i].ID == stageID {
			stage = &p.Stages[i]
			break
		}
	}
	if stage == nil {
		ids := make([]string, 0, len(p.Stages))
		for i := range p.Stages {
			ids = append(ids, p.Stages[i].ID)
		}
		return fmt.Errorf("stage %q not found (have: %s)", stageID, strings.Join(ids, ", "))
	}
	inputs, err := parseInputs(opts.Inputs, p)
	if err != nil {
		return err
	}
	absRunDir := ""
	if opts.RunDir != "" {
		absRunDir, err = filepath.Abs(opts.RunDir)
		if err != nil {
			return err
		}
		if info, err := os.Stat(absRunDir); err != nil {
			return fmt.Errorf("run-dir %s: %w", absRunDir, err)
		} else if !info.IsDir() {
			return fmt.Errorf("run-dir %s: not a directory", absRunDir)
		}
	}
	prior, err := buildRenderPrior(p, stage, inputs, absRunDir)
	if err != nil {
		return err
	}
	rendered, err := vamp.RenderStagePrompt(stage, pipelineDir, inputs, prior, absRunDir)
	if err != nil {
		return err
	}
	_, err = fmt.Fprint(stdout, rendered)
	return err
}
