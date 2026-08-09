package search

// The verifier's proof, ported. A token-less vibe-search on loopback was
// observed answering `POST /fetch` and MCP `fetch_url` with the body of an
// internal service, and following a 302 into one without re-checking the
// target. Each of those is a named test below.
//
// Nothing here touches the real network. The guard carries two seams
// (lookupIP, dialAddr); the tests own both, so a "public" host is a name
// this file says resolves to a documentation address, and the socket that
// name would open lands on an httptest server bound to 127.0.0.1:0.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// publicIP is TEST-NET-3 (RFC 5737, reserved for documentation). It is not
// loopback, private, link-local or CGNAT, so the guard classifies it as an
// ordinary internet address — which is exactly what a test needs to prove
// the guard still lets a real fetch through.
const publicIP = "203.0.113.10"

// fakeNet gives a directFetcher a resolver and a socket layer the test owns.
type fakeNet struct {
	// dns is what the resolver answers, by hostname.
	dns map[string][]net.IP
	// routes maps a VALIDATED dial address — the one the guard decided to
	// connect to — onto a real listener. An address with no entry is a
	// connection refused, which is what makes "did the guard dial this?"
	// answerable rather than merely plausible.
	routes map[string]string

	mu     sync.Mutex
	dialed []string
}

func (f *fakeNet) install(d *directFetcher) {
	d.guard.lookupIP = func(_ context.Context, host string) ([]net.IP, error) {
		ips, ok := f.dns[host]
		if !ok {
			return nil, fmt.Errorf("fakeNet: no such host %q", host)
		}
		return ips, nil
	}
	d.guard.dialAddr = func(ctx context.Context, network, addr string) (net.Conn, error) {
		f.mu.Lock()
		f.dialed = append(f.dialed, addr)
		f.mu.Unlock()
		target, ok := f.routes[addr]
		if !ok {
			return nil, fmt.Errorf("fakeNet: nothing listening at %s", addr)
		}
		return (&net.Dialer{}).DialContext(ctx, network, target)
	}
}

func (f *fakeNet) dials() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.dialed...)
}

// countingServer is an httptest server that records how many requests it
// actually served. "The guard refused" and "the internal service was read"
// are different claims, and only the second one is the finding.
func countingServer(t *testing.T, body string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return ts, &hits
}

// TestBlockedIPReasonSeesThroughIPv6TransitionAddresses: an IPv6 address
// can CARRY an IPv4 one, and the predicate set only ever saw the one form
// (`::ffff:a.b.c.d`) that net.IP.To4 normalizes for it.
//
// Measured before the fix, all reported as ordinary global unicast and
// therefore dialable: `::127.0.0.1`, `64:ff9b::7f00:1`, `64:ff9b::c0a8:1`,
// `2002:7f00:1::`. The URL reaching this guard comes from a model that has
// just read somebody else's page, so `http://[64:ff9b::c0a8:1]/` in that
// page is a request to 192.168.0.1 on any network with a NAT64 gateway.
//
// The allow half of the table is the point of the fix and not an
// afterthought: on a NAT64 network the well-known prefix is how every
// IPv4 host is reached, so blocking the prefix outright would take the
// public internet away from the fetcher rather than take the LAN away
// from an attacker.
func TestBlockedIPReasonSeesThroughIPv6TransitionAddresses(t *testing.T) {
	for _, tc := range []struct {
		addr    string
		blocked bool
		why     string
	}{
		// The forms that walked past it.
		{"::127.0.0.1", true, "IPv4-compatible (RFC 4291) wrapping loopback"},
		{"0:0:0:0:0:0:7f00:1", true, "the same address, spelled out"},
		{"64:ff9b::7f00:1", true, "NAT64 well-known prefix wrapping 127.0.0.1"},
		{"64:ff9b::c0a8:1", true, "NAT64 wrapping 192.168.0.1"},
		{"64:ff9b::a9fe:a9fe", true, "NAT64 wrapping link-local 169.254.169.254"},
		{"64:ff9b::6440:1", true, "NAT64 wrapping CGNAT/tailscale 100.64.0.1"},
		{"2002:7f00:1::", true, "6to4 wrapping 127.0.0.1"},
		{"2002:c0a8:132::", true, "6to4 wrapping 192.168.1.50"},
		// Already refused before the fix, and must stay refused.
		{"::1", true, "v6 loopback"},
		{"::ffff:127.0.0.1", true, "IPv4-mapped loopback"},
		{"fd00::1", true, "unique-local"},
		{"fe80::1", true, "link-local"},
		// Must NOT be refused: a wrapped PUBLIC address is the ordinary
		// way out of a v6-only network.
		{"64:ff9b::101:101", false, "NAT64 wrapping public 1.1.1.1"},
		{"2002:0808:0808::", false, "6to4 wrapping public 8.8.8.8"},
		{"2606:4700:4700::1111", false, "an ordinary public v6 address"},
		{"1.1.1.1", false, "an ordinary public v4 address"},
	} {
		t.Run(tc.addr, func(t *testing.T) {
			ip := net.ParseIP(tc.addr)
			if ip == nil {
				t.Fatalf("unparseable fixture %q", tc.addr)
			}
			reason := blockedIPReason(ip)
			if got := reason != ""; got != tc.blocked {
				t.Fatalf("blockedIPReason(%s) blocked = %v, want %v (%s). reason = %q",
					tc.addr, got, tc.blocked, tc.why, reason)
			}
		})
	}
}

// The headline finding: a fetch aimed straight at a service on this host is
// refused, and refused BEFORE a socket is opened.
func TestDirectFetcherRefusesALoopbackTarget(t *testing.T) {
	internal, hits := countingServer(t,
		`<html><title>Router</title><body>`+strings.Repeat("admin console ", 40)+`</body></html>`)

	f := newDirectFetcher(false)
	// The service is genuinely reachable: the address routes to itself, so
	// the ONLY thing between this fetch and the admin page is the guard. A
	// test whose target happened to be unreachable would pass with the guard
	// deleted, which is the difference between a proof and a coincidence.
	addr := internal.Listener.Addr().String()
	fn := &fakeNet{dns: map[string][]net.IP{}, routes: map[string]string{addr: addr}}
	fn.install(f)

	doc, err := f.Fetch(context.Background(), internal.URL+"/admin")
	if err == nil {
		t.Fatalf("fetched a loopback URL: an agent-supplied URL can read services on the search host (%+v)", doc)
	}
	for _, want := range []string{"127.0.0.1", "loopback", AllowPrivateEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q — it has to name the host, the address and the way out", err, want)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the internal service served %d request(s); the refusal must happen before the connection", n)
	}
	if d := fn.dials(); len(d) != 0 {
		t.Errorf("dialled %v; a blocked address must never reach the socket layer", d)
	}
}

// The reason the check is in the dialer and not on the URL. A URL-level
// allowlist passes this fetch — the requested URL is public — and net/http
// then follows the 302 with no second URL for anything to inspect.
func TestFetchIsRefusedAtTheRedirectHopIntoThePrivateNetwork(t *testing.T) {
	internal, internalHits := countingServer(t,
		`<html><title>NAS</title><body>`+strings.Repeat("internal secret ", 40)+`</body></html>`)

	var publicHits atomic.Int64
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		publicHits.Add(1)
		http.Redirect(w, r, "http://internal.example/secret", http.StatusFound)
	}))
	t.Cleanup(public.Close)

	f := newDirectFetcher(false)
	fn := &fakeNet{
		dns: map[string][]net.IP{
			"public.example":   {net.ParseIP(publicIP)},
			"internal.example": {net.ParseIP("127.0.0.1")},
		},
		routes: map[string]string{
			publicIP + ":80": public.Listener.Addr().String(),
			// The redirect target IS reachable. Without this the hop would
			// fail on its own and the test would stay green with the guard
			// removed — proving a connection error, not a refusal.
			"127.0.0.1:80": internal.Listener.Addr().String(),
		},
	}
	fn.install(f)

	doc, err := f.Fetch(context.Background(), "http://public.example/start")
	if err == nil {
		t.Fatalf("the redirect into the private network was followed: %+v", doc)
	}
	// The first hop MUST have happened, or this test proves only that a
	// fetch failed and nothing about redirects.
	if n := publicHits.Load(); n != 1 {
		t.Fatalf("the public page served %d request(s), want 1 — the fetch never reached the redirect", n)
	}
	if n := internalHits.Load(); n != 0 {
		t.Errorf("the internal service served %d request(s) via a 302; the redirect target is unchecked", n)
	}
	for _, want := range []string{"internal.example", "127.0.0.1", "loopback"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q — the refusal must name the hop, not the original URL", err, want)
		}
	}
	if d := fn.dials(); len(d) != 1 || d[0] != publicIP+":80" {
		t.Errorf("dialled %v, want only the public first hop", d)
	}
}

// Fetching a page off the LAN is a legitimate homelab use, so the opt-out
// has to actually re-enable both paths the two tests above forbid.
func TestAllowPrivateReEnablesTheLocalNetwork(t *testing.T) {
	internal, internalHits := countingServer(t, `<html><title>NAS</title><body>internal secret</body></html>`)

	t.Run("direct loopback target", func(t *testing.T) {
		// No seams installed: the URL carries a literal address, so this runs
		// through the guard's real resolve-and-dial path against a listener
		// on 127.0.0.1:0.
		f := newDirectFetcher(true)

		doc, err := f.Fetch(context.Background(), internal.URL+"/x")
		if err != nil {
			t.Fatalf("the opt-out did not re-enable a loopback fetch: %v", err)
		}
		if !strings.Contains(doc.Text, "internal secret") {
			t.Errorf("doc = %+v, want the internal page's text", doc)
		}
	})

	t.Run("redirect into the private network", func(t *testing.T) {
		before := internalHits.Load()
		public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://internal.example/secret", http.StatusFound)
		}))
		t.Cleanup(public.Close)

		f := newDirectFetcher(true)
		fn := &fakeNet{
			dns: map[string][]net.IP{
				"public.example":   {net.ParseIP(publicIP)},
				"internal.example": {net.ParseIP("127.0.0.1")},
			},
			routes: map[string]string{
				publicIP + ":80": public.Listener.Addr().String(),
				"127.0.0.1:80":   internal.Listener.Addr().String(),
			},
		}
		fn.install(f)

		doc, err := f.Fetch(context.Background(), "http://public.example/start")
		if err != nil {
			t.Fatalf("the opt-out did not re-enable the redirect hop: %v", err)
		}
		if !strings.Contains(doc.Text, "internal secret") {
			t.Errorf("doc = %+v, want the redirect target's text", doc)
		}
		if internalHits.Load() == before {
			t.Error("the internal service was never reached, so this proved nothing about the hop")
		}
	})
}

// The guard has to stay out of the way of the thing this service is FOR.
func TestDirectFetcherStillServesAnExternalHost(t *testing.T) {
	upstream, hits := countingServer(t,
		`<html><head><title>Doc</title></head><body><nav>skip</nav><p>Body text here.</p></body></html>`)

	f := newDirectFetcher(false)
	fn := &fakeNet{
		dns:    map[string][]net.IP{"docs.example": {net.ParseIP(publicIP)}},
		routes: map[string]string{publicIP + ":80": upstream.Listener.Addr().String()},
	}
	fn.install(f)

	doc, err := f.Fetch(context.Background(), "http://docs.example/page")
	if err != nil {
		t.Fatalf("an ordinary internet host was refused: %v", err)
	}
	if doc.Title != "Doc" || !strings.Contains(doc.Text, "Body text here.") {
		t.Errorf("doc = %+v, want title Doc and body text", doc)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("upstream served %d request(s), want 1", n)
	}
}

// DNS rebinding, mechanically: the address the guard APPROVED must be the
// address it connects to. Resolving a name, judging the answer, and then
// handing the NAME to the socket layer re-runs resolution after the verdict,
// and a resolver that answers differently the second time walks straight in.
func TestTheDialGoesToTheAddressThatWasChecked(t *testing.T) {
	internal, internalHits := countingServer(t, `<html><title>NAS</title><body>internal secret</body></html>`)
	external, _ := countingServer(t, `<html><title>Blog</title><body>`+strings.Repeat("public prose ", 40)+`</body></html>`)

	f := newDirectFetcher(false)
	fn := &fakeNet{
		dns: map[string][]net.IP{"rebind.example": {net.ParseIP(publicIP)}},
		routes: map[string]string{
			// What an honest dial of the CHECKED address reaches.
			publicIP + ":80": external.Listener.Addr().String(),
			// What a dial of the NAME reaches — the rebound answer. Present
			// only so that taking this path is observable rather than being
			// an indistinguishable connection error.
			"rebind.example:80": internal.Listener.Addr().String(),
		},
	}
	fn.install(f)

	doc, err := f.Fetch(context.Background(), "http://rebind.example/post")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if strings.Contains(doc.Text, "internal secret") || internalHits.Load() != 0 {
		t.Errorf("the fetch landed on the rebound address: the name was re-resolved after it was checked (doc = %+v)", doc)
	}
	if d := fn.dials(); len(d) != 1 || d[0] != publicIP+":80" {
		t.Errorf("dialled %v, want the checked address %s:80", d, publicIP)
	}
}

// A name with one public answer and one private one must not be let through
// on the strength of the first.
func TestEveryResolvedAddressIsChecked(t *testing.T) {
	internal, _ := countingServer(t,
		`<html><title>NAS</title><body>`+strings.Repeat("internal secret ", 40)+`</body></html>`)

	f := newDirectFetcher(false)
	fn := &fakeNet{
		dns: map[string][]net.IP{
			"mixed.example": {net.ParseIP(publicIP), net.ParseIP("192.168.1.10")},
		},
		// The public answer goes nowhere and the private one is live, which
		// is the arrangement that forces the fall-through: Go's dialer moves
		// to the next address on a connect error, so the second answer is
		// reached on merit rather than by contrivance.
		routes: map[string]string{"192.168.1.10:80": internal.Listener.Addr().String()},
	}
	fn.install(f)

	if doc, err := f.Fetch(context.Background(), "http://mixed.example/x"); err == nil {
		t.Fatalf("a name resolving partly into a private network was fetched: %+v", doc)
	}
	// The public answer is tried first and fails to connect (nothing is
	// routed); the private one must be REFUSED rather than attempted.
	for _, got := range fn.dials() {
		if strings.HasPrefix(got, "192.168.") {
			t.Errorf("dialled %s: a private answer was attempted after a public one failed", got)
		}
	}
}

func TestBlockedIPReasonCoversTheNonGlobalClasses(t *testing.T) {
	blocked := map[string]string{
		"127.0.0.1":            "loopback",
		"127.4.5.6":            "the whole of 127/8, not just the canonical address",
		"::1":                  "IPv6 loopback",
		"::ffff:127.0.0.1":     "an IPv4-mapped IPv6 loopback — the form that walks past a v6-only check",
		"::ffff:192.168.1.1":   "an IPv4-mapped IPv6 RFC 1918 address",
		"10.0.0.5":             "RFC 1918",
		"172.16.0.1":           "RFC 1918",
		"172.31.255.254":       "the top of RFC 1918's 172.16/12",
		"192.168.1.1":          "RFC 1918",
		"fc00::1":              "IPv6 unique-local",
		"fd12:3456::1":         "IPv6 unique-local, the half everyone actually uses",
		"169.254.169.254":      "link-local",
		"fe80::1":              "IPv6 link-local",
		"224.0.0.1":            "multicast",
		"ff02::1":              "IPv6 link-local multicast",
		"ff01::1":              "IPv6 interface-local multicast",
		"0.0.0.0":              "the unspecified address",
		"::":                   "the IPv6 unspecified address",
		"0.1.2.3":              "0.0.0.0/8",
		"255.255.255.255":      "the broadcast address",
		"100.64.0.1":           "RFC 6598 shared address space, which is Tailscale's range",
		"100.127.255.255":      "the top of 100.64.0.0/10",
		"::ffff:169.254.169.2": "an IPv4-mapped link-local address",
	}
	for addr, why := range blocked {
		ip := net.ParseIP(addr)
		if ip == nil {
			t.Fatalf("test bug: %q is not an IP", addr)
		}
		if reason := blockedIPReason(ip); reason == "" {
			t.Errorf("blockedIPReason(%s) = \"\" (allowed), want a refusal: it is %s", addr, why)
		}
	}

	allowed := []string{publicIP, "8.8.8.8", "1.1.1.1", "2606:4700::1111", "100.63.255.255", "100.128.0.1", "172.32.0.1", "172.15.0.1"}
	for _, addr := range allowed {
		ip := net.ParseIP(addr)
		if ip == nil {
			t.Fatalf("test bug: %q is not an IP", addr)
		}
		if reason := blockedIPReason(ip); reason != "" {
			t.Errorf("blockedIPReason(%s) = %q, want it allowed — a guard that blocks the internet is a broken fetcher", addr, reason)
		}
	}
}

// The opt-out is a security default, so "off unless the operator said the
// word" is the property, not "off unless something truthy is set".
func TestPrivateFetchIsOffUnlessTheEnvSaysExactlyOne(t *testing.T) {
	for _, v := range []string{"", "0", "true", "TRUE", "yes", "2", " 1", "1 ", "on"} {
		t.Setenv(AllowPrivateEnv, v)
		if AllowPrivateFetch() {
			t.Errorf("%s=%q turned the guard off; only \"1\" may", AllowPrivateEnv, v)
		}
	}
	t.Setenv(AllowPrivateEnv, "1")
	if !AllowPrivateFetch() {
		t.Errorf("%s=1 did not turn the guard off, so the documented escape hatch does not exist", AllowPrivateEnv)
	}
}

// NewFetcher is where the flag crosses from the command into the fetcher.
// The zero Options must produce a guarded fetcher: a caller that forgets the
// field gets the safe thing.
func TestNewFetcherGuardsUnlessOptionsSayOtherwise(t *testing.T) {
	f, err := NewFetcher("direct", Options{})
	if err != nil {
		t.Fatalf("NewFetcher: %v", err)
	}
	if f.(*directFetcher).guard.allowPrivate {
		t.Error("a zero Options produced an UNGUARDED fetcher — forgetting the field must not open the host")
	}
	f2, err := NewFetcher("direct", Options{AllowPrivateFetch: true})
	if err != nil {
		t.Fatalf("NewFetcher: %v", err)
	}
	if !f2.(*directFetcher).guard.allowPrivate {
		t.Error("Options.AllowPrivateFetch did not reach the fetcher's guard")
	}
}

// The HTTP surface, end to end — this is the shape the verifier ran: an
// unauthenticated POST /fetch naming a service on this host.
func TestPostFetchOfALoopbackURLIsRefused(t *testing.T) {
	internal, hits := countingServer(t, `<html><title>Registry</title><body>internal secret</body></html>`)

	srv := &Server{
		Provider: &stubProvider{resp: &Response{}},
		Fetcher:  newDirectFetcher(false),
	}
	ts := newTestServer(t, srv)

	resp, err := http.Post(ts.URL+"/fetch", "application/json",
		strings.NewReader(`{"url":"`+internal.URL+`/admin"}`))
	if err != nil {
		t.Fatalf("POST /fetch: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Errorf("POST /fetch = 200 for a loopback target: this endpoint is an SSRF proxy")
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the internal service served %d request(s) through POST /fetch", n)
	}
}
