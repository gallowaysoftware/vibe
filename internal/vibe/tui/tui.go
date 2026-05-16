package tui

import (
	"context"
	"fmt"
	"net"
	"net/http"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
	"github.com/gallowaysoftware/vibe/internal/vibeclient"
	vibev1 "github.com/gallowaysoftware/vibe/proto/vibe/v1"
)

// Run boots the TUI. apiURL follows the same precedence as the rest of the
// CLI: argument → $VIBE_API → DefaultBaseURL. When apiURL is empty we also
// fall back to vibe's unix-socket transport (used when speaking to a locally-
// running daemon).
//
// Run is a blocking call; it returns when the user quits or when Bubbletea
// errors out. The alt-screen is torn down before return so the terminal is
// left in a clean state.
//
// We intentionally do NOT auto-spawn the daemon here. If it isn't reachable,
// the model renders a "daemon not running" panel — that's friendlier than
// either refusing to start the TUI or quietly forking a long-lived process
// out from under the operator.
func Run(apiURL string) error {
	api, displayURL := newAPI(apiURL)
	m := NewModel(api, displayURL, false)
	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("tui: %w", err)
	}
	return nil
}

// vibeClientAdapter narrows *vibeclient.Client to the vibeAPI interface the
// model expects (Start returns void instead of *StartResult).
type vibeClientAdapter struct{ c *vibeclient.Client }

func (a vibeClientAdapter) Status(ctx context.Context) (*vibev1.Status, error) {
	return a.c.Status(ctx)
}
func (a vibeClientAdapter) ListProfiles(ctx context.Context) ([]*vibev1.Profile, error) {
	return a.c.ListProfiles(ctx)
}
func (a vibeClientAdapter) Logs(ctx context.Context) ([]string, error) {
	return a.c.Logs(ctx)
}
func (a vibeClientAdapter) Start(ctx context.Context, profile string) error {
	_, err := a.c.Start(ctx, profile)
	return err
}
func (a vibeClientAdapter) Stop(ctx context.Context) error {
	_, err := a.c.Stop(ctx)
	return err
}

// newAPI mirrors vibeclient.New's precedence (argument → $VIBE_API →
// DefaultBaseURL) but additionally prefers vibe's unix socket when the
// caller didn't pass an explicit URL. The display string is what the title
// bar shows.
func newAPI(apiURL string) (vibeAPI, string) {
	if apiURL == "" {
		// No explicit URL: dial the unix socket if it's there.
		sock := paths.Socket()
		hc := &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sock)
				},
			},
		}
		// Unix socket: no token required (filesystem perms gate access).
		c := vibeclient.NewWithHTTPClient("http://vibe.local", hc, "")
		return vibeClientAdapter{c: c}, "unix://" + sock
	}
	c := vibeclient.New(apiURL)
	return vibeClientAdapter{c: c}, apiURL
}
