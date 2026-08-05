package daemon_test

// The allowlist's subject: which spelling of the request path C12's
// positive (method, path) lookup is keyed on.
//
// AGENTS.md and daemon/auth.go both said "the RAW path before the mux
// cleans anything"; the code passed r.URL.Path, which net/url has
// already percent-DECODED — url.ParseRequestURI("/ui/%66leet") yields
// URL.Path == "/ui/fleet" with RawPath == "/ui/%66leet". Nothing was
// reachable that was not already reachable, because Go's ServeMux routes
// on the decoded path too, so middleware and router agreed and the
// allowlist is positive and exact (a decoded match only ever granted a
// route that was already granted). What was broken was the INVARIANT,
// and it becomes a hole the moment anything routes on RawPath or the mux
// changes.
//
// These tests pin the encoded spellings in both directions so the
// subject cannot drift back silently.

import (
	"net/http"
	"net/url"
	"testing"
)

// TestAuth_PercentEncodedSpellingsAreNotTheDeclaredRoute. The public
// page and the two guest routes are the entire non-token surface; each
// is declared as one exact string, and %66 is not f.
func TestAuth_PercentEncodedSpellingsAreNotTheDeclaredRoute(t *testing.T) {
	// The premise, asserted rather than assumed: net/url really does hand
	// the middleware a decoded Path, so matching on .Path is matching on
	// something the client did not send.
	u, err := url.ParseRequestURI("/ui/%66leet")
	if err != nil {
		t.Fatal(err)
	}
	if u.Path != "/ui/fleet" || u.RawPath != "/ui/%66leet" {
		t.Fatalf("net/url no longer decodes ahead of the middleware: Path=%q RawPath=%q — re-derive this "+
			"whole test before touching the fix", u.Path, u.RawPath)
	}

	f := startGuestFleetd(t)
	base := "http://" + f.addr

	// The plain spellings still work, or the "fix" is just a 401 machine.
	if status, hdr := authHeader(t, http.MethodGet, base+"/ui/fleet", ""); status != http.StatusOK || hdr != "" {
		t.Fatalf("anonymous GET /ui/fleet: HTTP %d, X-Vibe-Auth %q; want 200 with no header", status, hdr)
	}
	if status, _ := authHeader(t, http.MethodGet, base+"/api/fleet/state", f.guest); status != http.StatusOK {
		t.Fatalf("guest GET /api/fleet/state: HTTP %d, want 200", status)
	}

	// AccessPublic is exactly one string. An encoded spelling of it is a
	// miss, and a miss is token-only.
	for _, path := range []string{"/ui/%66leet", "/ui/flee%74", "/%75i/fleet"} {
		if status, _ := authHeader(t, http.MethodGet, base+path, ""); status != http.StatusUnauthorized {
			t.Errorf("anonymous GET %s: HTTP %d, want 401 — the public exemption is one exact string, and "+
				"an encoded spelling is a different one", path, status)
		}
	}

	// AccessGuest likewise: a valid read-only bearer on an encoded
	// spelling of a route it holds is refused, because the lookup missed.
	for _, path := range []string{"/api/fleet/%73tate", "/api/fleet/%65vents"} {
		if status, hdr := authHeader(t, http.MethodGet, base+path, f.guest); status != http.StatusUnauthorized || hdr != "" {
			t.Errorf("guest GET %s: HTTP %d, X-Vibe-Auth %q; want 401 with no header", path, status, hdr)
		}
	}

	// And the control-plane token is unaffected by any of it: the
	// middleware's job is deciding what a request may reach WITHOUT that
	// token, and an operator holding it never consults the table.
	for _, path := range []string{"/ui/%66leet", "/api/fleet/%73tate"} {
		if status, _ := authHeader(t, http.MethodGet, base+path, f.token); status == http.StatusUnauthorized {
			t.Errorf("operator GET %s: 401 — the allowlist decides the credential-free surface, not the "+
				"operator's", path)
		}
	}
}
