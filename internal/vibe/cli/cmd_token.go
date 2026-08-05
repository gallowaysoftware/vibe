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
	var regenerate, yes, guest bool

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
  vibe token --regenerate # write a new token (confirms unless --yes)

--guest targets the guest READ-ONLY token instead (fleet-control C12).
It is honored on GET /api/fleet/state and GET /api/fleet/events and
refused on everything else, so it can be handed to a phone or a
housemate without handing over drain. It lives wherever
fleet.guest_token_file points; the daemon mints one there at first start.

  vibe token --guest              # print it, to share it
  vibe token --guest --regenerate # revoke every guest and mint a new one`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if guest {
				path, err := guestTokenPath()
				if err != nil {
					return err
				}
				if regenerate {
					return regenerateGuestToken(cmd.OutOrStdout(), cmd.InOrStdin(), path, yes)
				}
				return printTokenFile(cmd.OutOrStdout(), path)
			}
			if regenerate {
				return regenerateToken(cmd.OutOrStdout(), cmd.InOrStdin(), yes)
			}
			return printTokenFile(cmd.OutOrStdout(), paths.TokenFile())
		},
	}
	cmd.Flags().BoolVar(&regenerate, "regenerate", false, "Replace the current token with a fresh random value.")
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip the confirmation prompt for --regenerate.")
	cmd.Flags().BoolVar(&guest, "guest", false, "Target the guest read-only token (fleet.guest_token_file) instead of the control-plane token.")
	return cmd
}

// guestTokenPath resolves fleet.guest_token_file. Guest access is off by
// default, so an unset key is a configuration answer rather than an
// error about a missing file.
func guestTokenPath() (string, error) {
	cfg, err := daemon.LoadConfig()
	if err != nil {
		return "", fmt.Errorf("read config: %w", err)
	}
	if cfg.Fleet.GuestTokenFile == "" {
		return "", fmt.Errorf("no guest read-only token is configured: set fleet.guest_token_file in %s and restart the daemon (it mints one at that path)", paths.ConfigFile())
	}
	// Refused, not obeyed. Printing this file would hand out the
	// control-plane token under a banner that says "share this", and
	// --regenerate would rotate the control-plane token from a command
	// whose name says guest — every client 401s and nothing says why.
	// The daemon refuses the same configuration (guest access stays off);
	// this is the same refusal on the operator's side of it.
	if daemon.IsControlTokenFile(cfg.Fleet.GuestTokenFile) {
		return "", fmt.Errorf("fleet.guest_token_file in %s points at the control-plane token file (%s): "+
			"guest access is disabled in that configuration, and rotating it here would revoke every "+
			"control-plane client. Give the guest credential its own path",
			paths.ConfigFile(), paths.TokenFile())
	}
	return cfg.Fleet.GuestTokenFile, nil
}

func printTokenFile(w io.Writer, path string) error {
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
	if !confirm(w, r, yes, "This will invalidate the current token. Any laptop or external client "+
		"that has it will stop working until you copy the new one over.\n") {
		fmt.Fprintln(w, "aborted")
		return nil
	}
	tok, err := daemon.RegenerateToken()
	if err != nil {
		return err
	}
	fmt.Fprintln(w, tok)
	fmt.Fprintln(w, "# Restart the daemon (`vibe shutdown` then your next command auto-spawns it) for the new token to take effect.")
	return nil
}

func regenerateGuestToken(w io.Writer, r io.Reader, path string, yes bool) error {
	if !confirm(w, r, yes, "This revokes the guest token for EVERY guest holding it — there are no "+
		"per-guest credentials to revoke individually.\n") {
		fmt.Fprintln(w, "aborted")
		return nil
	}
	tok, err := daemon.RegenerateGuestToken(path)
	if err != nil {
		return err
	}
	fmt.Fprintln(w, tok)
	fmt.Fprintln(w, "# Restart the daemon for the new guest token to take effect.")
	return nil
}

func confirm(w io.Writer, r io.Reader, yes bool, warning string) bool {
	if yes {
		return true
	}
	fmt.Fprint(w, warning+"Proceed? [y/N] ")
	// Use a bufio Reader so we can read a single line even if more is
	// buffered (e.g. piped input).
	br := bufio.NewReader(r)
	line, _ := br.ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}
