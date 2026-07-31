package search

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type stubProvider struct {
	resp *Response
	err  error
	last Query
}

func (s *stubProvider) Name() string { return "stub" }
func (s *stubProvider) Search(_ context.Context, q Query) (*Response, error) {
	s.last = q
	return s.resp, s.err
}

type stubFetcher struct {
	name  string
	doc   *Document
	err   error
	calls int
}

func (s *stubFetcher) Name() string { return s.name }
func (s *stubFetcher) Fetch(_ context.Context, u string) (*Document, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	d := *s.doc
	d.URL = u
	return &d, nil
}

func newTestServer(t *testing.T, s *Server) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// The search response must match the shape real SearXNG clients parse. These
// are the exact fields observed to be consumed by oh-my-pi against a fake
// endpoint, so a regression here silently breaks every client.
func TestSearchRendersSearxngEnvelope(t *testing.T) {
	published := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	srv := &Server{
		Provider: &stubProvider{resp: &Response{
			Answer:      "synthesized",
			Suggestions: []string{"related one"},
			Results: []Result{
				{URL: "https://example.com/a", Title: "A", Content: "snippet a", Score: 1, Published: published},
				{URL: "https://example.com/b", Title: "B", Content: "snippet b"},
			},
		}},
		Fetcher: &stubFetcher{name: "direct", doc: &Document{Text: "x"}},
	}
	ts := newTestServer(t, srv)

	resp, err := http.Get(ts.URL + "/search?q=hello&format=json&pageno=1")
	if err != nil {
		t.Fatalf("GET /search: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %s, want 200", resp.Status)
	}
	var env searxngEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.Query != "hello" {
		t.Errorf("query = %q, want hello", env.Query)
	}
	if env.NumberOfResults != 2 || len(env.Results) != 2 {
		t.Fatalf("results = %d (number_of_results %d), want 2", len(env.Results), env.NumberOfResults)
	}
	first := env.Results[0]
	if first.URL != "https://example.com/a" || first.Title != "A" || first.Content != "snippet a" {
		t.Errorf("first result = %+v, want url/title/content populated", first)
	}
	// publishedDate drives the client's "1d ago" rendering; a wrong format
	// there shows a confidently incorrect age.
	if first.PublishedDate != "2026-07-30T12:00:00" {
		t.Errorf("publishedDate = %q, want naive local form", first.PublishedDate)
	}
	// An absent timestamp must be omitted entirely rather than sent as a zero
	// date, which would render as the year 1.
	if env.Results[1].PublishedDate != "" {
		t.Errorf("second result publishedDate = %q, want empty", env.Results[1].PublishedDate)
	}
	if len(env.Answers) != 1 || env.Answers[0] != "synthesized" {
		t.Errorf("answers = %v, want [synthesized]", env.Answers)
	}
	if len(env.Suggestions) != 1 {
		t.Errorf("suggestions = %v, want one entry", env.Suggestions)
	}
}

// Clients send pageno; dropping it would silently pin every search to page 1.
func TestSearchPassesPaginationAndLimits(t *testing.T) {
	p := &stubProvider{resp: &Response{}}
	ts := newTestServer(t, &Server{Provider: p, Fetcher: &stubFetcher{name: "direct"}})

	if _, err := http.Get(ts.URL + "/search?q=x&pageno=3&limit=7&days=5"); err != nil {
		t.Fatalf("GET: %v", err)
	}
	if p.last.Page != 3 || p.last.Limit != 7 || p.last.Recency != 5 {
		t.Errorf("query = %+v, want page 3 limit 7 recency 5", p.last)
	}
}

func TestSearchMissingQueryIsBadRequest(t *testing.T) {
	ts := newTestServer(t, &Server{
		Provider: &stubProvider{resp: &Response{}},
		Fetcher:  &stubFetcher{name: "direct"},
	})
	resp, err := http.Get(ts.URL + "/search?format=json")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %s, want 400", resp.Status)
	}
}

// An upstream failure must be distinguishable from a bug in this service, or
// the operator can't tell whose status page to check.
func TestSearchUpstreamFailureIsBadGateway(t *testing.T) {
	ts := newTestServer(t, &Server{
		Provider: &stubProvider{err: errors.New("boom")},
		Fetcher:  &stubFetcher{name: "direct"},
	})
	resp, err := http.Get(ts.URL + "/search?q=x")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %s, want 502", resp.Status)
	}
}

func TestFetchUsesPrimaryWhenContentIsSubstantial(t *testing.T) {
	primary := &stubFetcher{name: "direct", doc: &Document{Title: "T", Text: strings.Repeat("word ", 200)}}
	escalate := &stubFetcher{name: "tavily", doc: &Document{Text: "escalated"}}
	ts := newTestServer(t, &Server{
		Provider: &stubProvider{resp: &Response{}},
		Fetcher:  primary,
		Escalate: escalate,
	})

	got := postFetch(t, ts.URL, "https://example.com/article")
	if got.Extractor != "direct" || got.Escalated {
		t.Errorf("extractor = %q escalated = %v, want direct/false", got.Extractor, got.Escalated)
	}
	if escalate.calls != 0 {
		t.Errorf("escalate called %d times, want 0 — paid path must not run for a good static extraction", escalate.calls)
	}
}

// A genuinely short page (small document, little text) must NOT escalate.
// Verified against the live API: example.com is ~1.2 KB yielding ~150 chars,
// and escalating it spends a credit to re-fetch text we already had in full.
func TestFetchDoesNotEscalateHonestlyShortPage(t *testing.T) {
	primary := &stubFetcher{name: "direct", doc: &Document{
		Title: "Example Domain", Text: "This domain is for use in examples.", SourceBytes: 1256,
	}}
	escalate := &stubFetcher{name: "tavily", doc: &Document{Text: "escalated"}}
	ts := newTestServer(t, &Server{
		Provider: &stubProvider{resp: &Response{}},
		Fetcher:  primary,
		Escalate: escalate,
	})

	got := postFetch(t, ts.URL, "https://example.com")
	if got.Escalated {
		t.Errorf("escalated a small honest page — that spends a paid credit for nothing: %+v", got)
	}
	if escalate.calls != 0 {
		t.Errorf("escalate called %d times, want 0", escalate.calls)
	}
}

// With no size reported, thin text is the only signal available, so it has to
// be trusted on its own.
func TestFetchEscalatesThinResultWithUnknownSize(t *testing.T) {
	primary := &stubFetcher{name: "direct", doc: &Document{Text: "Loading..."}}
	escalate := &stubFetcher{name: "tavily", doc: &Document{Text: strings.Repeat("ok ", 400)}}
	ts := newTestServer(t, &Server{
		Provider: &stubProvider{resp: &Response{}},
		Fetcher:  primary,
		Escalate: escalate,
	})
	if got := postFetch(t, ts.URL, "https://example.com/x"); !got.Escalated {
		t.Errorf("want escalation when size is unknown and text is thin: %+v", got)
	}
}

// The whole point of the tiered fetcher: an SPA ships a large document that
// yields almost no prose, and that is exactly when the paid extractor earns
// its cost.
func TestFetchEscalatesOnThinResult(t *testing.T) {
	primary := &stubFetcher{name: "direct", doc: &Document{Text: "Loading...", SourceBytes: 40 << 10}}
	escalate := &stubFetcher{name: "tavily", doc: &Document{Title: "Real", Text: strings.Repeat("rendered ", 100)}}
	ts := newTestServer(t, &Server{
		Provider: &stubProvider{resp: &Response{}},
		Fetcher:  primary,
		Escalate: escalate,
	})

	got := postFetch(t, ts.URL, "https://example.com/spa")
	if !got.Escalated || got.Extractor != "tavily" {
		t.Errorf("extractor = %q escalated = %v, want tavily/true", got.Extractor, got.Escalated)
	}
	if !strings.Contains(got.Text, "rendered") {
		t.Errorf("text = %q, want escalated content", got.Text)
	}
}

// A plain GET being blocked is the bot-detection case the paid API absorbs,
// so a hard failure has to escalate rather than surface as an error.
func TestFetchEscalatesOnPrimaryError(t *testing.T) {
	primary := &stubFetcher{name: "direct", err: errors.New("403 forbidden")}
	escalate := &stubFetcher{name: "tavily", doc: &Document{Text: strings.Repeat("ok ", 400)}}
	ts := newTestServer(t, &Server{
		Provider: &stubProvider{resp: &Response{}},
		Fetcher:  primary,
		Escalate: escalate,
	})

	got := postFetch(t, ts.URL, "https://example.com/blocked")
	if !got.Escalated || got.Extractor != "tavily" {
		t.Errorf("extractor = %q escalated = %v, want tavily/true", got.Extractor, got.Escalated)
	}
}

// When the paid tier is tried and fails, the response must say so. Silently
// returning the primary's thin text looks identical to a cheap success, hiding
// both the spend and the fact that the page was never actually read.
func TestFetchReportsFailedEscalation(t *testing.T) {
	primary := &stubFetcher{name: "direct", doc: &Document{Text: "Log in to continue", SourceBytes: 60 << 10}}
	escalate := &stubFetcher{name: "tavily", err: errors.New("extraction blocked")}
	ts := newTestServer(t, &Server{
		Provider: &stubProvider{resp: &Response{}},
		Fetcher:  primary,
		Escalate: escalate,
	})

	got := postFetch(t, ts.URL, "https://example.com/walled")
	if got.EscalationError == "" {
		t.Error("escalation failed but escalation_error is empty — the caller cannot tell this text is junk")
	}
	if got.Escalated {
		t.Error("escalated should be false when the escalation did not produce the text")
	}
	if got.Text != "Log in to continue" {
		t.Errorf("text = %q, want the primary's output preserved", got.Text)
	}
}

// The zero-cost deployment has no escalation target; a thin page must still
// return rather than fail, because a short page is a legitimate result.
func TestFetchWithoutEscalationReturnsThinResult(t *testing.T) {
	ts := newTestServer(t, &Server{
		Provider: &stubProvider{resp: &Response{}},
		Fetcher:  &stubFetcher{name: "direct", doc: &Document{Text: "short"}},
	})
	got := postFetch(t, ts.URL, "https://example.com/x")
	if got.Escalated || got.Text != "short" {
		t.Errorf("got %+v, want unescalated short text", got)
	}
}

func TestTokenGuardsSearchAndFetchButNotHealth(t *testing.T) {
	srv := &Server{
		Provider: &stubProvider{resp: &Response{}},
		Fetcher:  &stubFetcher{name: "direct", doc: &Document{Text: "x"}},
		Token:    "s3cret",
	}
	ts := newTestServer(t, srv)

	resp, err := http.Get(ts.URL + "/search?q=x")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated search = %s, want 401", resp.Status)
	}

	// Readiness probes must not need the credential.
	health, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	health.Body.Close()
	if health.StatusCode != http.StatusOK {
		t.Errorf("healthz = %s, want 200 without auth", health.Status)
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/search?q=x", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	ok, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("authenticated GET: %v", err)
	}
	ok.Body.Close()
	if ok.StatusCode != http.StatusOK {
		t.Errorf("bearer-authenticated search = %s, want 200", ok.Status)
	}

	// SearXNG clients commonly carry the secret as basic auth instead.
	basic, _ := http.NewRequest(http.MethodGet, ts.URL+"/search?q=x", nil)
	basic.SetBasicAuth("anyone", "s3cret")
	br, err := http.DefaultClient.Do(basic)
	if err != nil {
		t.Fatalf("basic-auth GET: %v", err)
	}
	br.Body.Close()
	if br.StatusCode != http.StatusOK {
		t.Errorf("basic-auth search = %s, want 200", br.Status)
	}
}

func postFetch(t *testing.T, base, target string) fetchResponse {
	t.Helper()
	body := strings.NewReader(`{"url":"` + target + `"}`)
	resp, err := http.Post(base+"/fetch", "application/json", body)
	if err != nil {
		t.Fatalf("POST /fetch: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /fetch status = %s, want 200", resp.Status)
	}
	var out fetchResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode fetch response: %v", err)
	}
	return out
}

func TestHTMLToTextStripsScriptsAndKeepsProse(t *testing.T) {
	html := `<html><head><title>Hello &amp; Goodbye</title>
	<style>body{color:red}</style></head>
	<body><script>var x = "not prose";</script>
	<h1>Heading</h1><p>First para&nbsp;here.</p><p>Second &#39;para&#39;.</p>
	<!-- a comment --></body></html>`

	text := htmlToText(html)
	// Script and style bodies are pure noise that would cost the caller real
	// context tokens.
	if strings.Contains(text, "not prose") || strings.Contains(text, "color:red") {
		t.Errorf("text retained script/style content: %q", text)
	}
	if strings.Contains(text, "a comment") {
		t.Errorf("text retained an HTML comment: %q", text)
	}
	for _, want := range []string{"Heading", "First para here.", "Second 'para'."} {
		if !strings.Contains(text, want) {
			t.Errorf("text missing %q; got %q", want, text)
		}
	}
	if got := htmlTitle(html); got != "Hello & Goodbye" {
		t.Errorf("title = %q, want entities unescaped", got)
	}
}

func TestDirectFetcherRejectsNonHTTPSchemes(t *testing.T) {
	// A URL reaches this service from an agent, so file:// would turn a page
	// fetch into local disk access on the fleet host.
	_, err := newDirectFetcher().Fetch(context.Background(), "file:///etc/passwd")
	if err == nil {
		t.Fatal("expected an error for a file:// URL")
	}
	if !strings.Contains(err.Error(), "unsupported scheme") {
		t.Errorf("err = %v, want an unsupported-scheme error", err)
	}
}

func TestDirectFetcherExtractsRealPage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><title>Doc</title></head>
			<body><nav>skip</nav><p>Body text here.</p></body></html>`))
	}))
	defer upstream.Close()

	doc, err := newDirectFetcher().Fetch(context.Background(), upstream.URL)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if doc.Title != "Doc" || !strings.Contains(doc.Text, "Body text here.") {
		t.Errorf("doc = %+v, want title Doc and body text", doc)
	}
}
