package daemon_test

// C12 adversarial-review coverage. Four things the feature commit
// shipped with no test at all:
//
//   - the X-Vibe-Auth response header, which is the ENTIRE mechanism
//     behind the read-only page (phase doc §5). Deleting the Set() left
//     the whole suite green, and so did stamping it on the operator's
//     responses too — two mutations, opposite user-visible bugs, neither
//     observable.
//   - the fail-closed ladder over HTTP. Gate 9 claims the identical-token
//     case "additionally proves the value still works as the
//     control-plane token"; its named test only calls the loader.
//   - guest_token_file pointing AT the control-plane token file. The
//     value comparison cannot be the only guard: the branch above it
//     MINTS into a missing file, and minting there overwrites the
//     control-plane token.
//   - gate 1's "401 to every route", which probed exactly one route.

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/vibe/daemon"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
)

// seedGuest describes the guest token file a daemon should start with:
// the path to configure, and the contents to pre-write ("" leaves the
// file absent so the daemon mints one).
type seedGuest struct {
	path     string
	contents string
}

// seededGuestFleetd is startGuestFleetd's sibling: the caller decides
// what is in the guest file (including "a copy of the control-plane
// token" and "the control-plane token file itself"), and gets the
// control-plane token back whether or not guest access ended up on.
func seededGuestFleetd(t *testing.T, seed func(controlPath, controlToken string) seedGuest) (addr, controlToken string) {
	t.Helper()
	cfgHome, _, _ := setupXDG(t)
	cell := fleetCellFake(t, `{"object":"list","data":[{"id":"qwen3.6-27b","object":"model"}]}`)
	hostsYAML := fmt.Sprintf("fleetd_url: \"http://127.0.0.1:1\"\ncells:\n  front: { url: \"%s\", class: always_on }\n", cell.URL)
	if err := os.WriteFile(filepath.Join(cfgHome, "vibe", "hosts.yaml"), []byte(hostsYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	// The control-plane token has to exist before the guest file can hold
	// a byte-identical copy of it; the daemon would otherwise mint it
	// during Run, long after this seeding ran.
	controlToken, _, err := daemon.LoadOrCreateToken()
	if err != nil {
		t.Fatal(err)
	}
	g := seed(paths.TokenFile(), controlToken)
	if g.contents != "" {
		if err := os.WriteFile(g.path, []byte(g.contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := daemon.Config{
		ProxyPort:     pickFreePort(t),
		DisableProxy:  true,
		HTTPAddr:      fmt.Sprintf("127.0.0.1:%d", pickFreePort(t)),
		LlamaBinary:   fakeBinary,
		FleetRegistry: true,
		Fleet:         daemon.FleetConfig{GuestTokenFile: g.path},
	}
	_, _ = startDaemon(t, daemon.New(cfg))
	return cfg.HTTPAddr, controlToken
}

// authHeader issues one request and returns its status plus the
// X-Vibe-Auth response header.
func authHeader(t *testing.T, method, url, tok string) (int, string) {
	t.Helper()
	r, err := http.NewRequest(method, url, nil)
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
	return resp.StatusCode, resp.Header.Get("X-Vibe-Auth")
}

// guestEnabled reads daemon.guest_enabled off the state document with
// the control-plane token.
func guestEnabled(t *testing.T, base, controlToken string) (bool, string) {
	t.Helper()
	status, body := call(t, http.MethodGet, base+"/api/fleet/state", controlToken, "")
	if status != http.StatusOK {
		t.Fatalf("state with the control-plane token: HTTP %d (%.120s)", status, body)
	}
	return strings.Contains(body, `"guest_enabled":true`), body
}

// TestGuestToken_AuthModeHeaderRidesTheGuestGrantOnly pins the header
// the page reads to decide whether to offer buttons at all. It must be
// present on exactly the responses the GUEST bearer authorized: absent
// for the operator (or every operator page renders read-only) and absent
// on a 401 (or a denial teaches a forger the vocabulary).
func TestGuestToken_AuthModeHeaderRidesTheGuestGrantOnly(t *testing.T) {
	f := startGuestFleetd(t)
	base := "http://" + f.addr

	for _, path := range []string{"/api/fleet/state", "/api/fleet/events"} {
		status, hdr := authHeader(t, http.MethodGet, base+path, f.guest)
		if status == http.StatusUnauthorized {
			t.Fatalf("guest GET %s: 401", path)
		}
		if hdr != "guest" {
			t.Errorf("guest GET %s: X-Vibe-Auth = %q, want %q — the page has no other way to learn it is "+
				"read-only, and without it a guest gets buttons that 401 and a token gate that lies "+
				"about their token", path, hdr, "guest")
		}
		status, hdr = authHeader(t, http.MethodGet, base+path, f.token)
		if status != http.StatusOK {
			t.Fatalf("operator GET %s: HTTP %d", path, status)
		}
		if hdr != "" {
			t.Errorf("operator GET %s: X-Vibe-Auth = %q, want it absent — the header rides only what the "+
				"GUEST bearer authorized, and stamping it here puts every operator page into read-only mode",
				path, hdr)
		}
	}

	// A refusal carries no auth-mode header: the page never sees one, and
	// neither does anything probing the boundary.
	if status, hdr := authHeader(t, http.MethodPost, base+"/mcp", f.guest); status != http.StatusUnauthorized || hdr != "" {
		t.Errorf("guest POST /mcp: HTTP %d, X-Vibe-Auth = %q; want 401 with no header", status, hdr)
	}
	if status, hdr := authHeader(t, http.MethodGet, base+"/api/fleet/savings", f.guest); status != http.StatusUnauthorized || hdr != "" {
		t.Errorf("guest GET /api/fleet/savings: HTTP %d, X-Vibe-Auth = %q; want 401 with no header", status, hdr)
	}
	// The public page is not a guest grant either — it is served before
	// any credential is looked at.
	if status, hdr := authHeader(t, http.MethodGet, base+"/ui/fleet", ""); status != http.StatusOK || hdr != "" {
		t.Errorf("anonymous GET /ui/fleet: HTTP %d, X-Vibe-Auth = %q; want 200 with no header", status, hdr)
	}
}

// TestGuestToken_IdenticalToControlPlaneFailsClosedOverHTTP is gate 9's
// missing half. The unit ladder proves the loader returns an error; what
// an operator needs is what the RUNNING daemon does with that
// configuration — guest access off, and the credential it was copied
// from completely unaffected.
func TestGuestToken_IdenticalToControlPlaneFailsClosedOverHTTP(t *testing.T) {
	addr, controlToken := seededGuestFleetd(t, func(controlPath, tok string) seedGuest {
		return seedGuest{path: filepath.Join(filepath.Dir(controlPath), "guest-copy"), contents: tok}
	})
	base := "http://" + addr

	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/fleet/state", ""},
		{http.MethodGet, "/api/fleet/leases", ""},
		{http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`},
	} {
		if status, _ := call(t, tc.method, base+tc.path, controlToken, tc.body); status == http.StatusUnauthorized {
			t.Errorf("%s %s with the control-plane token: 401 — refusing the guest copy must not touch "+
				"the credential it was copied from", tc.method, tc.path)
		}
	}
	if on, body := guestEnabled(t, base, controlToken); on {
		t.Errorf("guest_enabled is true for a guest file holding a copy of the control-plane token: %.200s", body)
	}
}

// TestGuestToken_RefusedFileOpensNothing: every rung of the ladder is a
// refusal to enable guest access, and a refusal has to be observable
// where it matters — at the door, with the value the operator put in the
// file. The unit tests assert the loader's return value.
func TestGuestToken_RefusedFileOpensNothing(t *testing.T) {
	const tooShort = "guest123"
	addr, controlToken := seededGuestFleetd(t, func(controlPath, _ string) seedGuest {
		return seedGuest{path: filepath.Join(filepath.Dir(controlPath), "guest-token"), contents: tooShort}
	})
	base := "http://" + addr
	if status, _ := call(t, http.MethodGet, base+"/api/fleet/state", tooShort, ""); status != http.StatusUnauthorized {
		t.Errorf("a REFUSED guest token opened /api/fleet/state: HTTP %d — the ladder must fail closed at "+
			"the door, not only in the loader", status)
	}
	on, body := guestEnabled(t, base, controlToken)
	if on {
		t.Errorf("guest_enabled is true after a refused token file: %.200s", body)
	}
	// The daemon kept serving: fleetd is read-and-request-only, and a
	// mis-mounted share token must never cost the fleet its registry.
	if !strings.Contains(body, `"front"`) {
		t.Errorf("the fleet registry did not render after a refused guest token: %.200s", body)
	}
}

// TestGuestToken_PointedAtTheControlPlaneFileNeverRewritesIt is the
// review's blocker case. `guest_token_file: <the control-plane token
// file>` is a configuration the phase doc already lists as reachable.
// The value comparison catches it only because the file HAPPENS to exist
// by then — the branch above it mints into a missing file, so the guard
// has to be on the path. Whatever else happens, the control-plane token
// must come out of daemon start byte-identical.
func TestGuestToken_PointedAtTheControlPlaneFileNeverRewritesIt(t *testing.T) {
	addr, controlToken := seededGuestFleetd(t, func(controlPath, _ string) seedGuest {
		return seedGuest{path: controlPath}
	})
	base := "http://" + addr

	if got := readTrimmed(t, paths.TokenFile()); got != controlToken {
		t.Fatalf("the control-plane token file was rewritten by the guest loader (%q -> %q): every "+
			"control-plane client is now locked out", controlToken, got)
	}
	if status, _ := call(t, http.MethodGet, base+"/api/fleet/state", controlToken, ""); status != http.StatusOK {
		t.Fatalf("the control-plane token stopped working: HTTP %d", status)
	}
	if on, body := guestEnabled(t, base, controlToken); on {
		t.Errorf("guest_enabled is true with guest_token_file pointed at the control-plane token file: %.200s", body)
	}
}

// TestGuestToken_RoutesAreProbedAgainstAnUnconfiguredDaemonToo widens
// gate 1: its named test claims "401 to every route for every token that
// is not the control-plane one" and probed exactly one route. Derived
// from the registry, like gate 5, so a future route is covered here the
// day it is added.
func TestGuestToken_RoutesAreProbedAgainstAnUnconfiguredDaemonToo(t *testing.T) {
	cfgHome, _, _ := setupXDG(t)
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

	// A value that WOULD be a valid guest token had the feature been on.
	const wouldBeGuest = "guest-tok-0123456789abcdef"
	probed := 0
	for _, rt := range fleetapi.Routes() {
		status, _ := call(t, rt.Method, "http://"+cfg.HTTPAddr+rt.Path, wouldBeGuest, "{}")
		probed++
		if rt.Access == fleetapi.AccessPublic {
			if status != http.StatusOK {
				t.Errorf("%s %s: HTTP %d, want 200 (public)", rt.Method, rt.Path, status)
			}
			continue
		}
		if status != http.StatusUnauthorized {
			t.Errorf("%s %s on a daemon with NO guest token configured: HTTP %d, want 401 — off by "+
				"default means off on every route", rt.Method, rt.Path, status)
		}
	}
	if probed < 10 {
		t.Fatalf("probed only %d routes; the registry looks empty", probed)
	}
}
