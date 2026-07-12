package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
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
		noEnsureSvc     bool
		warmFlag        bool
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
				Inputs:           inputFlags,
				RunDir:           runDirFlag,
				APIURL:           apiFlag,
				Resume:           resumeFlag,
				ResumeForce:      resumeForceFlag,
				DryRun:           dryRunFlag,
				NoCache:          noCacheFlag,
				NoEnsureServices: noEnsureSvc,
				Warm:             warmFlag,
				InternalRunJob:   internalRunJob,
			}
			return RunPipeline(ctx, p, filepath.Dir(pipelinePath), pipelineBytes, opts, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringArrayVar(&inputFlags, "input", nil, inputFlagUsage)
	cmd.Flags().StringVar(&runDirFlag, "run-dir", "", "Override run directory (default: timestamped under $XDG_STATE_HOME/vamp/runs/).")
	cmd.Flags().StringVar(&apiFlag, "api", "", "vibe control-plane URL (default: $VIBE_API or http://127.0.0.1:9001).")
	cmd.Flags().StringVar(&resumeFlag, "resume", "", "Resume a previous run from <dir>. Stages whose output files already exist with non-zero size are skipped; missing stages run as usual.")
	cmd.Flags().BoolVar(&resumeForceFlag, "resume-force", false, "With --resume, skip the safety check that errors out when the pipeline file has changed since the run was started.")
	cmd.Flags().BoolVar(&dryRunFlag, "dry-run", false, "Render templates and validate per-stage shape without contacting vibe, an LLM, ComfyUI, ffmpeg, Piper, or YouTube. Prints a per-stage plan and a final error/warning count.")
	cmd.Flags().BoolVar(&detachFlag, "detach", false, "Fork the run into a background `vamp` worker and return immediately with a job id. Use `vamp runs ls`, `vamp logs <id>`, `vamp cancel <id>` to drive it.")
	cmd.Flags().BoolVar(&noCacheFlag, "no-cache", false, "Disable the content-addressed cache for this run; overrides per-pipeline / per-stage `cache: true` defaults.")
	cmd.Flags().BoolVar(&noEnsureSvc, "no-ensure-services", false, "Skip the pre-flight probe + auto-start of declared RequireService URLs. Default behaviour auto-runs `vibe start <name>` for any unreachable service whose setup_hint matches that shape, sparing the operator a 3-second-retry-then-fail cascade in the first webhook stage.")
	cmd.Flags().BoolVar(&warmFlag, "warm", false, "Before DAG execution starts, ensure every capability the pipeline declares in PARALLEL (activation + a 1-token streaming warm probe for LLM capabilities) so router cold starts overlap instead of serializing at each capability's first stage. Prints one line per capability with resolve + warm durations.")
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
	// Capture stderr to a pipe so a fast-exit failure (the child errors
	// out before it can write to vamp.log) surfaces to the operator
	// instead of vanishing into /dev/null. The pipe is drained into a
	// bounded buffer and either reported on early exit OR discarded
	// once the child has clearly survived past the verification window.
	stderrPipe, err := c.StderrPipe()
	if err != nil {
		return fmt.Errorf("attach child stderr: %w", err)
	}
	if err := c.Start(); err != nil {
		return fmt.Errorf("spawn worker: %w", err)
	}
	stderrBuf := &bytes.Buffer{}
	stderrDone := make(chan struct{})
	go func() {
		// Cap at 8 KiB so a chatty child doesn't bloat parent memory.
		_, _ = io.CopyN(stderrBuf, stderrPipe, 8192)
		_, _ = io.Copy(io.Discard, stderrPipe)
		close(stderrDone)
	}()
	// Verify the child actually got past argv parsing / pre-flight and
	// is running the long-lived worker loop. The signal we wait on is
	// vamp.pid: the worker writes it in RunPipeline after opening the
	// log file, so its appearance proves the child reached a known-
	// good state. If the process exits before that — or doesn't write
	// the pid within the timeout — surface the captured stderr to the
	// operator so the failure isn't silent.
	pidPath := filepath.Join(runDir, vamp.PidFileName)
	startupDeadline := time.Now().Add(2 * time.Second)
	waitDone := make(chan error, 1)
	go func() { waitDone <- c.Wait() }()
	pidReady := false
	for time.Now().Before(startupDeadline) {
		if _, err := os.Stat(pidPath); err == nil {
			pidReady = true
			break
		}
		select {
		case werr := <-waitDone:
			// Child exited before the pid file appeared. Two cases:
			//   - werr == nil (exit status 0): the run completed
			//     successfully within the 2s window AND between
			//     writing+removing its pid file. Treat as success;
			//     the operator should `vamp logs` / `vamp runs show`
			//     to inspect the (fast) result.
			//   - werr != nil OR stderr captured: a real startup
			//     failure (bad argv, missing file, etc.) — surface
			//     so the failure isn't silent.
			<-stderrDone
			msg := strings.TrimSpace(stderrBuf.String())
			if werr == nil && msg == "" {
				// Exit 0 before the pid file appeared can mean the run
				// finished+cleaned up inside the window — but a child that
				// mis-parsed argv and returned nil without starting the
				// pipeline also lands here. Require positive evidence (a
				// vamp.log or pipeline.json in the run dir) so we don't
				// print a phantom run id for a job that left nothing for
				// `vamp logs` to read.
				_, logErr := os.Stat(filepath.Join(runDir, vamp.LogFileName))
				_, recErr := os.Stat(filepath.Join(runDir, "pipeline.json"))
				if logErr == nil || recErr == nil {
					pidReady = true // treat as success
					break
				}
				return fmt.Errorf("worker exited 0 without starting a run (no %s or pipeline.json in %s)", vamp.LogFileName, runDir)
			}
			if msg == "" && werr != nil {
				msg = werr.Error()
			}
			if msg == "" {
				msg = "worker exited with no output"
			}
			return fmt.Errorf("detached worker exited before startup: %s", msg)
		case <-time.After(100 * time.Millisecond):
		}
		if pidReady {
			break
		}
	}
	if !pidReady {
		// Worker is still alive but slow to write the pid file. Warn
		// instead of failing — slow disk / heavy fork can take longer
		// than 2s — but the operator should know to check.
		fmt.Fprintf(cmd.ErrOrStderr(), "[warn] detached worker did not write %s within 2s; check `vamp runs ls` shortly\n", vamp.PidFileName)
	}
	// Releasing detaches the process from the parent's reaper so the
	// worker survives parent exit. After this, the goroutine that
	// called Wait() will see the child as "released" and unblock with
	// an error we deliberately ignore (process.Release semantics).
	_ = c.Process.Release()
	out := cmd.OutOrStdout()
	id := filepath.Base(runDir)
	// First line is the bare run id so `id=$(vamp run --detach ...)` style
	// capture keeps working; the rest are copy-pasteable next steps.
	fmt.Fprintln(out, id)
	fmt.Fprintf(out, "run dir: %s\n", runDir)
	fmt.Fprintf(out, "follow:  vamp logs %s -f\n", id)
	fmt.Fprintf(out, "cancel:  vamp cancel %s\n", id)
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
		// Required inputs need a value from the CLI or a non-empty
		// default. Empty-string default + Required is treated as
		// "user must supply" — empty default doesn't satisfy the
		// requirement.
		if spec.Required && spec.Default == "" {
			return nil, fmt.Errorf("input %q is required (--input %s=...)", name, name)
		}
		// Even an empty default registers the input as "" in the
		// map, so prompt templates can safely reference
		// `.inputs.<name>` without tripping the missingkey=error
		// safety net. Optional inputs with no caller-supplied value
		// and no default land here too: declared, present, empty.
		if spec.Default == "" {
			inputs[name] = ""
			continue
		}
		rendered, err := renderInputDefault(name, spec.Default, inputs)
		if err != nil {
			return nil, err
		}
		inputs[name] = rendered
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
