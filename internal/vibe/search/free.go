package search

// The zero-API-cost path: a real SearXNG upstream for search, and a plain
// HTTP GET plus local HTML-to-text for fetch. Both are deliberately
// dependency-free so this service stays a single static binary with nothing
// to install alongside it.
//
// The tradeoff is honest and should stay documented rather than discovered:
// the direct fetcher cannot execute JavaScript, so client-rendered pages come
// back empty or as a shell. That is the failure the paid extract path buys
// its way out of.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// searxngUpstream proxies a real SearXNG instance. The server speaks the
// SearXNG contract either way, so this provider is nearly a passthrough — it
// exists so one deployment can switch between paid and free search with a
// config change instead of a different service.
type searxngUpstream struct {
	base string
	hc   *http.Client
}

// newSearxngUpstream's client is deliberately unguarded. The base URL comes
// from --search-upstream, an address the OPERATOR chose, and the documented
// zero-cost deployment points it at a SearXNG container on a private
// network. The dial guard exists for targets an attacker picks; applying it
// here would break the shipped configuration and protect nothing.
func newSearxngUpstream(base string) *searxngUpstream {
	return &searxngUpstream{base: strings.TrimRight(base, "/"), hc: &http.Client{}}
}

func (s *searxngUpstream) Name() string { return "searxng" }

type searxngWire struct {
	Results []struct {
		URL           string  `json:"url"`
		Title         string  `json:"title"`
		Content       string  `json:"content"`
		Score         float64 `json:"score"`
		PublishedDate string  `json:"publishedDate"`
	} `json:"results"`
	Answers     []string `json:"answers"`
	Suggestions []string `json:"suggestions"`
}

func (s *searxngUpstream) Search(ctx context.Context, q Query) (*Response, error) {
	v := url.Values{}
	v.Set("q", q.Text)
	v.Set("format", "json")
	if q.Page > 1 {
		v.Set("pageno", strconv.Itoa(q.Page))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/search?"+v.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("searxng: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("searxng: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("searxng: status %s", resp.Status)
	}
	var wire searxngWire
	if err := decodeJSON(resp.Body, &wire); err != nil {
		return nil, fmt.Errorf("searxng: decode: %w", err)
	}
	out := &Response{Suggestions: wire.Suggestions, Results: make([]Result, 0, len(wire.Results))}
	if len(wire.Answers) > 0 {
		out.Answer = wire.Answers[0]
	}
	for _, r := range wire.Results {
		out.Results = append(out.Results, Result{
			URL:       r.URL,
			Title:     r.Title,
			Content:   r.Content,
			Score:     r.Score,
			Published: parseTavilyDate(r.PublishedDate),
		})
	}
	return out, nil
}

// maxFetchBytes caps what the direct fetcher will read from one page. A
// missing or hostile Content-Length must not be able to exhaust memory on the
// host that also serves the router.
const maxFetchBytes = 4 << 20 // 4 MiB

type directFetcher struct {
	hc *http.Client
	// guard is held so its two seams stay reachable from the package's own
	// tests. Nothing outside this package touches it.
	guard *dialGuard
}

func newDirectFetcher(allowPrivate bool) *directFetcher {
	guard := newDialGuard(allowPrivate)
	return &directFetcher{
		guard: guard,
		hc: &http.Client{
			Transport: &http.Transport{
				// The whole point of this file's security posture, in one
				// field: every socket this client opens — first request,
				// each redirect hop, each address a name resolves to — goes
				// through the guard. See dialguard.go for why the check
				// cannot live on the parsed URL.
				DialContext: guard.DialContext,
				// An operator's egress proxy stays honoured, as it was when
				// this client was http.DefaultTransport. Note the boundary:
				// with a proxy configured the socket goes to the PROXY, so
				// the proxy — not this guard — is what decides where the
				// request ultimately lands.
				Proxy:                 http.ProxyFromEnvironment,
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          100,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
			// Bound redirect chains; the default is already 10 but making it
			// explicit documents that a redirect loop is a fetch failure, not
			// a hang the caller's context has to clean up.
			//
			// Deliberately NOT where the address check happens: a URL check
			// here would see only the hops net/http chooses to report, and
			// would still be resolving a name it does not then dial.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return fmt.Errorf("stopped after %d redirects", len(via))
				}
				return nil
			},
		},
	}
}

func (d *directFetcher) Name() string { return "direct" }

func (d *directFetcher) Fetch(ctx context.Context, rawURL string) (*Document, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		// url.Error again, and this one quotes the WHOLE string it failed
		// to parse — including anything a caller put in the query.
		return nil, fmt.Errorf("direct fetch: parse url: %w", causeWithoutURL(err))
	}
	// Reject non-HTTP schemes outright: this handler takes a URL from an
	// agent, and file:// or similar would turn a page fetch into local disk
	// access on the fleet host.
	//
	// This is the scheme check and ONLY the scheme check. Where the request
	// is allowed to land is decided at dial time (see dialguard.go), because
	// a check on this URL would miss every redirect hop and every rebind.
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("direct fetch: unsupported scheme %q (http and https only)", u.Scheme)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("direct fetch: build request: %w", err)
	}
	// A default Go user-agent gets blocked by a meaningful share of sites.
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; vibe-search/1.0)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.5")
	resp, err := d.hc.Do(req)
	if err != nil {
		// Both halves, and neither covers the other: redactURL hides what
		// we print, causeWithoutURL drops the second copy net/http embeds
		// in *url.Error (which strips only the password). See redact.go.
		return nil, fmt.Errorf("direct fetch %s: %w", redactURL(rawURL), causeWithoutURL(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("direct fetch %s: status %s", redactURL(rawURL), resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes))
	if err != nil {
		return nil, fmt.Errorf("direct fetch %s: read body: %w", redactURL(rawURL), causeWithoutURL(err))
	}
	html := string(raw)
	return &Document{
		URL:         rawURL,
		Title:       htmlTitle(html),
		Text:        htmlToText(html),
		SourceBytes: len(raw),
	}, nil
}

var (
	// Script and style content is removed wholesale before tag stripping —
	// otherwise a page's JS source ends up in the "readable text" as a wall
	// of noise that costs the caller real context tokens.
	//
	// One regex per tag rather than one alternation with a backreference:
	// Go's RE2 has no backreferences, so `<(script|style)…</\1>` does not
	// compile. Matching each tag to its own closer is the portable form.
	dropBlockREs = compileDropBlocks("script", "style", "noscript", "template", "svg")
	commentRE    = regexp.MustCompile(`(?s)<!--.*?-->`)
	// Block-level boundaries become newlines so paragraph structure survives.
	blockBreakRE = regexp.MustCompile(`(?i)</?(p|div|br|li|tr|h[1-6]|section|article|header|footer|blockquote)\b[^>]*>`)
	tagRE        = regexp.MustCompile(`(?s)<[^>]*>`)
	titleRE      = regexp.MustCompile(`(?is)<title\b[^>]*>(.*?)</\s*title\s*>`)
	blankLinesRE = regexp.MustCompile(`\n{3,}`)
	spacesRE     = regexp.MustCompile(`[ \t]{2,}`)
)

// htmlToText is a deliberately small HTML-to-prose reducer: drop scripts and
// comments, turn block tags into line breaks, strip what's left, unescape the
// handful of entities that actually matter, and collapse whitespace.
//
// It is NOT a readability implementation — it keeps navigation and footer
// text a real extractor would discard. That is an accepted limit of the free
// path: good enough for article-shaped pages, and the reason the paid extract
// path is the default recommendation.
func compileDropBlocks(tags ...string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(tags))
	for _, tag := range tags {
		out = append(out, regexp.MustCompile(`(?is)<`+tag+`\b[^>]*>.*?</\s*`+tag+`\s*>`))
	}
	return out
}

func htmlToText(html string) string {
	s := commentRE.ReplaceAllString(html, "")
	for _, re := range dropBlockREs {
		s = re.ReplaceAllString(s, " ")
	}
	s = blockBreakRE.ReplaceAllString(s, "\n")
	s = tagRE.ReplaceAllString(s, "")
	s = unescapeEntities(s)
	// Normalize per line so trailing spaces from stripped tags disappear.
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimSpace(spacesRE.ReplaceAllString(ln, " "))
	}
	s = strings.Join(lines, "\n")
	s = blankLinesRE.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

func htmlTitle(html string) string {
	m := titleRE.FindStringSubmatch(html)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(unescapeEntities(tagRE.ReplaceAllString(m[1], "")))
}

// unescapeEntities handles the named entities that appear in ordinary prose
// plus numeric escapes. A full entity table is not worth carrying: anything
// missed survives as literal text rather than corrupting the output.
func unescapeEntities(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	s = strings.NewReplacer(
		"&nbsp;", " ",
		"&amp;", "&",
		"&lt;", "<",
		"&gt;", ">",
		"&quot;", `"`,
		"&#39;", "'",
		"&apos;", "'",
		"&mdash;", "—",
		"&ndash;", "–",
		"&hellip;", "…",
		"&rsquo;", "'",
		"&lsquo;", "'",
		"&ldquo;", `"`,
		"&rdquo;", `"`,
	).Replace(s)
	return numericEntityRE.ReplaceAllStringFunc(s, func(m string) string {
		digits := strings.TrimSuffix(strings.TrimPrefix(m, "&#"), ";")
		base := 10
		if len(digits) > 0 && (digits[0] == 'x' || digits[0] == 'X') {
			base, digits = 16, digits[1:]
		}
		n, err := strconv.ParseInt(digits, base, 32)
		if err != nil || n <= 0 {
			return m
		}
		return string(rune(n))
	})
}

var numericEntityRE = regexp.MustCompile(`&#[xX]?[0-9a-fA-F]+;`)

// defaultTimeout is the per-request budget when the caller doesn't set one.
// Chosen so a wedged upstream surfaces as an error inside an agent's patience
// rather than as a stalled turn.
const defaultTimeout = 30 * time.Second
