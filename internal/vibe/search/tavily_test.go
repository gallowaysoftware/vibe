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

// These tests pin the wire shape against Tavily's published API reference.
// They cannot prove the live service agrees — only a real call does that —
// but they catch the failure mode that a live test makes expensive to
// diagnose: sending a field name the API does not recognise and silently
// having the filter ignored.

func newTavilyAgainst(t *testing.T, handler http.HandlerFunc) (*tavily, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	tv := newTavily("test-key")
	tv.baseURL = ts.URL
	tv.now = func() time.Time { return time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC) }
	return tv, ts
}

func TestTavilySearchRequestMatchesDocumentedSchema(t *testing.T) {
	var got map[string]any
	var authHeader string
	tv, _ := newTavilyAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		if r.URL.Path != "/search" {
			t.Errorf("path = %q, want /search", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"results":[]}`))
	})

	_, err := tv.Search(context.Background(), Query{Text: "hello", Limit: 5, Recency: 7})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if authHeader != "Bearer test-key" {
		t.Errorf("Authorization = %q, want Bearer form", authHeader)
	}
	if got["query"] != "hello" {
		t.Errorf("query = %v", got["query"])
	}
	if got["max_results"] != float64(5) {
		t.Errorf("max_results = %v, want 5", got["max_results"])
	}
	// start_date is the documented recency filter; `days` is a legacy field
	// absent from the current reference, and sending it would mean the filter
	// silently does nothing.
	if got["start_date"] != "2026-07-24" {
		t.Errorf("start_date = %v, want 2026-07-24 (7 days before the fixed now)", got["start_date"])
	}
	if _, sentDays := got["days"]; sentDays {
		t.Errorf("request still sends the undocumented `days` field: %v", got)
	}
}

// max_results is documented as 0-20; a client asking for more must not turn
// into an upstream 400.
func TestTavilySearchClampsMaxResults(t *testing.T) {
	var got map[string]any
	tv, _ := newTavilyAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"results":[]}`))
	})
	if _, err := tv.Search(context.Background(), Query{Text: "x", Limit: 100}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got["max_results"] != float64(tavilyMaxResults) {
		t.Errorf("max_results = %v, want clamped to %d", got["max_results"], tavilyMaxResults)
	}
}

// Recency 0 means "no bound" and must not send an empty start_date, which
// would be a schema violation rather than an absent filter.
func TestTavilySearchOmitsStartDateWithoutRecency(t *testing.T) {
	var got map[string]any
	tv, _ := newTavilyAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"results":[]}`))
	})
	if _, err := tv.Search(context.Background(), Query{Text: "x"}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if _, ok := got["start_date"]; ok {
		t.Errorf("start_date present without recency: %v", got)
	}
}

func TestTavilySearchParsesDocumentedResponse(t *testing.T) {
	tv, _ := newTavilyAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"query": "x",
			"answer": "the answer",
			"results": [
				{"title":"T1","url":"https://a.example","content":"snippet one","score":0.9,
				 "published_date":"2026-07-30T00:00:00"},
				{"title":"T2","url":"https://b.example","content":"snippet two","score":0.5}
			],
			"response_time": 1.2
		}`))
	})
	resp, err := tv.Search(context.Background(), Query{Text: "x"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if resp.Answer != "the answer" {
		t.Errorf("answer = %q", resp.Answer)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(resp.Results))
	}
	if resp.Results[0].Title != "T1" || resp.Results[0].URL != "https://a.example" ||
		resp.Results[0].Content != "snippet one" || resp.Results[0].Score != 0.9 {
		t.Errorf("first result = %+v", resp.Results[0])
	}
	// published_date is not in the current published reference but is emitted
	// for some topics; parse it when present and leave the zero time when not.
	if resp.Results[0].Published.IsZero() {
		t.Errorf("published_date was not parsed: %+v", resp.Results[0])
	}
	if !resp.Results[1].Published.IsZero() {
		t.Errorf("absent published_date should stay zero, got %v", resp.Results[1].Published)
	}
}

func TestTavilyExtractRequestAndResponse(t *testing.T) {
	var got map[string]any
	tv, _ := newTavilyAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/extract" {
			t.Errorf("path = %q, want /extract", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"results":[{"url":"https://a.example","raw_content":"# Page\n\nbody"}],
			"failed_results":[]}`))
	})

	doc, err := tv.Fetch(context.Background(), "https://a.example")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	// urls is documented as a string OR an array; the array form is
	// unambiguous and is what a batch-capable caller would grow into.
	urls, ok := got["urls"].([]any)
	if !ok || len(urls) != 1 || urls[0] != "https://a.example" {
		t.Errorf("urls = %v, want a one-element array", got["urls"])
	}
	if doc.Text != "# Page\n\nbody" {
		t.Errorf("text = %q, want raw_content", doc.Text)
	}
}

// Tavily reports per-URL failures inside a 200 response, so an empty results
// array is the normal shape of "this page could not be read" and must surface
// as an error rather than an empty document.
func TestTavilyExtractSurfacesFailedResults(t *testing.T) {
	tv, _ := newTavilyAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[],"failed_results":[
			{"url":"https://a.example","error":"page timed out"}]}`))
	})
	_, err := tv.Fetch(context.Background(), "https://a.example")
	if err == nil {
		t.Fatal("expected an error when extraction failed")
	}
	if !strings.Contains(err.Error(), "page timed out") {
		t.Errorf("err = %v, want the upstream reason", err)
	}
}

// A bad key is the most likely operational failure; it must be distinguishable
// from a network blip in the logs.
func TestTavilyUnauthorizedIsTyped(t *testing.T) {
	tv, _ := newTavilyAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	_, err := tv.Search(context.Background(), Query{Text: "x"})
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
}
