package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vamp"
	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
)

func capabilitiesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "capabilities",
		Short: "Show the capability → vibe backend mapping (profile-name fallback).",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			caps, err := vamp.LoadCapabilities()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(caps.Mapping) == 0 {
				fmt.Fprintln(out, "no capabilities defined")
				return nil
			}
			keys := make([]string, 0, len(caps.Mapping))
			for k := range caps.Mapping {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				// Resolve through Profiles so both the string-shorthand
				// and long-form candidates list render the same way. A
				// single-element list looks like the historical
				// "<cap>  <name>" output; longer lists are joined
				// with a comma so the candidate order is visible.
				ps, err := caps.Profiles(k)
				if err != nil {
					return err
				}
				annotated := make([]string, len(ps))
				for i, name := range ps {
					annotated[i] = name + resolutionNote(name)
				}
				fmt.Fprintf(out, "%-25s  %s\n", k, strings.Join(annotated, ", "))
			}
			return nil
		},
	}
}

// resolutionNote mirrors the executor's activation order (exec.go): a
// candidate names a backend first, falls back to profile activation when
// no backend by that name exists, and fails when neither does. Annotating
// the listing means the operator sees the same resolution a run will use
// without starting anything. Backend hits stay bare — that's the intended
// mapping, only the fallback and the miss are worth calling out.
func resolutionNote(name string) string {
	if _, err := os.Stat(filepath.Join(paths.BackendsDir(), name+".yaml")); err == nil {
		return ""
	}
	if _, err := os.Stat(filepath.Join(paths.ProfilesDir(), name+".yaml")); err == nil {
		return " (profile fallback)"
	}
	return " (not found)"
}
