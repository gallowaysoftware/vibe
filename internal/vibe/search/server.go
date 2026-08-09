package search

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Server exposes the retrieval plane over HTTP.
//
// Two surfaces, deliberately different in shape:
//
//   - GET /search speaks SearXNG's JSON contract, because that is the only
//     search endpoint mainstream harnesses let you redirect. Clients keep
//     their native search path.
//   - POST /fetch is our own small JSON contract, because no harness offers a
//     redirectable fetch endpoint at all. It is what the MCP fetch_url tool
//     calls, and is usable directly by anything else on the network.
type Server struct {
	Provider Provider
	// Fetcher is the primary page extractor.
	Fetcher Fetcher
	// Escalate is an optional second extractor tried when the primary returns
	// something that looks like an unrendered shell. This is the two-tier
	// path: static extraction is free, instant, and scores ~0.94 F1 on
	// article-shaped HTML, but returns near-nothing on the ~20-25% of pages
	// that only populate after JavaScript runs. Detecting that cheaply and
	// escalating only then keeps the paid call for the cases that need it.
	Escalate Fetcher
	// EscalateBelowChars is the extracted-text length under which a page is
	// treated as unrendered. A real article clears this comfortably; an SPA
	// shell does not. Zero uses defaultEscalateBelowChars.
	EscalateBelowChars int
	// Token, when set, is required as `Authorization: Bearer <token>` on
	// every request. Worth setting even on a trusted VPN: an open endpoint
	// here is an endpoint that spends the operator's search quota, and an
	// agent in a retry loop can spend a lot of it quickly.
	Token   string
	Timeout time.Duration
	Log     *slog.Logger
	// MCP tunes the MCP surface at /mcp, which exists because page fetching
	// has no redirectable endpoint in any mainstream harness.
	MCP MCPOptions
}

// defaultEscalateBelowChars is the shell-detection threshold. Chosen well
// below any real article and well above the handful of characters an empty
// SPA shell yields, so the classification is unambiguous in both directions
// rather than tuned to a particular site.
const defaultEscalateBelowChars = 500

func (s *Server) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Server) timeout() time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	return defaultTimeout
}

// Handler returns the mux serving both surfaces plus /healthz.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/search", s.handleSearch)
	mux.HandleFunc("/fetch", s.handleFetch)
	mux.HandleFunc("/mcp", s.handleMCP)
	mux.HandleFunc("/healthz", s.handleHealth)
	// Origin outside auth: a browser request from somewhere else is refused
	// before the credential is even looked at, and /healthz — exempt from
	// auth so a supervisor can probe it — is NOT exempt from this, because a
	// readiness probe never carries an Origin and a rebound page does.
	return s.withOriginGuard(s.withAuth(mux))
}

// withOriginGuard refuses any request carrying a browser Origin that is not
// this machine.
//
// Does a JSON service on loopback have a browser threat model at all? Yes,
// and it is the one MCP's own streamable-HTTP transport calls out. A page
// the operator is merely LOOKING AT can POST a JSON-RPC body to
// 127.0.0.1:14003 with no preflight — send it as text/plain and it is a CORS
// "simple request", and nothing here inspects Content-Type. CORS then stops
// the attacker READING the reply… until DNS rebinding, at which point the
// browser believes the response is same-origin and CORS protects nothing.
// That is the whole exfiltration path for `fetch_url`, and the reason this
// is a real control rather than ceremony.
//
// The Origin header is what survives rebinding: it still names the page's
// ORIGINAL origin, because the browser stamps it at request time from the
// document, not from DNS. So the rule is: absent Origin is fine, present
// Origin must be loopback. Non-browser clients — curl, an MCP harness, a
// harness's native SearXNG path — send none and are unaffected, which is why
// this needs no flag and no allowlist to configure.
func (s *Server) withOriginGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && !isLoopbackOrigin(origin) {
			s.logger().Warn("refused a cross-origin request",
				"origin", origin, "path", r.URL.Path, "method", r.Method)
			http.Error(w, "forbidden: cross-origin browser request", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isLoopbackOrigin reports whether an Origin header names a page served from
// this machine. Anything unparseable is not loopback — `Origin: null`, which
// a sandboxed iframe or a file:// document sends, has no host and must be
// refused rather than treated as absent.
func isLoopbackOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// withAuth enforces the bearer token when one is configured. /healthz stays
// open so a supervisor's readiness probe doesn't need the credential.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Token == "" || r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		// Match on the scheme prefix rather than trimming blindly: a `Basic`
		// header is non-empty, so a bare TrimPrefix would leave the whole
		// header in hand and skip the basic-auth branch entirely.
		var got string
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			got = strings.TrimPrefix(auth, "Bearer ")
		}
		// Basic auth as well: SearXNG clients commonly authenticate that way
		// (oh-my-pi exposes SEARXNG_BASIC_USERNAME/PASSWORD), and refusing it
		// would mean the token is unusable from the very clients this service
		// is built for.
		if got == "" {
			if _, pass, ok := r.BasicAuth(); ok {
				got = pass
			}
		}
		if got != s.Token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"search":   s.Provider.Name(),
		"fetch":    s.Fetcher.Name(),
		"escalate": escalateName(s.Escalate),
	})
}

func escalateName(f Fetcher) string {
	if f == nil {
		return ""
	}
	return f.Name()
}

// searxngEnvelope is the response shape SearXNG clients parse. The field set
// is the verified minimum: oh-my-pi consumes results[].{url,title,content},
// optional results[].publishedDate, and suggestions[]. Everything else is
// present because real SearXNG emits it and a missing key is a cheap way to
// trip a stricter client.
type searxngEnvelope struct {
	Query               string       `json:"query"`
	NumberOfResults     int          `json:"number_of_results"`
	Results             []searxngHit `json:"results"`
	Answers             []string     `json:"answers"`
	Corrections         []string     `json:"corrections"`
	Infoboxes           []any        `json:"infoboxes"`
	Suggestions         []string     `json:"suggestions"`
	UnresponsiveEngines [][]string   `json:"unresponsive_engines"`
}

type searxngHit struct {
	URL           string   `json:"url"`
	Title         string   `json:"title"`
	Content       string   `json:"content"`
	Engine        string   `json:"engine"`
	Engines       []string `json:"engines"`
	Score         float64  `json:"score"`
	Category      string   `json:"category"`
	PublishedDate string   `json:"publishedDate,omitempty"`
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		http.Error(w, "missing q", http.StatusBadRequest)
		return
	}
	query := Query{
		Text:    q,
		Page:    atoiDefault(r.URL.Query().Get("pageno"), 1),
		Limit:   atoiDefault(r.URL.Query().Get("limit"), 0),
		Recency: atoiDefault(r.URL.Query().Get("days"), 0),
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout())
	defer cancel()

	started := time.Now()
	resp, err := s.Provider.Search(ctx, query)
	if err != nil {
		s.logger().Error("search failed", "provider", s.Provider.Name(), "query", q, "err", err)
		// 502, not 500: the failure is upstream, and the distinction is what
		// tells the operator whether to look at this service or at the
		// provider's status page.
		http.Error(w, "search upstream failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	env := searxngEnvelope{
		Query:               q,
		NumberOfResults:     len(resp.Results),
		Results:             make([]searxngHit, 0, len(resp.Results)),
		Answers:             []string{},
		Corrections:         []string{},
		Infoboxes:           []any{},
		Suggestions:         resp.Suggestions,
		UnresponsiveEngines: [][]string{},
	}
	if resp.Answer != "" {
		env.Answers = append(env.Answers, resp.Answer)
	}
	if env.Suggestions == nil {
		env.Suggestions = []string{}
	}
	name := s.Provider.Name()
	for _, r := range resp.Results {
		hit := searxngHit{
			URL:      r.URL,
			Title:    r.Title,
			Content:  r.Content,
			Engine:   name,
			Engines:  []string{name},
			Score:    r.Score,
			Category: "general",
		}
		if !r.Published.IsZero() {
			// SearXNG emits naive local timestamps in this field; matching
			// that avoids clients mis-parsing an offset they don't expect.
			hit.PublishedDate = r.Published.Format("2006-01-02T15:04:05")
		}
		env.Results = append(env.Results, hit)
	}
	s.logger().Info("search", "provider", name, "query", q,
		"results", len(env.Results), "ms", time.Since(started).Milliseconds())
	writeJSON(w, http.StatusOK, env)
}

type fetchRequest struct {
	URL string `json:"url"`
}

type fetchResponse struct {
	URL   string `json:"url"`
	Title string `json:"title"`
	Text  string `json:"text"`
	// Extractor names which tier produced this text, so a caller comparing
	// results can tell a free static extraction from an escalated one.
	Extractor string `json:"extractor"`
	Escalated bool   `json:"escalated"`
	// EscalationError is set when the paid tier was tried and failed, leaving
	// the primary's thin text as the answer. Its presence means the page was
	// probably not read successfully, whatever the text length suggests.
	EscalationError string `json:"escalation_error,omitempty"`
}

func (s *Server) handleFetch(w http.ResponseWriter, r *http.Request) {
	var req fetchRequest
	switch r.Method {
	case http.MethodPost:
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "invalid json body", http.StatusBadRequest)
			return
		}
	case http.MethodGet:
		req.URL = r.URL.Query().Get("url")
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if req.URL == "" {
		http.Error(w, "missing url", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout())
	defer cancel()

	doc, meta, err := s.fetchTiered(ctx, req.URL)
	if err != nil {
		s.logger().Error("fetch failed", "url", req.URL, "err", err)
		http.Error(w, "fetch failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	s.logger().Info("fetch", "url", req.URL, "extractor", meta.Extractor,
		"escalated", meta.Escalated, "chars", len(doc.Text))
	writeJSON(w, http.StatusOK, fetchResponse{
		URL: doc.URL, Title: doc.Title, Text: doc.Text,
		Extractor: meta.Extractor, Escalated: meta.Escalated,
		EscalationError: meta.EscalationErr,
	})
}

// fetchMeta describes how an extraction was produced. It exists so a caller
// can tell a clean cheap read from a page that defeated every tier — the
// difference between "here is the article" and "here are 37 characters that
// happen not to be an error".
type fetchMeta struct {
	Extractor string
	Escalated bool
	// EscalationErr records that the paid tier was attempted and failed. That
	// combination is invisible otherwise: the response falls back to the
	// primary's thin text and would look like an ordinary cheap success, even
	// though a credit was spent and the content was never retrieved.
	EscalationErr string
}

// fetchTiered runs the primary extractor and escalates when the result looks
// like an unrendered shell — either because the primary failed outright or
// because it returned too little text to be a real page.
//
// The ordering matters for cost: the cheap path runs first and answers most
// requests, and the paid path is reached only by pages that actually defeated
// it. A primary failure escalates too, since a 403 from a plain GET is often
// exactly the bot-detection case the paid API exists to absorb.
func (s *Server) fetchTiered(ctx context.Context, rawURL string) (*Document, fetchMeta, error) {
	threshold := s.EscalateBelowChars
	if threshold == 0 {
		threshold = defaultEscalateBelowChars
	}
	doc, err := s.Fetcher.Fetch(ctx, rawURL)
	if err == nil && !looksUnrendered(doc, threshold) {
		return doc, fetchMeta{Extractor: s.Fetcher.Name()}, nil
	}
	if s.Escalate == nil {
		if err != nil {
			return nil, fetchMeta{}, err
		}
		// Thin but not empty, with nowhere to escalate: return what we have
		// rather than failing. A short page is a legitimate result, and the
		// caller can see the length for itself.
		return doc, fetchMeta{Extractor: s.Fetcher.Name()}, nil
	}
	why := "thin result"
	if err != nil {
		why = err.Error()
	}
	s.logger().Info("escalating fetch", "url", rawURL,
		"from", s.Fetcher.Name(), "to", s.Escalate.Name(), "why", why)
	esc, escErr := s.Escalate.Fetch(ctx, rawURL)
	if escErr != nil {
		s.logger().Warn("escalation failed", "url", rawURL,
			"escalator", s.Escalate.Name(), "err", escErr)
		// Prefer the primary's output if it produced anything at all: a thin
		// page beats an error when the escalation also failed. But say so —
		// silently returning the thin text hides both the spend and the fact
		// that the page was never really read.
		if err == nil {
			return doc, fetchMeta{
				Extractor:     s.Fetcher.Name(),
				EscalationErr: escErr.Error(),
			}, nil
		}
		return nil, fetchMeta{}, fmt.Errorf("%s: %w (escalation: %v)", s.Fetcher.Name(), err, escErr)
	}
	return esc, fetchMeta{Extractor: s.Escalate.Name(), Escalated: true}, nil
}

// minShellSourceBytes is how large a raw document must be before "almost no
// text" is read as an unrendered shell rather than an honestly short page.
// An SPA ships a bundle reference and scaffolding well past this; a small
// static page like example.com (~1.2 KB yielding ~150 chars) does not, and
// escalating those to a paid extractor spends credits to re-fetch text we
// already had in full.
const minShellSourceBytes = 8 << 10 // 8 KiB

// looksUnrendered reports whether an extraction should be retried through the
// escalation path. Thin text alone is not enough — a short page is a valid
// result. The tell is a LOT of document producing almost no prose, which is
// what a client-rendered shell looks like.
//
// SourceBytes == 0 means the extractor didn't report a size. In that case
// thin text is the only signal available, so it is used on its own.
func looksUnrendered(doc *Document, threshold int) bool {
	if doc == nil {
		return true
	}
	if len(strings.TrimSpace(doc.Text)) >= threshold {
		return false
	}
	if doc.SourceBytes == 0 {
		return true
	}
	return doc.SourceBytes >= minShellSourceBytes
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return def
	}
	return n
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func decodeJSON(r io.Reader, v any) error {
	return json.NewDecoder(r).Decode(v)
}
