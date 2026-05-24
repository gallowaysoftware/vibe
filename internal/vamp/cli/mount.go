package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/buildinfo"
	"github.com/gallowaysoftware/vibe/internal/vamp"
)

// PipelineFactory produces the *Pipeline a Go-DSL binary's subcommand should
// operate on. Called once per subcommand invocation so a factory that reads
// from os.Environ or the cobra flag set can react to the current invocation
// rather than baking in process-start state. The returned pipeline must
// already be Validate'd (Pipeline.Build does this).
type PipelineFactory func() (*vamp.Pipeline, error)

// BuildRoot returns the cobra root command a Go-DSL pipeline binary's
// main() should Execute(). It mounts the four pipeline-bound subcommands
// (run / validate / viz / render) wired to the supplied factory, plus
// every pipeline-independent subcommand the standalone `vamp` binary
// exposes (list / capabilities / runs / jobs / logs / cancel / confirm /
// schema / diff / cache). Use the binary's own command name (typically
// filepath.Base(os.Args[0]) is fine) so the help text matches what the
// user types.
func BuildRoot(name string, factory PipelineFactory) *cobra.Command {
	root := &cobra.Command{
		Use:           name,
		Short:         "vamp pipeline binary (Go-DSL)",
		Long:          "vamp pipeline binary. Built from Go code using github.com/gallowaysoftware/vibe/vamp; mounts the same subcommands as the standalone vamp binary, but with the pipeline already in memory.",
		Version:       buildinfo.String(),
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.SetVersionTemplate("{{.Version}}\n")
	root.AddCommand(
		runCmdInMemory(factory),
		validateCmdInMemory(factory),
		vizCmdInMemory(factory),
		renderCmdInMemory(factory),
		requirementsCmdInMemory(factory),
		activateCmdInMemory(factory),
		doctorCmdInMemory(factory),
		listCmd(),
		capabilitiesCmd(),
		runsCmd(),
		jobsCmd(),
		logsCmd(),
		cancelCmd(),
		confirmCmd(),
		schemaCmd(),
		diffCmd(),
		cacheCmd(),
	)
	return root
}

func runCmdInMemory(factory PipelineFactory) *cobra.Command {
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
		internalRunJob  bool
	)
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Execute the embedded pipeline end-to-end.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if detachFlag {
				if dryRunFlag {
					return fmt.Errorf("--detach is incompatible with --dry-run")
				}
				if internalRunJob {
					return fmt.Errorf("--detach and --%s are mutually exclusive", internalRunJobFlag)
				}
				p, err := factory()
				if err != nil {
					return err
				}
				return spawnDetached(cmd, runDirFlag, resumeFlag, p.Name)
			}
			p, err := factory()
			if err != nil {
				return err
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
				InternalRunJob:   internalRunJob,
			}
			// pipelineDir / pipelineSource left zero: the Go-DSL path
			// resolves assets through Stage.AssetFS (Phase 3) and the
			// snapshot is marshalled from the in-memory pipeline
			// (Phase 6). Until those phases land RunPipeline still
			// works because no current code path requires a non-empty
			// PipelineDir when AssetFS is set.
			return RunPipeline(ctx, p, "", nil, opts, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringArrayVar(&inputFlags, "input", nil, "Pipeline input as KEY=VALUE; can repeat. Commas inside the value are NOT split.")
	cmd.Flags().StringVar(&runDirFlag, "run-dir", "", "Override run directory (default: timestamped under $XDG_STATE_HOME/vamp/runs/).")
	cmd.Flags().StringVar(&apiFlag, "api", "", "vibe control-plane URL (default: $VIBE_API or http://127.0.0.1:9001).")
	cmd.Flags().StringVar(&resumeFlag, "resume", "", "Resume a previous run from <dir>.")
	cmd.Flags().BoolVar(&resumeForceFlag, "resume-force", false, "With --resume, skip the pipeline-drift safety check.")
	cmd.Flags().BoolVar(&dryRunFlag, "dry-run", false, "Render templates and validate per-stage shape without contacting vibe / LLM / ComfyUI / ffmpeg / Piper / YouTube.")
	cmd.Flags().BoolVar(&detachFlag, "detach", false, "Fork the run into a background worker and return a job id.")
	cmd.Flags().BoolVar(&noCacheFlag, "no-cache", false, "Disable the content-addressed cache for this run.")
	cmd.Flags().BoolVar(&noEnsureSvc, "no-ensure-services", false, "Skip the pre-flight probe + auto-start of declared RequireService URLs. Default behaviour auto-runs `vibe start <name>` for any unreachable service whose setup_hint matches that shape.")
	cmd.Flags().BoolVar(&internalRunJob, internalRunJobFlag, false, "Internal: detached-worker marker; do not invoke manually.")
	_ = cmd.Flags().MarkHidden(internalRunJobFlag)
	return cmd
}

func validateCmdInMemory(factory PipelineFactory) *cobra.Command {
	var skipCaps bool
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate the embedded pipeline (does not run it).",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := factory()
			if err != nil {
				return err
			}
			return ValidatePipeline(p, "", skipCaps, cmd.OutOrStdout())
		},
	}
	cmd.Flags().BoolVar(&skipCaps, "no-capability-check", false, "Skip loading capabilities.yaml and verifying every stage's capability is mapped.")
	return cmd
}

func vizCmdInMemory(factory PipelineFactory) *cobra.Command {
	var (
		outFlag        string
		formatFlag     string
		showInputsFlag bool
	)
	cmd := &cobra.Command{
		Use:   "viz",
		Short: "Emit a Mermaid flowchart of the embedded pipeline's DAG.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := factory()
			if err != nil {
				return err
			}
			return VizPipeline(p, VizOptions{OutFile: outFlag, Format: formatFlag, ShowInputs: showInputsFlag}, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&outFlag, "out", "", "Write rendered Mermaid to this file instead of stdout.")
	cmd.Flags().StringVar(&formatFlag, "format", "mermaid", "Output format. Only \"mermaid\" is supported.")
	cmd.Flags().BoolVar(&showInputsFlag, "show-inputs", false, "Include declared inputs as a subgraph block.")
	return cmd
}

func renderCmdInMemory(factory PipelineFactory) *cobra.Command {
	var (
		inputFlags []string
		runDirFlag string
	)
	cmd := &cobra.Command{
		Use:   "render <stage_id>",
		Short: "Render a single stage's prompt template (no LLM call).",
		Long:  renderCmdLongDescription,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := factory()
			if err != nil {
				return err
			}
			return RenderStageForPipeline(p, "", args[0], RenderStageOptions{Inputs: inputFlags, RunDir: runDirFlag}, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringArrayVar(&inputFlags, "input", nil, "Pipeline input as KEY=VALUE; can repeat.")
	cmd.Flags().StringVar(&runDirFlag, "run-dir", "", "Optional: prior run directory whose stage outputs should seed the template.")
	return cmd
}
