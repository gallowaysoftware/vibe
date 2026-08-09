package search

// Where the fetch path is allowed to open a socket.
//
// `POST /fetch` and the MCP `fetch_url` tool take a URL from an agent — in
// practice from a model that has just read a page somebody else wrote — and
// open a connection from the host that runs beside the router. Without a
// check on WHERE that connection goes, this service is a proxy into
// everything the search host can reach: the NAS and router admin pages, the
// container registry, every other cell's HTTP surface. There is no cloud
// metadata endpoint on a homelab box, so the loss is not a stolen IAM
// credential; it is a LAN PIVOT with the operator's own model driving it,
// and under the documented cross-host deployment (`--bind 0.0.0.0`) the
// caller need not even be on this machine.
//
// The check lives in the DIALER rather than on the parsed URL, and that
// choice is the fix. A URL check sees one string, once. The dialer sees
// every socket the client opens, which is three separate holes closed in
// one place:
//
//   - the initial request;
//   - every REDIRECT hop — a public URL that 302s to 127.0.0.1 never meets a
//     second URL check, because there is no second URL: the redirect is
//     followed inside net/http and only the dialer is consulted again;
//   - DNS REBINDING — a name that answers public when it is checked and
//     private when it is dialled. Closed by connecting to the ADDRESS that
//     was validated instead of handing the name back to the resolver.
//
// The guard is on the `direct` fetcher only, deliberately. The SearXNG
// upstream (`--search-upstream`) and the Tavily API are addresses the
// OPERATOR chose, and a private SearXNG container is the documented
// zero-cost deployment — guarding it would break that for no gain. The
// attacker picks the direct fetcher's target and nothing else here.

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"
)

// AllowPrivateEnv, when set to "1", turns the guard below OFF for the whole
// process. Off by default; `cmd/vibe-search` logs loudly at startup when it
// is on.
//
// It exists because fetching a page off the LAN is a legitimate homelab use
// — the operator's own wiki, a NAS release-notes page — and a guard with no
// way out gets deleted rather than configured. Spelled as an env var read as
// exactly "1", matching VAMP_NO_CACHE and VIBE_MUTATION_TEST, so a stray
// `VIBE_SEARCH_ALLOW_PRIVATE=0` in a unit file cannot read as "on".
const AllowPrivateEnv = "VIBE_SEARCH_ALLOW_PRIVATE"

// AllowPrivateFetch reports whether the operator has opted out of the
// non-global address guard.
func AllowPrivateFetch() bool { return os.Getenv(AllowPrivateEnv) == "1" }

// dialGuard is an http.Transport DialContext that refuses to connect to an
// address which is not globally routable.
type dialGuard struct {
	// allowPrivate disables the refusal. The resolve-then-dial-the-checked-IP
	// behaviour stays either way, so the opt-out changes what is permitted
	// and not how a connection is made.
	allowPrivate bool
	// lookupIP and dialAddr are the two seams this guard is proven through.
	// Both are set to the real thing by newDialGuard; the tests substitute
	// them so the whole proof — loopback refused, redirect hop refused,
	// external host still served — runs with no DNS lookup and no packet
	// leaving the machine.
	lookupIP func(ctx context.Context, host string) ([]net.IP, error)
	dialAddr func(ctx context.Context, network, addr string) (net.Conn, error)
}

func newDialGuard(allowPrivate bool) *dialGuard {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return &dialGuard{
		allowPrivate: allowPrivate,
		lookupIP: func(ctx context.Context, host string) ([]net.IP, error) {
			addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			out := make([]net.IP, 0, len(addrs))
			for _, a := range addrs {
				out = append(out, a.IP)
			}
			return out, nil
		},
		dialAddr: d.DialContext,
	}
}

// DialContext resolves addr, refuses every non-global answer, and connects
// to an address it has actually checked.
func (g *dialGuard) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("fetch: cannot parse dial address %q: %w", addr, err)
	}
	ips, err := g.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	// Every answer is checked, not just the first. A name that resolves to
	// one public address and one private one is the cheapest way past a
	// first-answer-only check, and Go's own dialer falls through to the next
	// address on any connect error.
	var lastErr error
	for _, ip := range ips {
		if !g.allowPrivate {
			if reason := blockedIPReason(ip); reason != "" {
				// Name the host AND the address it landed on. The interesting
				// case is a public NAME that resolves somewhere private, and
				// an error printing only one of the two sends the operator
				// looking at the wrong thing.
				lastErr = fmt.Errorf("refusing to connect to %s: it resolves to %s, which is %s. "+
					"Set %s=1 on vibe-search to allow page fetches into the local network",
					host, ip, reason, AllowPrivateEnv)
				continue
			}
		}
		// Dial the address that was CHECKED, never the name. Passing the
		// hostname on would re-run resolution after the verdict, which is
		// exactly the rebinding hole this guard exists to close.
		conn, dialErr := g.dialAddr(ctx, network, net.JoinHostPort(ip.String(), port))
		if dialErr != nil {
			lastErr = dialErr
			continue
		}
		return conn, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("fetch: %s resolved to no usable address", host)
	}
	return nil, lastErr
}

// resolve turns a dial host into the addresses that will be checked. A
// literal IP is its own answer — asking a resolver about it would be a no-op
// at best and a way to lose the literal at worst.
func (g *dialGuard) resolve(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	ips, err := g.lookupIP(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("fetch: resolve %s: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("fetch: %s resolved to no addresses", host)
	}
	return ips, nil
}

// cgnat is RFC 6598 shared address space. Listed explicitly because the
// stdlib does not call it private and because it is Tailscale's range: on a
// fleet stitched together with a tailnet, 100.64.0.0/10 IS the LAN this
// guard exists to keep a fetched page away from.
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// blockedIPReason names why an address is off limits for a page fetch, or
// returns "" when it is a fine thing to connect to.
//
// The wording of the reason is load-bearing: it goes into the error the
// caller sees, and "which is loopback" is the difference between an operator
// understanding the refusal and filing a bug about DNS.
//
// Two passes, because an IPv6 address can CARRY an IPv4 one. The direct
// predicates below see `::ffff:127.0.0.1` (net.IP.To4 normalizes it) and
// nothing else: measured, `::127.0.0.1`, `64:ff9b::c0a8:1` and
// `2002:7f00:1::` all came back as ordinary global unicast. Each is a
// standard SSRF bypass, and the URL here is supplied by a model that has
// just read a page somebody else wrote — `http://[64:ff9b::c0a8:1]/` in
// that page is a request to 192.168.0.1 on any network with a NAT64
// gateway, which is what a v6-only fleet has by construction.
//
// The wrapped address is UNWRAPPED and judged by the same rules rather
// than the prefix being blocked outright, which matters: on a NAT64
// network the well-known prefix is how every IPv4 host is reached, so
// refusing it wholesale would stop the fetcher reaching the public
// internet at all. 64:ff9b::1.1.1.1 is fine; 64:ff9b::192.168.0.1 is the
// LAN.
func blockedIPReason(ip net.IP) string {
	if reason := directBlockedReason(ip); reason != "" {
		return reason
	}
	if v4 := embeddedIPv4(ip); v4 != nil {
		if reason := directBlockedReason(v4); reason != "" {
			return reason + " (wrapped in an IPv6 transition address, which resolves to " + v4.String() + ")"
		}
	}
	return ""
}

// embeddedIPv4 returns the IPv4 address an IPv6 address carries, for the
// three transition encodings that carry one at a FIXED offset, or nil.
//
// Bounded on purpose. RFC 6052 also allows a network-specific NAT64
// prefix at /32, /40, /48, /56 or /64, and this cannot know which an
// operator chose — an unknown prefix stays an ordinary v6 address here,
// and the operator's answer for a network like that is the same
// VIBE_SEARCH_ALLOW_PRIVATE escape hatch the rest of this file documents,
// in reverse. What is covered is what an attacker can use without knowing
// anything about the target's network.
func embeddedIPv4(ip net.IP) net.IP {
	if len(ip) != net.IPv6len || ip.To4() != nil {
		// Not v6, or already an IPv4-mapped form the direct pass saw.
		return nil
	}
	switch {
	case ip[0] == 0x20 && ip[1] == 0x02:
		// 6to4, 2002:AABB:CCDD::/48 — the v4 address is bytes 2..5.
		return net.IPv4(ip[2], ip[3], ip[4], ip[5])
	case ip[0] == 0x00 && ip[1] == 0x64 && ip[2] == 0xff && ip[3] == 0x9b:
		// NAT64's well-known prefix 64:ff9b::/96 (and RFC 8215's
		// 64:ff9b:1::/48), which RFC 6052 §2.1 fixes at /96 — last four
		// bytes.
		return net.IPv4(ip[12], ip[13], ip[14], ip[15])
	}
	// IPv4-compatible ::a.b.c.d (RFC 4291 §2.5.5.1, deprecated). `::` and
	// `::1` share the twelve zero bytes and are already refused by the
	// unspecified and loopback predicates, so reaching here with one is
	// harmless — but 0.0.0.0/8 catches them anyway.
	for _, b := range ip[:12] {
		if b != 0 {
			return nil
		}
	}
	return net.IPv4(ip[12], ip[13], ip[14], ip[15])
}

// directBlockedReason is the predicate set, applied to one address as
// given. blockedIPReason runs it twice — see there.
func directBlockedReason(ip net.IP) string {
	if len(ip) == 0 {
		return "not an IP address"
	}
	// Normalize the IPv4-mapped IPv6 forms first (::ffff:127.0.0.1 and
	// friends). To4 is what makes the whole switch below see the 4-byte
	// shape, so a mapped loopback cannot walk past the v4 predicates and
	// come out the far end as an ordinary global v6 address.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	switch {
	case ip.IsUnspecified():
		return `the unspecified address ("this host")`
	case ip.IsLoopback():
		return "loopback"
	case ip.IsPrivate():
		return "a private network (RFC 1918, or RFC 4193 fc00::/7)"
	case ip.IsLinkLocalUnicast():
		return "link-local (169.254.0.0/16 or fe80::/10)"
	case ip.IsLinkLocalMulticast():
		return "link-local multicast"
	case ip.IsInterfaceLocalMulticast():
		return "interface-local multicast"
	case ip.IsMulticast():
		return "multicast, which is never a page"
	case cgnat.Contains(ip):
		return "RFC 6598 shared address space (100.64.0.0/10, also Tailscale's range)"
	case len(ip) == net.IPv4len && ip[0] == 0:
		return `0.0.0.0/8 ("this network")`
	case !ip.IsGlobalUnicast():
		// Broadcast, and every other class the stdlib declines to call a
		// routable unicast address. Last on purpose: the cases above exist to
		// produce a reason an operator recognises, and this is the catch-all
		// that keeps a class nobody enumerated from being allowed by default.
		return "not a globally routable unicast address"
	}
	return ""
}
