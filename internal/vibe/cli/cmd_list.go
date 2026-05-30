package cli

import (
	"github.com/spf13/cobra"
)

// listCmd is a top-level convenience alias for `vibe profile list`. It reads
// the profile YAMLs straight off disk via renderProfileList, so it does not
// touch (or spawn) the daemon — asking "which profiles exist" should never
// start a background process.
func listCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List available profiles (alias of `vibe profile list`).",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return renderProfileList(cmd.OutOrStdout())
		},
	}
}
