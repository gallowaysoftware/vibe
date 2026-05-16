package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vamp"
)

func capabilitiesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "capabilities",
		Short: "Show the capability → vibe profile mapping.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			caps, err := vamp.LoadCapabilities()
			if err != nil {
				return err
			}
			if len(caps.Mapping) == 0 {
				fmt.Println("no capabilities defined")
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
				// "<cap>  <profile>" output; longer lists are joined
				// with a comma so the candidate order is visible.
				ps, err := caps.Profiles(k)
				if err != nil {
					return err
				}
				fmt.Printf("%-25s  %s\n", k, strings.Join(ps, ", "))
			}
			return nil
		},
	}
}
