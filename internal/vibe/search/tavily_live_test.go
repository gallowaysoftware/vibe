package search

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// Live verification against the real Tavily API. Skipped by default: it costs
// credits and needs a credential, so it must never run as part of an ordinary
// `go test ./...`.
//
// Run it with the key supplied through the environment, never on the command
// line (a --flag would put the secret in the process list for every user on
// the host):
//
//	TAVILY_LIVE=1 TAVILY_API_KEY="$(...)" \
//	  go test ./internal/vibe/search/ -run TestLive -v
//
// Nothing here prints the key or any part of it. Failures name the operation
// and the upstream's own message.
func liveTavily(t *testing.T) *tavily {
	t.Helper()
	if os.Getenv("TAVILY_LIVE") != "1" {
		t.Skip("set TAVILY_LIVE=1 to run live Tavily checks (consumes API credits)")
	}
	key := os.Getenv("TAVILY_API_KEY")
	if key == "" {
		t.Fatal("TAVILY_LIVE=1 but TAVILY_API_KEY is empty")
	}
	return newTavily(key)
}

func TestLiveTavilySearch(t *testing.T) {
	tv := liveTavily(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := tv.Search(ctx, Query{Text: "llama.cpp speculative decoding", Limit: 5})
	if err != nil {
		t.Fatalf("live search failed: %v", err)
	}
	if len(resp.Results) == 0 {
		t.Fatal("live search returned zero results — the request shape is probably wrong")
	}
	// The three fields the SearXNG envelope requires. If Tavily renamed any of
	// them, every client would render blank rows rather than error.
	for i, r := range resp.Results {
		if r.URL == "" || r.Title == "" {
			t.Errorf("result %d missing url/title: %+v", i, r)
		}
		if r.Content == "" {
			t.Errorf("result %d has no snippet — content mapping is wrong", i)
		}
	}
	t.Logf("live search: %d results, answer=%t, first=%q",
		len(resp.Results), resp.Answer != "", resp.Results[0].Title)
}

// Recency uses start_date, which the current API reference documents. If
// Tavily rejects it, this is where that surfaces rather than in production.
func TestLiveTavilySearchWithRecency(t *testing.T) {
	tv := liveTavily(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := tv.Search(ctx, Query{Text: "AI news", Limit: 5, Recency: 7})
	if err != nil {
		t.Fatalf("live search with start_date failed: %v", err)
	}
	t.Logf("live recency search: %d results", len(resp.Results))
}

func TestLiveTavilyExtract(t *testing.T) {
	tv := liveTavily(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	doc, err := tv.Fetch(ctx, "https://example.com")
	if err != nil {
		t.Fatalf("live extract failed: %v", err)
	}
	if strings.TrimSpace(doc.Text) == "" {
		t.Fatal("live extract returned empty raw_content")
	}
	t.Logf("live extract: %d chars from %s", len(doc.Text), doc.URL)
}

// The escalation path end to end against the real API: direct extraction
// first, Tavily only when the page comes back as an unrendered shell.
//
// Override the target with TAVILY_LIVE_SPA_URL — which page is client-rendered
// changes over time, so this reports what happened rather than asserting that
// escalation must trigger for a specific site.
func TestLiveTieredFetchEscalation(t *testing.T) {
	tv := liveTavily(t)
	target := os.Getenv("TAVILY_LIVE_SPA_URL")
	if target == "" {
		target = "https://example.com"
	}
	srv := &Server{
		Provider: &stubProvider{resp: &Response{}},
		Fetcher:  newDirectFetcher(),
		Escalate: tv,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	doc, meta, err := srv.fetchTiered(ctx, target)
	if err != nil {
		t.Fatalf("tiered fetch of %s failed: %v", target, err)
	}
	if strings.TrimSpace(doc.Text) == "" {
		t.Fatalf("tiered fetch of %s produced no text", target)
	}
	t.Logf("tiered fetch %s: extractor=%s escalated=%t chars=%d escalation_err=%q",
		target, meta.Extractor, meta.Escalated, len(doc.Text), meta.EscalationErr)
}
