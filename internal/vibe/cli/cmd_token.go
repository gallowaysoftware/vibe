package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vibe/daemon"
	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
)

func tokenCmd() *cobra.Command {
	var regenerate, yes bool

	cmd := &cobra.Command{
		Use:   "token",
		Short: "Print or regenerate the daemon's bearer token.",
		Long: `Print the bearer token the daemon expects on TCP control-plane requests.

The token is stored at $XDG_STATE_HOME/vibe/token (mode 0600). Copy it to
a remote laptop and export it as $VIBE_TOKEN, then talk to the daemon
with $VIBE_API=http://devbox.local:9001.

The unix socket does not require a token; this command targets the
TCP path only.

  vibe token              # print the current token
  vibe token --regenerate # write a new token (confirms unless --yes)`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if regenerate {
				return regenerateToken(cmd.OutOrStdout(), cmd.InOrStdin(), yes)
			}
			return printToken(cmd.OutOrStdout())
		},
	}
	cmd.Flags().BoolVar(&regenerate, "regenerate", false, "Replace the current token with a fresh random value.")
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip the confirmation prompt for --regenerate.")
	return cmd
}

func printToken(w io.Writer) error {
	path := paths.TokenFile()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("token file %s does not exist (start the daemon once to create it)", path)
	}
	if err != nil {
		return fmt.Errorf("read token: %w", err)
	}
	tok := strings.TrimSpace(string(data))
	if tok == "" {
		return fmt.Errorf("token file %s is empty", path)
	}
	fmt.Fprintln(w, tok)
	return nil
}

func regenerateToken(w io.Writer, r io.Reader, yes bool) error {
	if !yes {
		fmt.Fprintf(w, "This will invalidate the current token. Any laptop or external client "+
			"that has it will stop working until you copy the new one over.\n"+
			"Proceed? [y/N] ")
		// Use a bufio Reader so we can read a single line even if more is
		// buffered (e.g. piped input).
		br := bufio.NewReader(r)
		line, _ := br.ReadString('\n')
		line = strings.ToLower(strings.TrimSpace(line))
		if line != "y" && line != "yes" {
			fmt.Fprintln(w, "aborted")
			return nil
		}
	}
	tok, err := daemon.RegenerateToken()
	if err != nil {
		return err
	}
	fmt.Fprintln(w, tok)
	fmt.Fprintln(w, "# Restart the daemon (`vibe shutdown` then your next command auto-spawns it) for the new token to take effect.")
	return nil
}
