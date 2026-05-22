package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vamp"
)

// requirementsCmdInMemory exposes the embedded pipeline's runtime contract:
// capabilities (auto-derived from stages), required external services,
// inputs, GPU/disk hints, free-form notes. Tooling that has to set up a
// host before invoking the binary (vibe doctor, CI smoke tests, README
// generators) reads the JSON form of this command's output. The default
// human format is for the operator who runs `mybinary requirements` to
// eyeball what they need to install.
func requirementsCmdInMemory(factory PipelineFactory) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "requirements",
		Short: "Report the runtime resources this pipeline needs (capabilities, services, inputs, hardware hints).",
		Long: `Report the runtime resources this pipeline needs:

  - vibe capabilities (auto-derived from stage definitions)
  - non-vibe HTTP services (SearXNG, Kokoro-FastAPI, …) — declared by the pipeline author
  - declared CLI inputs and their defaults
  - GPU / disk footprint hints and free-form notes

Designed to be consumed by tooling (vibe doctor, CI preflight checks)
that has to set the host up before invoking the binary. The default
human-readable format is for operators running the binary by hand;
` + "`" + `--format json` + "`" + ` emits the machine-readable form.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := factory()
			if err != nil {
				return err
			}
			req := p.Requirements()
			switch strings.ToLower(format) {
			case "json":
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(req)
			case "", "text":
				return printRequirementsText(cmd, req)
			default:
				return fmt.Errorf("unknown format %q (supported: json, text)", format)
			}
		},
	}
	cmd.Flags().StringVar(&format, "format", "text", "Output format: text (default, human) or json (machine).")
	return cmd
}

func printRequirementsText(cmd *cobra.Command, req vamp.Requirements) error {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Pipeline: %s\n", req.Pipeline)
	if req.Description != "" {
		fmt.Fprintf(out, "  %s\n", req.Description)
	}

	if len(req.Capabilities) > 0 {
		fmt.Fprintln(out, "\nCapabilities (resolved against your vibe capabilities.yaml):")
		for _, c := range req.Capabilities {
			fmt.Fprintf(out, "  - %s\n", c.Name)
			fmt.Fprintf(out, "      used by: %s\n", strings.Join(c.UsedBy, ", "))
			if c.ModelHint != nil {
				if c.ModelHint.SuggestedModel != "" {
					fmt.Fprintf(out, "      suggested model: %s\n", c.ModelHint.SuggestedModel)
				}
				if c.ModelHint.MinParams != "" {
					fmt.Fprintf(out, "      min params: %s\n", c.ModelHint.MinParams)
				}
				if c.ModelHint.MinContext > 0 {
					fmt.Fprintf(out, "      min context: %d tokens\n", c.ModelHint.MinContext)
				}
				if len(c.ModelHint.Capabilities) > 0 {
					fmt.Fprintf(out, "      model capabilities: %s\n", strings.Join(c.ModelHint.Capabilities, ", "))
				}
			}
		}
	}

	if len(req.Services) > 0 {
		fmt.Fprintln(out, "\nExternal services (must be reachable before running):")
		for _, s := range req.Services {
			fmt.Fprintf(out, "  - %s", s.Name)
			if s.URL != "" {
				fmt.Fprintf(out, " (%s)", s.URL)
			}
			fmt.Fprintln(out)
			if s.Description != "" {
				fmt.Fprintf(out, "      %s\n", s.Description)
			}
			if s.SetupHint != "" {
				fmt.Fprintf(out, "      setup: %s\n", s.SetupHint)
			}
		}
	}

	if len(req.Inputs) > 0 {
		fmt.Fprintln(out, "\nInputs:")
		for _, in := range req.Inputs {
			tag := "optional"
			if in.Required {
				tag = "REQUIRED"
			}
			fmt.Fprintf(out, "  - %s (%s)\n", in.Name, tag)
			if in.Description != "" {
				fmt.Fprintf(out, "      %s\n", in.Description)
			}
			if in.Default != "" {
				fmt.Fprintf(out, "      default: %s\n", in.Default)
			}
		}
	}

	if req.GPUMemory != "" || req.DiskSpace != "" {
		fmt.Fprintln(out, "\nHardware footprint:")
		if req.GPUMemory != "" {
			fmt.Fprintf(out, "  GPU memory: %s\n", req.GPUMemory)
		}
		if req.DiskSpace != "" {
			fmt.Fprintf(out, "  Disk space: %s\n", req.DiskSpace)
		}
	}

	if len(req.Notes) > 0 {
		fmt.Fprintln(out, "\nNotes:")
		for _, n := range req.Notes {
			fmt.Fprintf(out, "  - %s\n", n)
		}
	}
	return nil
}
