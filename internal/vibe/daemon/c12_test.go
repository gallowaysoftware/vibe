package daemon_test

// C12 coverage: the guest read-only bearer against the real TCP auth
// boundary. The point of these tests is that they are DERIVED from
// fleetapi.Routes() — a route added by a later phase is probed against
// the guest boundary without anyone remembering to extend a list here.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/vibe/daemon"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
)

type guestFleetd struct {
	addr  string
	token string
	guest string
}

// startGuestFleetd brings up a fleetd-role daemon with every optional
// store enabled (so every route in the table is mounted) and a guest
// token file the daemon mints for itself.
func startGuestFleetd(t *testing.T) guestFleetd {
	t.Helper()
	cfgHome, _, _ := setupXDG(t)
	cell := fleetCellFake(t, `{"object":"list","data":[{"id":"qwen3.6-27b","object":"model"}]}`)
	hostsYAML := fmt.Sprintf(`
fleetd_url: "http://127.0.0.1:1"
cells:
  front:    { url: "%s", class: always_on }
  gpu-cell: { url: "%s", class: opportunistic }
`, cell.URL, cell.URL)
	if err := os.WriteFile(filepath.Join(cfgHome, "vibe", "hosts.yaml"), []byte(hostsYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	guestPath := filepath.Join(cfgHome, "vibe", "guest-token")
	cfg := daemon.Config{
		ProxyPort:     pickFreePort(t),
		DisableProxy:  true,
		HTTPAddr:      fmt.Sprintf("127.0.0.1:%d", pickFreePort(t)),
		LlamaBinary:   fakeBinary,
		FleetRegistry: true,
		Fleet:         daemon.FleetConfig{GuestTokenFile: guestPath},
	}
	d := daemon.New(cfg)
	_, _ = startDaemon(t, d)

	return guestFleetd{
		addr:  cfg.HTTPAddr,
		token: readTrimmed(t, paths.TokenFile()),
		guest: readTrimmed(t, guestPath),
	}
}

func readTrimmed(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		t.Fatalf("%s is empty", path)
	}
	return s
}

// call issues one request and returns its status plus body. No timeout
// context: /api/fleet/events is a live stream and a deferred cancel
// would race the read of its first frame.
func call(t *testing.T, method, url, tok, body string) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	r, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	// An accepted /api/fleet/events request is a live stream that never
	// ends: its status is the whole answer, and reading the body would
	// block until the daemon shuts down.
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return resp.StatusCode, ""
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, string(b)
}

// TestGuestToken_DeniedOnEveryRouteItDoesNotName is gate 5: walk the
// registry, present the guest bearer at every declared route, and hold
// each to what its Access says. A route added later is covered here the
// day it is added.
func TestGuestToken_DeniedOnEveryRouteItDoesNotName(t *testing.T) {
	f := startGuestFleetd(t)
	probed := 0
	for _, rt := range fleetapi.Routes() {
		url := "http://" + f.addr + rt.Path
		status, body := call(t, rt.Method, url, f.guest, "{}")
		probed++
		switch rt.Access {
		case fleetapi.AccessGuest:
			if status == http.StatusUnauthorized {
				t.Errorf("%s %s with the guest token: 401, want the guest grant to work", rt.Method, rt.Path)
			}
		case fleetapi.AccessPublic:
			if status != http.StatusOK {
				t.Errorf("%s %s: HTTP %d, want 200 (public)", rt.Method, rt.Path, status)
			}
		default:
			if status != http.StatusUnauthorized {
				t.Errorf("%s %s with the guest token: HTTP %d, want 401 — the guest grant is two GETs, "+
					"not every read (body: %.80s)", rt.Method, rt.Path, status, body)
			}
		}
	}
	if probed < 10 {
		t.Fatalf("probed only %d routes; the registry looks empty", probed)
	}

	// The positive control, and gate 10's state half: the guest reads the
	// same document the operator does, and that document does not carry
	// the credential.
	status, body := call(t, http.MethodGet, "http://"+f.addr+"/api/fleet/state", f.guest, "")
	if status != http.StatusOK || !strings.Contains(body, "gpu-cell") {
		t.Fatalf("guest state read: HTTP %d, %.120s", status, body)
	}
	if strings.Contains(body, f.guest) || strings.Contains(body, f.token) {
		t.Error("a token appears in /api/fleet/state; it is a credential and belongs in no document")
	}
}

// TestGuestToken_ReachesNeitherMCPNorTheRPCs pins the two surfaces that
// are NOT in fleetapi's table and hold every mutation in the fleet.
func TestGuestToken_ReachesNeitherMCPNorTheRPCs(t *testing.T) {
	f := startGuestFleetd(t)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`},
		{http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fleet_status","arguments":{}}}`},
		{http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"drain_cell","arguments":{"cell":"gpu-cell"}}}`},
		{http.MethodGet, "/mcp", ""},
		{http.MethodPost, "/vibe.v1.ControlService/Status", "{}"},
		{http.MethodPost, "/vibe.v1.ControlService/CellDrain", "{}"},
		{http.MethodPost, "/vibe.v1.ControlService/Shutdown", "{}"},
		{http.MethodGet, "/", ""},
		{http.MethodGet, "/api/fleet", ""},
	} {
		status, body := call(t, tc.method, "http://"+f.addr+tc.path, f.guest, tc.body)
		if status != http.StatusUnauthorized {
			t.Errorf("%s %s with the guest token: HTTP %d, want 401 (%.60s)", tc.method, tc.path, status, body)
		}
	}
}

// TestGuestToken_AllowlistIsExactMatchAndGETOnly applies C5's bypass
// shapes to the new hole. Every one of these is a path a prefix match, a
// path.Clean or a decoded path would let through.
func TestGuestToken_AllowlistIsExactMatchAndGETOnly(t *testing.T) {
	f := startGuestFleetd(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/fleet/state"},
		{http.MethodPut, "/api/fleet/state"},
		{http.MethodGet, "/api/fleet/state/"},
		{http.MethodGet, "/api/fleet/state/../usage"},
		{http.MethodGet, "/api/fleet/state%2f"},
		{http.MethodGet, "//api/fleet/state"},
		{http.MethodGet, "/api/fleet/state/sub"},
		{http.MethodGet, "/api/fleet/statex"},
		{http.MethodGet, "/api/fleet/events/"},
		{http.MethodGet, "/api/fleet/events/../savings"},
	} {
		status, _ := call(t, tc.method, "http://"+f.addr+tc.path, f.guest, "")
		if status != http.StatusUnauthorized {
			t.Errorf("%s %s with the guest token: HTTP %d, want 401", tc.method, tc.path, status)
		}
	}
	// A query string is not part of the path and must not break the match.
	if status, _ := call(t, http.MethodGet, "http://"+f.addr+"/api/fleet/state?x=1", f.guest, ""); status != http.StatusOK {
		t.Errorf("guest GET /api/fleet/state?x=1: HTTP %d, want 200", status)
	}
}

// TestGuestToken_RejectionsCountSeparately is gate 11. auth_rejected
// answers "someone holds a stale token"; a guest reaching past their
// grant is a different fact and must not degrade that signal.
func TestGuestToken_RejectionsCountSeparately(t *testing.T) {
	f := startGuestFleetd(t)
	counters := func() (auth, guest int64, enabled bool) {
		t.Helper()
		status, body := call(t, http.MethodGet, "http://"+f.addr+"/api/fleet/state", f.token, "")
		if status != http.StatusOK {
			t.Fatalf("state: HTTP %d", status)
		}
		var st struct {
			Daemon struct {
				AuthRejected  int64 `json:"auth_rejected"`
				GuestRejected int64 `json:"guest_rejected"`
				GuestEnabled  bool  `json:"guest_enabled"`
			} `json:"daemon"`
		}
		if err := json.Unmarshal([]byte(body), &st); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return st.Daemon.AuthRejected, st.Daemon.GuestRejected, st.Daemon.GuestEnabled
	}

	auth0, guest0, enabled := counters()
	if !enabled {
		t.Error("guest_enabled is false on a daemon that loaded a guest token")
	}

	if status, _ := call(t, http.MethodGet, "http://"+f.addr+"/api/fleet/state", "not-the-token-at-all", ""); status != http.StatusUnauthorized {
		t.Fatalf("wrong token: HTTP %d, want 401", status)
	}
	auth1, guest1, _ := counters()
	if auth1 != auth0+1 {
		t.Errorf("auth_rejected %d -> %d, want +1 for a wrong token", auth0, auth1)
	}
	if guest1 != guest0 {
		t.Errorf("guest_rejected moved (%d -> %d) on a wrong token", guest0, guest1)
	}

	if status, _ := call(t, http.MethodPost, "http://"+f.addr+"/mcp", f.guest, "{}"); status != http.StatusUnauthorized {
		t.Fatalf("guest on /mcp: HTTP %d, want 401", status)
	}
	auth2, guest2, _ := counters()
	if guest2 != guest1+1 {
		t.Errorf("guest_rejected %d -> %d, want +1 for a guest past its grant", guest1, guest2)
	}
	if auth2 != auth1 {
		t.Errorf("auth_rejected moved (%d -> %d) on a valid guest token; that counter means "+
			"'a client holds a stale token' and must stay readable", auth1, auth2)
	}
}

// TestGuestToken_UnconfiguredIsOffAndMintsNothing is gate 1: the feature
// is off by default and a fleet that never configures one behaves
// exactly as it did before C12.
func TestGuestToken_UnconfiguredIsOffAndMintsNothing(t *testing.T) {
	cfgHome, stateHome, _ := setupXDG(t)
	cell := fleetCellFake(t, `{"object":"list","data":[]}`)
	hostsYAML := fmt.Sprintf("fleetd_url: \"http://127.0.0.1:1\"\ncells:\n  front: { url: \"%s\", class: always_on }\n", cell.URL)
	if err := os.WriteFile(filepath.Join(cfgHome, "vibe", "hosts.yaml"), []byte(hostsYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := daemon.Config{
		ProxyPort:     pickFreePort(t),
		DisableProxy:  true,
		HTTPAddr:      fmt.Sprintf("127.0.0.1:%d", pickFreePort(t)),
		LlamaBinary:   fakeBinary,
		FleetRegistry: true,
	}
	_, _ = startDaemon(t, daemon.New(cfg))

	// Nothing that looks like a guest token opens anything.
	for _, tok := range []string{"", "guest", strings.Repeat("a", 43)} {
		status, _ := call(t, http.MethodGet, "http://"+cfg.HTTPAddr+"/api/fleet/state", tok, "")
		if status != http.StatusUnauthorized {
			t.Errorf("state with %q on an unconfigured daemon: HTTP %d, want 401", tok, status)
		}
	}
	// And no credential file appeared anywhere in the state dir.
	entries, err := os.ReadDir(filepath.Join(stateHome, "vibe"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "guest") {
			t.Errorf("unconfigured daemon minted %s; guest access must cost nothing until it is asked for", e.Name())
		}
	}
	status, body := call(t, http.MethodGet, "http://"+cfg.HTTPAddr+"/api/fleet/state", readTrimmed(t, paths.TokenFile()), "")
	if status != http.StatusOK {
		t.Fatalf("state with the real token: HTTP %d", status)
	}
	if strings.Contains(body, "guest_enabled") || strings.Contains(body, "guest_rejected") {
		t.Errorf("an unconfigured daemon reports guest fields: %.200s", body)
	}
}
