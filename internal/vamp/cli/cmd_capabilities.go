package cli

import (
	"fmt"
	"sort"

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
				fmt.Printf("%-25s  %s\n", k, caps.Mapping[k])
			}
			return nil
		},
	}
}
