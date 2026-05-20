package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/template"
	"time"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vamp"
	"github.com/gallowaysoftware/vibe/internal/vamp/cache"
	"github.com/gallowaysoftware/vibe/internal/vibeclient"
)

// internalRunJobFlag is the hidden flag the foreground `vamp run --detach`
// spawner uses to wake the child process up in "worker" mode. When set,
// the child knows it's the long-lived background process and is
// responsible for:
//   - redirecting Executor.Log to <run-dir>/vamp.log
//   - writing its own pid to <run-dir>/vamp.pid (so `jobs ls` / `cancel`
//     can find it)
//   - wiring signal.NotifyContext to translate SIGTERM/SIGINT into ctx
//     cancellation (the executor handles the rest)
//   - removing the pid file on exit so post-mortem `jobs ls` sees the
//     correct terminal state.
//
// The flag name is deliberately ugly + hidden; users should never type it.
const internalRunJobFlag = "internal-run-job"

func runCmd() *cobra.Command {
	var (
		inputFlags      []string
		runDirFlag      string
		apiFlag         string
		resumeFlag      string
		resumeForceFlag bool
		dryRunFlag      bool
		detachFlag      bool
		noCacheFlag     bool
		internalRunJob  bool
	)
	cmd := &cobra.Command{
		Use:               "run <pipeline.yaml>",
		Short:             "Execute a pipeline end-to-end.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completePipelineFiles,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			pipelinePath, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			// --detach forks a fresh worker process and exits before
			// loading the pipeline; we resolve the run-dir up-front so
			// the parent can print a job id and the child knows where
			// to land. We also bail on flag combinations that don't
			// make sense for a detached run.
			if detachFlag {
				if dryRunFlag {
					return fmt.Errorf("--detach is incompatible with --dry-run")
				}
				if internalRunJob {
					return fmt.Errorf("--detach and --%s are mutually exclusive", internalRunJobFlag)
				}
				return spawnDetached(cmd, pipelinePath, runDirFlag, resumeFlag)
			}
			p, err := vamp.LoadPipeline(pipelinePath)
			if err != nil {
				return err
			}
			// Read the raw bytes too: the Executor writes them verbatim
			// into <run-dir>/pipeline.yaml.snapshot at start (so resume
			// can later detect that the pipeline file changed) and uses
			// them as the comparison source when --resume is set.
			pipelineBytes, err := os.ReadFile(pipelinePath)
			if err != nil {
				return fmt.Errorf("read pipeline file: %w", err)
			}
			caps, err := vamp.LoadCapabilities()
			if err != nil {
				return err
			}
			// Fail-fast before vibe is involved: unmapped capabilities,
			// missing prompt_files, and broken template syntax all surface
			// here rather than after the daemon has spent seconds activating
			// a profile. Inline prompt parsing is already covered by
			// Pipeline.Validate; CheckRunnable adds the cross-cutting checks
			// that need either the pipeline dir (for prompt_file resolution)
			// or the capabilities config (for the mapping check).
			if err := vamp.CheckRunnable(p, filepath.Dir(pipelinePath), caps); err != nil {
				return err
			}
			inputs, err := parseInputs(inputFlags, p)
			if err != nil {
				return err
			}
			// --dry-run rejects flags that only make sense when actually
			// running the pipeline. Resume / resume-force imply mutating
			// disk state for a real run; --run-dir overrides the
			// destination of artifacts dry-run never writes. Each
			// combination is rejected explicitly so the user sees which
			// flag conflicts rather than a silent override.
			if dryRunFlag {
				if resumeFlag != "" || resumeForceFlag {
					return fmt.Errorf("--dry-run is incompatible with --resume / --resume-force")
				}
				if runDirFlag != "" {
					return fmt.Errorf("--dry-run is incompatible with --run-dir (no files are written)")
				}
			}
			// Resume short-circuits the timestamped-dir computation:
			// the resume directory IS the run directory. --run-dir is
			// rejected alongside --resume because the two flags would
			// fight over the same field with conflicting semantics
			// (override-fresh vs. reuse-existing).
			if resumeFlag != "" && runDirFlag != "" {
				return fmt.Errorf("--resume and --run-dir are mutually exclusive")
			}
			if resumeForceFlag && resumeFlag == "" {
				return fmt.Errorf("--resume-force requires --resume")
			}
			runDir := runDirFlag
			if resumeFlag != "" {
				runDir, err = filepath.Abs(resumeFlag)
				if err != nil {
					return err
				}
			} else if runDir == "" {
				runDir = filepath.Join(vamp.RunsDir(), time.Now().Local().Format("2006-01-02T15-04-05")+"_"+p.Name)
			}

			// Worker-mode setup: when we were spawned by `--detach`, we
			// own the long-lived process. Pin our pid into the run dir,
			// redirect logs to vamp.log, and translate SIGTERM/SIGINT
			// into ctx cancellation so `vamp cancel` works.
			//
			// We do this BEFORE constructing the executor so any
			// MkdirAll / pid-file write failure surfaces before we
			// start spending time on Vibe. The pid file is best-effort
			// cleaned up in a defer; the log file we keep open until
			// Run returns so deferred timing output makes it to disk.
			var logCloser io.Closer
			logOut := io.Writer(os.Stdout)
			if internalRunJob {
				if err := os.MkdirAll(runDir, 0o755); err != nil {
					return fmt.Errorf("create run dir: %w", err)
				}
				logPath := filepath.Join(runDir, vamp.LogFileName)
				// O_APPEND so a worker that gets re-attached (future)
				// or a crash-restart cycle doesn't truncate the prior
				// log. The append cost is negligible at our write rates.
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
				// Pid-file cleanup runs on every exit path. We deliberately
				// remove BEFORE closing the log so a tailing `vamp logs -f`
				// observes the pid-gone signal and drains the final bytes.
				defer func() {
					if rmErr := vamp.RemovePidFile(runDir); rmErr != nil && !os.IsNotExist(rmErr) {
						slog.Warn("remove pid file", "err", rmErr)
					}
					if logCloser != nil {
						_ = logCloser.Close()
					}
				}()
				// Translate SIGTERM / SIGINT into ctx cancellation. The
				// executor honors ctx.Done() everywhere it can block;
				// writePipelineJSON renders ctx.Canceled as
				// status="canceled" in pipeline.json, which is what
				// `jobs show` will report.
				var cancel context.CancelFunc
				ctx, cancel = signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
				defer cancel()
			}

			exec := &vamp.Executor{
				Pipeline:       p,
				PipelineDir:    filepath.Dir(pipelinePath),
				PipelineSource: pipelineBytes,
				Capabilities:   caps,
				Vibe:           vibeclient.New(apiFlag),
				Inputs:         inputs,
				RunDir:         runDir,
				Log:            logOut,
			}
			// Wire the content-addressed cache unless the user explicitly
			// opted out via --no-cache or VAMP_NO_CACHE=1. Per-pipeline /
			// per-stage `cache: false` is honoured inside the executor; the
			// CLI surface only decides whether to instantiate the store at
			// all. A store init failure is a soft failure: log via slog
			// inside the executor (Executor.Cache stays nil) and run as if
			// caching were disabled.
			if !noCacheFlag && !vamp.EnvCacheDisabled() {
				if store, err := cache.New(cache.DefaultRoot()); err == nil {
					exec.Cache = store
				} else {
					fmt.Fprintf(cmd.ErrOrStderr(), "cache: init failed: %v (continuing without cache)\n", err)
				}
			}
			if resumeFlag != "" {
				exec.ResumeDir = runDir
				exec.ResumeForce = resumeForceFlag
			}
			if dryRunFlag {
				// Dry-run never touches vibe, never spawns subprocesses,
				// and never writes outputs. We skip vibe-client + Run
				// entirely; the executor's other fields (Pipeline,
				// Capabilities, Inputs, RunDir, Log) are still consulted
				// by DryRun.
				return exec.DryRun(ctx)
			}
			return exec.Run(ctx)
		},
	}
	cmd.Flags().StringArrayVar(&inputFlags, "input", nil, "Pipeline input as KEY=VALUE; can repeat. Commas inside the value are NOT split.")
	cmd.Flags().StringVar(&runDirFlag, "run-dir", "", "Override run directory (default: timestamped under $XDG_STATE_HOME/vamp/runs/).")
	cmd.Flags().StringVar(&apiFlag, "api", "", "vibe control-plane URL (default: $VIBE_API or http://127.0.0.1:9001).")
	cmd.Flags().StringVar(&resumeFlag, "resume", "", "Resume a previous run from <dir>. Stages whose output files already exist with non-zero size are skipped; missing stages run as usual.")
	cmd.Flags().BoolVar(&resumeForceFlag, "resume-force", false, "With --resume, skip the safety check that errors out when the pipeline file has changed since the run was started.")
	cmd.Flags().BoolVar(&dryRunFlag, "dry-run", false, "Render templates and validate per-stage shape without contacting vibe, an LLM, ComfyUI, ffmpeg, Piper, or YouTube. Prints a per-stage plan and a final error/warning count.")
	cmd.Flags().BoolVar(&detachFlag, "detach", false, "Fork the run into a background `vamp` worker and return immediately with a job id. Use `vamp jobs ls`, `vamp logs <id>`, `vamp cancel <id>` to drive it.")
	cmd.Flags().BoolVar(&noCacheFlag, "no-cache", false, "Disable the content-addressed cache for this run; overrides per-pipeline / per-stage `cache: true` defaults.")
	cmd.Flags().BoolVar(&internalRunJob, internalRunJobFlag, false, "Internal: marks this process as the detached worker spawned by --detach. Sets up vamp.log + vamp.pid in the run dir. Do not invoke manually.")
	_ = cmd.Flags().MarkHidden(internalRunJobFlag)
	return cmd
}

// spawnDetached re-execs the current vamp binary with --internal-run-job
// (so the child sets up logging + pid file + signal handling) in a fresh
// session. The parent prints the run-dir basename (job id) and exits.
//
// The child inherits the full argv minus --detach and gets --run-dir
// pinned to the resolved run dir so its log+pid land in a directory the
// parent has already chosen. We deliberately don't double-fork (vibe's
// daemon supervisor uses the same setsid-only approach); the worker
// process is the long-lived one, no init-reparent intermediary needed.
func spawnDetached(cmd *cobra.Command, pipelinePath, runDirFlag, resumeFlag string) error {
	// Resolve the run-dir up-front so the parent prints a stable id and
	// the child doesn't have to mint a fresh timestamp (two processes
	// minting independently would race on second-resolution boundaries).
	runDir := runDirFlag
	switch {
	case resumeFlag != "":
		abs, err := filepath.Abs(resumeFlag)
		if err != nil {
			return err
		}
		runDir = abs
	case runDir == "":
		// We need the pipeline name to form the basename. Cheaper to
		// parse it than to scan args looking for a name field; the
		// child will load it again anyway.
		p, err := vamp.LoadPipeline(pipelinePath)
		if err != nil {
			return err
		}
		runDir = filepath.Join(vamp.RunsDir(), time.Now().Local().Format("2006-01-02T15-04-05")+"_"+p.Name)
	}
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return fmt.Errorf("create run dir: %w", err)
	}
	// Re-build argv for the child. We start from os.Args and:
	//   - strip --detach (boolean flag, may appear with =true/=false)
	//   - inject --<internalRunJobFlag>
	//   - inject --run-dir <runDir> if not already present
	// We never strip --run-dir if the user supplied it; we only fill
	// the gap.
	childArgs := make([]string, 0, len(os.Args)+2)
	hasRunDir := false
	for i := 1; i < len(os.Args); i++ {
		a := os.Args[i]
		switch {
		case a == "--detach":
			continue
		case strings.HasPrefix(a, "--detach="):
			continue
		case a == "--run-dir":
			hasRunDir = true
			childArgs = append(childArgs, a)
			// next arg is the value; the normal loop iteration
			// picks it up unmodified.
		case strings.HasPrefix(a, "--run-dir="):
			hasRunDir = true
			childArgs = append(childArgs, a)
		default:
			childArgs = append(childArgs, a)
		}
	}
	if !hasRunDir {
		childArgs = append(childArgs, "--run-dir", runDir)
	}
	childArgs = append(childArgs, "--"+internalRunJobFlag)
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate vamp executable: %w", err)
	}
	c := exec.Command(exe, childArgs...)
	// Setsid puts the worker in its own session so closing the parent
	// terminal doesn't SIGHUP it; mirrors `vibe daemon`'s spawn pattern
	// in internal/vibe/cli/spawn.go. nil std streams + the child's own
	// vamp.log open keep the worker fully detached from the parent's tty.
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	c.Stdin = nil
	c.Stdout = nil
	c.Stderr = nil
	if err := c.Start(); err != nil {
		return fmt.Errorf("spawn worker: %w", err)
	}
	// Release detaches the parent's Wait responsibility — we want the
	// worker to outlive this process and be re-parented to init.
	_ = c.Process.Release()
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, filepath.Base(runDir))
	fmt.Fprintf(out, "run dir: %s\n", runDir)
	return nil
}

func parseInputs(flags []string, p *vamp.Pipeline) (map[string]string, error) {
	inputs := make(map[string]string)
	for _, f := range flags {
		idx := strings.Index(f, "=")
		if idx <= 0 {
			return nil, fmt.Errorf("--input %q must be KEY=VALUE", f)
		}
		inputs[f[:idx]] = f[idx+1:]
	}
	for name, spec := range p.Inputs {
		if _, ok := inputs[name]; ok {
			continue
		}
		if spec.Default != "" {
			// Defaults are template-rendered against the inputs collected so
			// far so an input can substitute another (e.g.
			// `lesson_root: { default: "~/.../Module_{{ .inputs.module_num }}" }`
			// where module_num is a separate required input). Single-pass:
			// defaults that depend on OTHER defaults aren't supported yet —
			// add a topological-sort pass here if a pipeline ever needs it.
			rendered, err := renderInputDefault(name, spec.Default, inputs)
			if err != nil {
				return nil, err
			}
			inputs[name] = rendered
			continue
		}
		if spec.Required {
			return nil, fmt.Errorf("input %q is required (--input %s=...)", name, name)
		}
	}
	return inputs, nil
}

// renderInputDefault evaluates an InputSpec.Default as a Go text/template
// against the inputs already resolved. Empty / non-templated defaults short-
// circuit (no rendering overhead). The template binding mirrors what every
// stage's prompt template sees: `.inputs.<name>` for other inputs.
func renderInputDefault(name, raw string, resolved map[string]string) (string, error) {
	if !strings.Contains(raw, "{{") {
		return raw, nil
	}
	tmpl, err := template.New("input:" + name).Parse(raw)
	if err != nil {
		return "", fmt.Errorf("input %q: parse default template: %w", name, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]any{"inputs": resolved}); err != nil {
		return "", fmt.Errorf("input %q: render default: %w", name, err)
	}
	return buf.String(), nil
}
