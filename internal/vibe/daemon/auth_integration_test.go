package daemon_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/daemon"
	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
	"github.com/gallowaysoftware/vibe/internal/vibeclient"
)

// TestDaemon_TCP_RequiresBearerToken brings up the daemon on a real TCP
// listener (using the same Run() path production hits) and verifies that:
//
//   - a TCP client with no Authorization header gets 401,
//   - a TCP client with the wrong token gets 401,
//   - a TCP client with the right token gets 200,
//   - a unix-socket client gets 200 with no token at all.
//
// This is the load-bearing test for the design: the auth boundary is
// listener-scoped, not RPC-scoped.
func TestDaemon_TCP_RequiresBearerToken(t *testing.T) {
	setupXDG(t)
	httpPort := pickFreePort(t)
	httpAddr := fmt.Sprintf("127.0.0.1:%d", httpPort)

	cfg := daemon.Config{
		ProxyPort:   pickFreePort(t),
		HTTPAddr:    httpAddr,
		LlamaBinary: fakeBinary,
	}
	d := daemon.New(cfg)
	_, _ = startDaemon(t, d)

	// The daemon should have written a token file at first start.
	tokenBytes, err := os.ReadFile(paths.TokenFile())
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		t.Fatalf("token file empty after daemon start")
	}

	// 1) TCP with no token → 401.
	noTokenClient := vibeclient.NewWithHTTPClient("http://"+httpAddr, http.DefaultClient, "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := noTokenClient.Status(ctx); err == nil {
		t.Errorf("TCP Status with no token: got nil error; want unauthenticated")
	} else if !isUnauthenticatedErr(err) {
		t.Errorf("TCP Status with no token: err = %v; want unauthenticated", err)
	}

	// 2) TCP with the wrong token → 401.
	wrongTokenClient := vibeclient.NewWithHTTPClient("http://"+httpAddr, http.DefaultClient, "not-the-token")
	if _, err := wrongTokenClient.Status(ctx); err == nil {
		t.Errorf("TCP Status with wrong token: got nil error; want unauthenticated")
	} else if !isUnauthenticatedErr(err) {
		t.Errorf("TCP Status with wrong token: err = %v; want unauthenticated", err)
	}

	// 3) TCP with the correct token → 200.
	rightTokenClient := vibeclient.NewWithHTTPClient("http://"+httpAddr, http.DefaultClient, token)
	if _, err := rightTokenClient.Status(ctx); err != nil {
		t.Errorf("TCP Status with correct token: %v", err)
	}

	// 4) Unix socket with no token → 200 (startDaemon already proved this
	// by polling Status). Assert explicitly to make the contract obvious.
	hc := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", paths.Socket())
			},
		},
	}
	unix := vibeclient.NewWithHTTPClient("http://vibe.local", hc, "")
	if _, err := unix.Status(ctx); err != nil {
		t.Errorf("unix Status with no token: %v (unix socket must not require auth)", err)
	}
}

// TestDaemon_TokenFilePermissions confirms the token file the daemon writes
// is 0600. Re-asserts the unit-test invariant against the real Run() path.
func TestDaemon_TokenFilePermissions(t *testing.T) {
	setupXDG(t)
	d := makeDaemon(t)
	_, _ = startDaemon(t, d)

	info, err := os.Stat(paths.TokenFile())
	if err != nil {
		t.Fatalf("stat token: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("token file mode = %o, want 0600", got)
	}
}

// isUnauthenticatedErr detects the Connect error shape the client returns
// when the underlying HTTP response is 401. Connect maps 401 to
// CodeUnauthenticated.
func isUnauthenticatedErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "unauthenticated") || strings.Contains(s, "Unauthenticated") || strings.Contains(s, "401")
}
