package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vamp"
	"github.com/gallowaysoftware/vibe/internal/vibeclient"
)

func runCmd() *cobra.Command {
	var (
		inputFlags []string
		runDirFlag string
		apiFlag    string
	)
	cmd := &cobra.Command{
		Use:   "run <pipeline.yaml>",
		Short: "Execute a pipeline end-to-end.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			pipelinePath, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			p, err := vamp.LoadPipeline(pipelinePath)
			if err != nil {
				return err
			}
			caps, err := vamp.LoadCapabilities()
			if err != nil {
				return err
			}
			inputs, err := parseInputs(inputFlags, p)
			if err != nil {
				return err
			}
			runDir := runDirFlag
			if runDir == "" {
				runDir = filepath.Join(vamp.RunsDir(), time.Now().Local().Format("2006-01-02T15-04-05")+"_"+p.Name)
			}

			exec := &vamp.Executor{
				Pipeline:     p,
				PipelineDir:  filepath.Dir(pipelinePath),
				Capabilities: caps,
				Vibe:         vibeclient.New(apiFlag),
				Inputs:       inputs,
				RunDir:       runDir,
				Log:          os.Stdout,
			}
			return exec.Run(ctx)
		},
	}
	cmd.Flags().StringSliceVar(&inputFlags, "input", nil, "Pipeline input as KEY=VALUE; can repeat.")
	cmd.Flags().StringVar(&runDirFlag, "run-dir", "", "Override run directory (default: timestamped under $XDG_STATE_HOME/vamp/runs/).")
	cmd.Flags().StringVar(&apiFlag, "api", "", "vibe control-plane URL (default: $VIBE_API or http://127.0.0.1:9001).")
	return cmd
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
			inputs[name] = spec.Default
			continue
		}
		if spec.Required {
			return nil, fmt.Errorf("input %q is required (--input %s=...)", name, name)
		}
	}
	return inputs, nil
}
