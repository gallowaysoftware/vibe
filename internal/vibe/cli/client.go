package cli

import (
	"context"
	"net"
	"net/http"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
	"github.com/gallowaysoftware/vibe/internal/vibeclient"
)

// newClient returns a vibeclient bound to vibe's unix-socket control plane.
// The hostname in the base URL is ignored by the custom transport but Connect
// requires a parseable URL.
func newClient() *vibeclient.Client {
	hc := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", paths.Socket())
			},
		},
	}
	// Empty token: the daemon doesn't enforce auth on the unix socket
	// (filesystem perms are the auth boundary there). Avoids forcing the
	// local CLI through token discovery for every command.
	return vibeclient.NewWithHTTPClient("http://vibe.local", hc, "")
}

// pingDaemon returns nil if the daemon answers Status within the given timeout.
func pingDaemon(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, err := newClient().Status(ctx)
	return err
}
