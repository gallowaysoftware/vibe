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
package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
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
		bind      = flag.String("bind", "127.0.0.1:14003", "address to listen on (use 0.0.0.0:PORT to serve the LAN/VPN)")
		provider  = flag.String("search-provider", "tavily", "search provider: tavily | searxng")
		upstream  = flag.String("search-upstream", envOr("SEARCH_UPSTREAM_URL", ""), "upstream base URL (required for --search-provider searxng)")
		fetcher   = flag.String("fetch", "direct", "primary page extractor: direct | tavily")
		escalate  = flag.String("escalate", "", "extractor to escalate to when the primary returns an unrendered shell (e.g. tavily); empty disables escalation")
		thresh    = flag.Int("escalate-below", 0, "escalate when extracted text is shorter than this many characters (0 uses the built-in default)")
		timeout   = flag.Duration("timeout", 30*time.Second, "per-request budget")
		mcpSearch = flag.Bool("mcp-expose-search", false, "also expose web_search over MCP (off by default: clients with a redirectable search endpoint should use their native path)")
	)
	flag.Parse()

	apiKey := os.Getenv("TAVILY_API_KEY")
	// Fail at startup rather than on the first request: a service that boots
	// happily and 502s every search is far harder to diagnose than one that
	// refuses to start with the reason on stderr.
	opts := search.Options{APIKey: apiKey, UpstreamURL: *upstream}
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

	// The token is read from the environment only. A --token flag would put
	// the secret in the process list for every user on the host.
	token := os.Getenv("VIBE_SEARCH_TOKEN")
	if token == "" {
		slog.Warn("no VIBE_SEARCH_TOKEN set: this endpoint is unauthenticated, and any caller that reaches it can spend the configured search quota")
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
		"fetch", f.Name(), "escalate", escalateName(esc), "authenticated", token != "")
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
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
