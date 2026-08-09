// Command vibe-search serves vibe's central retrieval plane: web search over
// the SearXNG wire contract, and page extraction over a small JSON API.
//
// It is meant to run beside the router on the fleet host, so a harness needs
// exactly three addresses — models, search, fetch — and no client ever holds
// a search credential.
//
// Zero-cost deployment (no API spend at all):
//
//	vibe-search --search-provider searxng --search-upstream http://searxng:8080 \
//	            --fetch direct
//
// Paid deployment with the two-tier fetch (recommended): static extraction
// first, paid extraction only for pages that defeat it.
//
//	TAVILY_API_KEY=... vibe-search --search-provider tavily \
//	            --fetch direct --escalate tavily
//
// Both forms need VIBE_SEARCH_TOKEN, or an explicit --no-auth. See
// startupAuthCheck for why that is a refusal to start rather than a warning.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/search"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "vibe-search:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		bind      = flag.String("bind", "127.0.0.1:14003", "address to listen on (use 0.0.0.0:PORT to serve the LAN/VPN; that needs VIBE_SEARCH_TOKEN)")
		noAuth    = flag.Bool("no-auth", false, "start without VIBE_SEARCH_TOKEN, serving every route unauthenticated (loopback only; anything else is an open fetch proxy into this host's network)")
		provider  = flag.String("search-provider", "tavily", "search provider: tavily | searxng")
		upstream  = flag.String("search-upstream", envOr("SEARCH_UPSTREAM_URL", ""), "upstream base URL (required for --search-provider searxng)")
		fetcher   = flag.String("fetch", "direct", "primary page extractor: direct | tavily")
		escalate  = flag.String("escalate", "", "extractor to escalate to when the primary returns an unrendered shell (e.g. tavily); empty disables escalation")
		thresh    = flag.Int("escalate-below", 0, "escalate when extracted text is shorter than this many characters (0 uses the built-in default)")
		timeout   = flag.Duration("timeout", 30*time.Second, "per-request budget")
		mcpSearch = flag.Bool("mcp-expose-search", false, "also expose web_search over MCP (off by default: clients with a redirectable search endpoint should use their native path)")
	)
	flag.Parse()

	// The token is read from the environment only. A --token flag would put
	// the secret in the process list for every user on the host.
	token := os.Getenv("VIBE_SEARCH_TOKEN")
	warn, err := startupAuthCheck(token, *noAuth, *bind)
	if err != nil {
		return err
	}
	if warn != "" {
		slog.Warn(warn)
	}

	allowPrivate := search.AllowPrivateFetch()
	if allowPrivate {
		slog.Warn("page fetches into the local network are ALLOWED",
			"env", search.AllowPrivateEnv,
			"effect", "any caller that reaches /fetch or the MCP fetch_url tool can read every HTTP "+
				"service this host can reach — NAS and router admin pages, the registry, other cells. "+
				"Unset it unless this endpoint is as trusted as the network behind it.")
	}

	apiKey := os.Getenv("TAVILY_API_KEY")
	// Fail at startup rather than on the first request: a service that boots
	// happily and 502s every search is far harder to diagnose than one that
	// refuses to start with the reason on stderr.
	opts := search.Options{APIKey: apiKey, UpstreamURL: *upstream, AllowPrivateFetch: allowPrivate}
	p, err := search.NewProvider(*provider, opts)
	if err != nil {
		return err
	}
	f, err := search.NewFetcher(*fetcher, opts)
	if err != nil {
		return err
	}
	var esc search.Fetcher
	if *escalate != "" {
		if esc, err = search.NewFetcher(*escalate, opts); err != nil {
			return fmt.Errorf("escalate: %w", err)
		}
	}

	srv := &search.Server{
		Provider:           p,
		Fetcher:            f,
		Escalate:           esc,
		EscalateBelowChars: *thresh,
		Token:              token,
		Timeout:            *timeout,
		MCP:                search.MCPOptions{ExposeSearch: *mcpSearch},
	}
	hs := &http.Server{
		Addr:    *bind,
		Handler: srv.Handler(),
		// Generous relative to the per-request budget so a slow upstream is
		// reported by the handler (with a useful message) rather than cut off
		// at the transport layer.
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      *timeout + 30*time.Second,
	}
	slog.Info("vibe-search listening", "addr", *bind, "search", p.Name(),
		"fetch", f.Name(), "escalate", escalateName(esc), "authenticated", token != "",
		"allow_private_fetch", allowPrivate)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// startupAuthCheck decides what a token-less start means. It returns a
// warning to log, or an error that stops the process before it listens.
//
// The decision, written down because the previous default WAS the bug: a
// missing VIBE_SEARCH_TOKEN used to log a warning and then serve every route
// to every caller. That fails OPEN — and it fails open on a service whose
// documented deployment is `--bind 0.0.0.0` beside the router, which puts an
// unauthenticated fetch_url in front of every HTTP service this host can
// reach. A warning is the wrong instrument for that: it scrolls past once at
// boot, the service then works perfectly, and nothing ever sends the
// operator back to read it.
//
// So: FAIL CLOSED, with one explicit escape. Refusing to start is loud,
// immediate, and names its own remedy, and `--no-auth` is a single flag for
// the operator who genuinely wants a token-less loopback service.
//
// The other candidate — keep warn-and-start but silently bind loopback-only
// when no token is set — was rejected for being silent. It would change a
// documented deployment without saying so: the cross-host `search_url` that
// profiles/omp.example.yaml wires into a harness would begin answering on
// 127.0.0.1 only, and the operator would debug it from the client end, on
// the wrong host, with nothing in the log to contradict them. A service that
// refuses to start states its problem exactly once, at the place the problem
// is.
//
// `--no-auth` on a non-loopback bind is the combination the verifier proved
// hostile, so it warns rather than passing quietly. It is not an error: the
// operator has already said the quiet part out loud with the flag, and a
// second, unskippable refusal with no escape would be the silent-breakage
// failure mode arriving from the other direction.
func startupAuthCheck(token string, noAuth bool, bind string) (string, error) {
	if token != "" {
		return "", nil
	}
	if !noAuth {
		return "", fmt.Errorf("refusing to start unauthenticated: VIBE_SEARCH_TOKEN is not set\n"+
			"  Every route here fetches arbitrary URLs and spends the configured search quota on\n"+
			"  behalf of whoever reaches it. Set VIBE_SEARCH_TOKEN=<token> (the same value the\n"+
			"  harness sends as `Authorization: Bearer` or as the basic-auth password), or pass\n"+
			"  --no-auth to serve without one on %s", bind)
	}
	if bindsBeyondLoopback(bind) {
		return "--no-auth on a non-loopback --bind (" + bind + "): every host that can reach this " +
			"address can fetch pages through this one and spend the search quota. Set " +
			"VIBE_SEARCH_TOKEN instead.", nil
	}
	return "", nil
}

// bindsBeyondLoopback reports whether a listen address reaches past this
// machine. Anything it cannot read is treated as exposed: the whole point is
// to be wrong in the direction that warns too often rather than the one that
// stays quiet about an open port.
func bindsBeyondLoopback(bind string) bool {
	host, _, err := net.SplitHostPort(bind)
	if err != nil {
		return true
	}
	// ":14003" — an empty host listens on every interface.
	if host == "" {
		return true
	}
	if host == "localhost" {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return true
	}
	return !ip.IsLoopback()
}

func escalateName(f search.Fetcher) string {
	if f == nil {
		return "none"
	}
	return f.Name()
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
