// Package search is vibe's central retrieval plane: one service, deployed
// beside the router, serving the two capabilities every coding harness wants
// and neither can supply well on its own — web search and page extraction.
//
// Why this is framework and not house config: a harness needs three things
// pointed at infrastructure — models, search, fetch. vibe already centralizes
// the first through the router. Centralizing the other two turns harness
// setup into three URLs, and keeps every paid credential on one host. The
// house-specific part is only the address and which provider is configured;
// that lives in the operator's config, not here.
//
// Why not an MCP server for the search half: MCP is the harness's LOCAL
// identity plane — servers are configured per profile and many authenticate
// through a browser flow on the machine the harness runs on. Shared
// infrastructure behind a single server-side credential does not belong
// there. Search also has a native path in every harness worth supporting,
// with its own result rendering and provider chain, and routing it through a
// generic tool call would throw that away.
//
// The search wire contract is therefore SearXNG's JSON API — a deliberate
// impersonation. Harnesses hardcode their search providers' hosts; SearXNG is
// one of the few whose endpoint is redirectable (oh-my-pi exposes
// SEARXNG_ENDPOINT, while TAVILY_API_KEY has no TAVILY_BASE_URL companion).
// Speaking SearXNG is what lets an unmodified harness route through this
// service and keep its native search path.
//
// Fetch has no such seam: oh-my-pi's providers.fetch chooses among built-in
// readers with hardcoded endpoints. Page extraction is therefore exposed over
// MCP, which is the one capability with no native hook — a narrow, deliberate
// exception to the layering rule above, not a general pattern.
package search

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Result is one search hit, normalized across providers. The field set is
// deliberately the intersection the SearXNG envelope can carry — anything
// richer is dropped on the way out, so providers shouldn't spend effort
// populating more.
type Result struct {
	URL     string
	Title   string
	Content string
	// Score is the provider's own relevance score when it reports one.
	// Passed through untouched; clients sort by array order regardless.
	Score float64
	// Published is the publication timestamp when the provider reports one.
	// Zero means unknown — renderers show a relative age ("1d ago") only when
	// this is set, so inventing a value is worse than leaving it absent.
	Published time.Time
}

// Response is a provider's answer to one query.
type Response struct {
	Results []Result
	// Answer is a synthesized answer where the provider offers one (Tavily's
	// `answer`). Carried because it costs nothing to pass along; note that
	// not every client renders it — oh-my-pi did not in testing.
	Answer string
	// Suggestions are related-query suggestions, rendered by oh-my-pi under
	// "Related".
	Suggestions []string
}

// Query is a normalized search request. Providers map it onto their own
// parameter names and ignore what they cannot express, because a
// slightly-too-broad search beats a failed one.
type Query struct {
	Text string
	// Limit is the max results wanted; zero means the provider's default.
	// Frequently zero even when the caller wanted a specific count: some
	// clients (oh-my-pi among them) truncate client-side and never send one.
	Limit int
	// Recency bounds results to roughly the last N days. Zero means no bound.
	Recency int
	// Page is the 1-based result page. Zero and one both mean the first page.
	Page int
}

// Document is one extracted page.
type Document struct {
	URL   string
	Title string
	// Text is the readable body, already stripped of navigation and markup.
	// The whole point of the extract path is that the caller gets prose, not
	// HTML it has to clean up itself.
	Text string
	// SourceBytes is the size of the raw document the text was extracted
	// from, when the extractor knows it. It is the signal that separates
	// "this page is genuinely short" from "this page is a shell waiting for
	// JavaScript": a small document yielding little text is honest, while a
	// large one yielding almost none means the content never arrived.
	// Zero means unknown, and callers must not treat that as a small page.
	SourceBytes int
}

// Provider is one upstream search API. Implementations must be safe for
// concurrent use — the server shares a single instance across requests.
type Provider interface {
	// Name is the provider id used in logs and /healthz.
	Name() string
	// Search runs one query. It must respect ctx deadlines: the server
	// budgets the whole request, and a provider that blocks past the budget
	// turns a slow search into a hung agent.
	Search(ctx context.Context, q Query) (*Response, error)
}

// Fetcher extracts readable text from a URL. Split from Provider because the
// two halves are independently configurable: paying for search while fetching
// pages directly (or the reverse) is a legitimate deployment.
type Fetcher interface {
	Name() string
	Fetch(ctx context.Context, rawURL string) (*Document, error)
}

// ErrUnauthorized is returned when an upstream rejects the credential, so the
// server can answer with a message that names the cause. An expired key or an
// exhausted quota is the single most likely operational failure of this
// service, and it should be obvious in the logs rather than looking like a
// network blip.
var ErrUnauthorized = errors.New("upstream rejected the credential")

// Options configure provider construction. APIKey is required by paid
// providers and ignored by free ones; UpstreamURL is required by providers
// that front another service (searxng).
type Options struct {
	APIKey      string
	UpstreamURL string
	// AllowPrivateFetch turns the direct fetcher's non-global address guard
	// off. Ignored by every other provider — see dialguard.go for why the
	// guard is scoped to the one fetcher whose target an attacker picks.
	// The zero value is the safe one on purpose: a caller that forgets this
	// field gets the guard.
	AllowPrivateFetch bool
}

// NewProvider builds a search provider by id. The id set is closed rather
// than plugin-based on purpose: each provider is a hand-written adapter
// against a bespoke API, so "add a provider" is a code change either way.
//
// "searxng" here means a REAL upstream SearXNG that this service proxies —
// the zero-API-cost path. It is not the wire format the server speaks, which
// is always SearXNG regardless of the provider behind it.
func NewProvider(id string, o Options) (Provider, error) {
	switch id {
	case "tavily":
		if o.APIKey == "" {
			return nil, errors.New("tavily: api key is required (set TAVILY_API_KEY)")
		}
		return newTavily(o.APIKey), nil
	case "searxng":
		if o.UpstreamURL == "" {
			return nil, errors.New("searxng: upstream url is required (set SEARCH_UPSTREAM_URL)")
		}
		return newSearxngUpstream(o.UpstreamURL), nil
	default:
		return nil, fmt.Errorf("unknown search provider %q (known: tavily, searxng)", id)
	}
}

// NewFetcher builds a page fetcher by id. "direct" is the zero-cost path: a
// plain HTTP GET plus local HTML-to-text. It cannot execute JavaScript, which
// is precisely why the paid path exists.
func NewFetcher(id string, o Options) (Fetcher, error) {
	switch id {
	case "tavily":
		if o.APIKey == "" {
			return nil, errors.New("tavily: api key is required (set TAVILY_API_KEY)")
		}
		return newTavily(o.APIKey), nil
	case "direct":
		return newDirectFetcher(o.AllowPrivateFetch), nil
	default:
		return nil, fmt.Errorf("unknown fetcher %q (known: tavily, direct)", id)
	}
}
