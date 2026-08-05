package fleetapi

// C12 coverage: the route registry is the guest allowlist. These tests
// are the reason the table exists — a hand-maintained list of "routes a
// guest may reach" drifts the first time a phase adds a route and
// forgets, and this plan adds routes most phases.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestRoutes_EveryRouteDeclaresAnAccessLevel is gate 2, the
// registry-completeness check: a route added without deciding what it
// grants keeps Access's zero value, and that fails here. It is the one
// test that must keep working for phases nobody has written yet.
func TestRoutes_EveryRouteDeclaresAnAccessLevel(t *testing.T) {
	seen := map[string]bool{}
	for _, rt := range Routes() {
		key := rt.Method + " " + rt.Path
		if rt.Access == AccessUndecided {
			t.Errorf("%s has no access declaration: every route must say whether the guest read-only "+
				"token reaches it (see docs/design/fleet-control-plan/c12-guest-token.md)", key)
		}
		if seen[key] {
			t.Errorf("%s is declared twice; the allowlist lookup would keep only one", key)
		}
		seen[key] = true
		if got := AccessFor(rt.Method, rt.Path); got != rt.Access {
			t.Errorf("AccessFor(%s) = %v, table says %v", key, got, rt.Access)
		}
	}
	if len(seen) == 0 {
		t.Fatal("route table is empty; this test would pass vacuously")
	}
}

// TestRoutes_GuestSurfaceIsExactlyStateAndEvents is a tripwire, not a
// completeness check: widening the guest grant must be a deliberate edit
// to a test that names the doc, never a side effect of adding a route.
func TestRoutes_GuestSurfaceIsExactlyStateAndEvents(t *testing.T) {
	want := map[string]bool{
		"GET /api/fleet/state":  true,
		"GET /api/fleet/events": true,
	}
	got := map[string]bool{}
	for _, rt := range Routes() {
		if rt.Access == AccessGuest {
			got[rt.Method+" "+rt.Path] = true
		}
	}
	for k := range want {
		if !got[k] {
			t.Errorf("%s is no longer guest-readable; the page's live table needs both", k)
		}
	}
	for k := range got {
		if !want[k] {
			t.Errorf("%s was added to the guest surface. That is a real decision — the guest bearer is "+
				"handed to phones and housemates. Update c12-guest-token.md §1 and this test together.", k)
		}
	}
}

// TestRoutes_PublicSurfaceIsExactlyTheFleetPage guards C5's one bearer
// exemption now that it is declared in the table rather than written as
// a literal in the middleware.
func TestRoutes_PublicSurfaceIsExactlyTheFleetPage(t *testing.T) {
	var public []string
	for _, rt := range Routes() {
		if rt.Access == AccessPublic {
			public = append(public, rt.Method+" "+rt.Path)
		}
	}
	if len(public) != 1 || public[0] != "GET /ui/fleet" {
		t.Fatalf("public surface = %v, want exactly [GET /ui/fleet] — an unauthenticated route is a "+
			"decision C5 made once, for a static asset with no fleet data in it", public)
	}
}

// TestAccessFor_IsExactMatchOnMethodAndPath pins the lookup itself. The
// middleware calls it with the RAW request path, so every shape a mux
// would clean into a match must be a miss here.
func TestAccessFor_IsExactMatchOnMethodAndPath(t *testing.T) {
	if got := AccessFor(http.MethodGet, "/api/fleet/state"); got != AccessGuest {
		t.Fatalf("the positive control failed: AccessFor(GET /api/fleet/state) = %v", got)
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/fleet/state"},   // method-exact
		{"get", "/api/fleet/state"},             // methods are not case-folded
		{http.MethodGet, "/api/fleet/state/"},   // trailing slash
		{http.MethodGet, "/api/fleet/state/x"},  // sub-path
		{http.MethodGet, "/api/fleet/statex"},   // prefix, not path
		{http.MethodGet, "//api/fleet/state"},   // doubled separator
		{http.MethodGet, "/api/fleet/state%2f"}, // percent-encoded separator
		{http.MethodGet, "/api/fleet/events/../usage"},
		{http.MethodGet, "/ui/fleet/"},
		{http.MethodGet, "/mcp"},
		{http.MethodPost, "/mcp"},
		{http.MethodPost, "/vibe.v1.ControlService/CellDrain"},
		{http.MethodGet, "/api/fleet/usage"},
		{http.MethodGet, "/api/fleet/savings"},
		{http.MethodGet, "/api/fleet/leases"},
		{http.MethodGet, "/"},
	} {
		if got := AccessFor(tc.method, tc.path); got != AccessTokenOnly {
			t.Errorf("AccessFor(%s %s) = %v, want token-only", tc.method, tc.path, got)
		}
	}
}

// TestRoutes_NoHandlerIsRegisteredOutsideTheTable keeps the table
// authoritative: a route mounted directly on the mux would carry no
// access declaration and would be invisible to every test above. Same
// shape as C7a's module-wide grep against truncation-based day buckets.
func TestRoutes_NoHandlerIsRegisteredOutsideTheTable(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	// Matches mux.HandleFunc( / mux.Handle( on any receiver name.
	re := regexp.MustCompile(`\.Handle(Func)?\(`)
	found := false
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if !re.MatchString(line) {
				continue
			}
			if name == "routes.go" {
				found = true
				continue
			}
			t.Errorf("%s:%d registers a handler outside routes.go: %s\n"+
				"Every route goes in routeTable so it carries a guest-access decision.", name, i+1, strings.TrimSpace(line))
		}
	}
	if !found {
		t.Error("no registration found in routes.go; this test would pass vacuously")
	}
}

// TestRegister_MountsExactlyTheEnabledRoutes proves the table is what is
// actually served, in both role shapes — otherwise the access
// declarations above would describe a surface nobody mounts.
func TestRegister_MountsExactlyTheEnabledRoutes(t *testing.T) {
	dir := t.TempDir()
	full := New([]Cell{{Name: "front", URL: "http://127.0.0.1:1"}}, dir+"/hist.json", testDaemonInfo, Options{
		IntentPath: filepath.Join(dir, "intent.json"),
		LeasePath:  filepath.Join(dir, "leases.json"),
		UsagePath:  filepath.Join(dir, "usage.jsonl"),
	})
	defer full.Close()
	bare := New([]Cell{{Name: "front", URL: "http://127.0.0.1:1"}}, dir+"/hist2.json", testDaemonInfo, Options{})
	defer bare.Close()

	mounted := func(s *Server) map[string]bool {
		mux := http.NewServeMux()
		s.Register(mux)
		out := map[string]bool{}
		for _, rt := range Routes() {
			req := httptest.NewRequest(rt.Method, rt.Path, nil)
			if _, pattern := mux.Handler(req); pattern != "" {
				out[rt.Method+" "+rt.Path] = true
			}
		}
		return out
	}

	fullMounted := mounted(full)
	for _, rt := range Routes() {
		if !fullMounted[rt.Method+" "+rt.Path] {
			t.Errorf("%s %s declared but not mounted on a fully-featured server", rt.Method, rt.Path)
		}
	}
	bareMounted := mounted(bare)
	want := map[string]bool{"GET /api/fleet/state": true, "GET /api/fleet/events": true}
	for k := range want {
		if !bareMounted[k] {
			t.Errorf("%s missing from a plain (non-fleetd) daemon", k)
		}
	}
	for k := range bareMounted {
		if !want[k] {
			t.Errorf("%s mounted on a plain daemon; fleetd routes must stay fleetd-only", k)
		}
	}
}

// TestFleetPage_GuestModeIsWiredToTheHeaderNotAProbe: a static
// assertion, like the page's other gates — the browser run is live gate
// L1. What it pins is the three decisions §5 of the phase doc argued:
// the page learns its privilege from a response header (no probe, no new
// route, no per-viewer field in the state document), it stops OFFERING
// what a guest cannot do, and the savings view says so instead of
// popping the token gate.
func TestFleetPage_GuestModeIsWiredToTheHeaderNotAProbe(t *testing.T) {
	data, err := fleetPageFS.ReadFile("fleet.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(data)
	for _, want := range []string{
		// Learned from a response header, not probed — and applied in BOTH
		// directions, so pasting the control-plane token into a tab that
		// browsed as a guest gives the buttons back without a reload.
		`if (r.status !== 401) setAuthMode(r.headers.get("X-Vibe-Auth") === "guest");`,
		`document.body.classList.toggle("guest", isGuest);`,
		`if (!guest) {`,                       // the action buttons
		`if (!n.configured || guest) return;`, // the notify controls
		`if (guest) { showSavingsDenied(); return; }`,
		`body.guest #warmrow`,
		`body.guest #nav-savings`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page is missing its guest-mode wiring: %s", want)
		}
	}
	// A guest must never be told their working token is invalid, so the
	// savings fetch is the one call that does not pop the token gate.
	if !regexp.MustCompile(`api\("/api/fleet/savings(.|\n)*?, null, true\)`).MatchString(page) {
		t.Error("the savings fetch must pass quiet=true: a 401 there is C12's answer, not a bad token")
	}
	// The chip is class-driven. `.chip { display: inline-block }` is an
	// author rule and beats the UA sheet's [hidden], so a hidden attribute
	// on a .chip element does nothing and the read-only badge would render
	// for the operator too.
	if !strings.Contains(page, "#guest-chip { display: none;") ||
		!strings.Contains(page, "body.guest #guest-chip { display: inline-block; }") {
		t.Error("the read-only chip must be shown/hidden by the guest class, not by a hidden attribute")
	}
	if regexp.MustCompile(`id="guest-chip"[^>]*hidden`).MatchString(page) {
		t.Error(`the chip carries a hidden attribute that .chip's display rule overrides`)
	}
	// And the page must not have grown a route to ask about privilege.
	if strings.Contains(page, "/api/fleet/whoami") || strings.Contains(page, "/api/fleet/auth") {
		t.Error("the page added a route to discover its own privilege; C12 §5 forbids it")
	}
}

// TestFleetPage_A401FromMCPDoesNotPopTheTokenGate is the review's page
// finding. §5's chosen degradation is "if a proxy strips the header the
// page keeps its buttons and a click 401s" — but the 401 handler pops
// the TOKEN GATE, which is the exact behaviour §5 says this phase must
// not ship: a guest with a perfectly good token told it is invalid. A
// /mcp 401 is ambiguous (stale control-plane token vs. guest past their
// grant) and does not get to decide; /api/fleet/state can tell the two
// apart, flash() re-runs refresh() 1.5s after the click, and a genuinely
// dead token pops the gate there.
func TestFleetPage_A401FromMCPDoesNotPopTheTokenGate(t *testing.T) {
	data, err := fleetPageFS.ReadFile("fleet.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(data)
	// The /mcp call is quiet: `}, true).then(...)` closes rpc()'s api()
	// call. Matched as a block so an argument added later cannot silently
	// shift `quiet` back to undefined.
	if !regexp.MustCompile(`api\("/mcp",(.|\n)*?\n  \}, true\)`).MatchString(page) {
		t.Error("rpc() must pass quiet=true: a 401 from /mcp is ambiguous, and popping the token gate " +
			"tells a guest whose X-Vibe-Auth header was stripped that their working token is invalid")
	}
	// The button still fails LOUDLY, and the state fetch still gets to
	// pop the gate for a token that really is dead.
	if !strings.Contains(page, `flash(btn, old, "failed", true);`) {
		t.Error("a refused action must still report failure on the button")
	}
	if !strings.Contains(page, `refresh().catch(() => {}); }, 1500)`) {
		t.Error("flash() must re-run refresh(): /api/fleet/state is the route that can tell a stale " +
			"control-plane token from a guest past their grant, and it pops the gate")
	}
	// Re-authenticating on #savings must not leave the un-hidden body
	// empty until the 60s timer happens to fire.
	if !strings.Contains(page, `if (location.hash === "#savings") loadSavings(true);`) {
		t.Error("setAuthMode(false) must reload the savings view when it is the one on screen")
	}
}
