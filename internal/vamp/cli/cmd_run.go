package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"text/template"
	"time"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vamp"
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
			if detachFlag {
				if dryRunFlag {
					return fmt.Errorf("--detach is incompatible with --dry-run")
				}
				if internalRunJob {
					return fmt.Errorf("--detach and --%s are mutually exclusive", internalRunJobFlag)
				}
				// Cheap load to recover the pipeline name for the run-dir
				// basename. The child re-loads in the worker.
				p, err := vamp.LoadPipeline(pipelinePath)
				if err != nil {
					return err
				}
				return spawnDetached(cmd, runDirFlag, resumeFlag, p.Name)
			}
			p, err := vamp.LoadPipeline(pipelinePath)
			if err != nil {
				return err
			}
			pipelineBytes, err := os.ReadFile(pipelinePath)
			if err != nil {
				return fmt.Errorf("read pipeline file: %w", err)
			}
			opts := RunOptions{
				Inputs:         inputFlags,
				RunDir:         runDirFlag,
				APIURL:         apiFlag,
				Resume:         resumeFlag,
				ResumeForce:    resumeForceFlag,
				DryRun:         dryRunFlag,
				NoCache:        noCacheFlag,
				InternalRunJob: internalRunJob,
			}
			return RunPipeline(ctx, p, filepath.Dir(pipelinePath), pipelineBytes, opts, cmd.OutOrStdout(), cmd.ErrOrStderr())
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

// spawnDetached re-execs the current binary with --internal-run-job (so the
// child sets up logging + pid file + signal handling) in a fresh session.
// The parent prints the run-dir basename (job id) and exits. Used by both
// the YAML `vamp run --detach` path and Go-DSL pipeline binaries that
// re-export `run --detach`; `pipelineName` provides the run-dir basename
// without forcing the caller to re-load the pipeline source.
//
// The child inherits the full argv minus --detach and gets --run-dir pinned
// to the resolved run dir so its log+pid land in a directory the parent has
// already chosen. We deliberately don't double-fork (vibe's daemon
// supervisor uses the same setsid-only approach); the worker process is
// the long-lived one, no init-reparent intermediary needed.
func spawnDetached(cmd *cobra.Command, runDirFlag, resumeFlag, pipelineName string) error {
	runDir := runDirFlag
	switch {
	case resumeFlag != "":
		abs, err := filepath.Abs(resumeFlag)
		if err != nil {
			return err
		}
		runDir = abs
	case runDir == "":
		runDir = filepath.Join(vamp.RunsDir(), time.Now().Local().Format("2006-01-02T15-04-05")+"_"+pipelineName)
	}
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return fmt.Errorf("create run dir: %w", err)
	}
	// Re-build argv for the child. We start from os.Args and:
	//   - strip --detach (boolean flag, may appear with =true/=false)
	//   - inject --<internalRunJobFlag>
	//   - inject --run-dir <runDir> if not already present
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
		case strings.HasPrefix(a, "--run-dir="):
			hasRunDir = true
			childArgs = append(childArgs, a)
		default:
			childArgs = append(childArgs, a)
		}
	}
	// Only inject --run-dir when there's no --resume — they're mutually
	// exclusive at the RunPipeline level. Without this guard a
	// `vamp run --detach --resume <dir>` invocation spawns a child whose
	// argv contains BOTH flags; the child immediately errors with
	// "--resume and --run-dir are mutually exclusive" and exits before
	// touching vamp.log or vamp.pid — silently, because the detached
	// worker's stderr is /dev/null.
	if !hasRunDir && resumeFlag == "" {
		childArgs = append(childArgs, "--run-dir", runDir)
	}
	childArgs = append(childArgs, "--"+internalRunJobFlag)
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate vamp executable: %w", err)
	}
	c := exec.Command(exe, childArgs...)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	c.Stdin = nil
	c.Stdout = nil
	c.Stderr = nil
	if err := c.Start(); err != nil {
		return fmt.Errorf("spawn worker: %w", err)
	}
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
