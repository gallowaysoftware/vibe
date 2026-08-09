package cli

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"connectrpc.com/connect"

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

// daemonAbsent reports whether a pingDaemon failure is evidence that there
// is NO daemon, as opposed to one that did not answer inside the budget.
//
// The distinction is the whole point. "Nothing is running" is a claim, and
// the only ping failure that supports it is one where the socket refused
// the connection or was not there — measured, both arrive as
// connect.CodeUnavailable ("connect: no such file or directory",
// "connect: connection refused"). A ping that ran out of time arrives as
// CodeDeadlineExceeded and is evidence of nothing: the daemon may be busy,
// the box loaded, the handler wedged. Reporting that as "not running" is
// this repo's most-repeated defect — absent evidence rendered as a
// definite value — and it is worst on the surface a script reads.
//
// Fail-closed on purpose: only Unavailable-and-not-a-deadline is treated
// as absence, so a code this build does not recognise, or a future
// transport that maps a dial timeout to Unavailable while still wrapping
// context.DeadlineExceeded, lands on "we do not know" rather than on a
// claim.
func daemonAbsent(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	return connect.CodeOf(err) == connect.CodeUnavailable
}
