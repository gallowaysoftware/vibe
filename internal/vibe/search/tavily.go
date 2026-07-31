package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// tavilyBaseURL is Tavily's API origin. Overridable only for tests — the
// whole reason this service exists is that clients cannot override it.
const tavilyBaseURL = "https://api.tavily.com"

// tavily implements both Provider and Fetcher: /search and /extract are the
// same API with the same credential, and splitting them across two types
// would mean two clients and two keys to configure for no benefit.
type tavily struct {
	apiKey  string
	baseURL string
	hc      *http.Client
	// now is injectable so the recency->start_date mapping is testable
	// without the test depending on today's date.
	now func() time.Time
}

func newTavily(apiKey string) *tavily {
	return &tavily{
		apiKey:  apiKey,
		baseURL: tavilyBaseURL,
		now:     time.Now,
		// No global timeout here: every call is driven by the caller's
		// context, which the server sets from its own request budget. A
		// client-level timeout would silently override that and make the
		// budget two numbers instead of one.
		hc: &http.Client{},
	}
}

func (t *tavily) Name() string { return "tavily" }

type tavilySearchReq struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results,omitempty"`
	// StartDate is Tavily's documented recency filter (YYYY-MM-DD). An
	// earlier draft sent `days`, which their current API reference does not
	// list — it is a legacy news-topic parameter, and relying on an
	// undocumented field is how a recency filter silently stops applying.
	StartDate         string `json:"start_date,omitempty"`
	IncludeAnswer     bool   `json:"include_answer"`
	IncludeRawContent bool   `json:"include_raw_content"`
}

// tavilyMaxResults is the ceiling Tavily documents for max_results. Callers
// can ask for more (a client is free to request 50); clamping here turns what
// would be an upstream 400 into a slightly shorter result list.
const tavilyMaxResults = 20

type tavilySearchResp struct {
	Answer  string `json:"answer"`
	Results []struct {
		URL           string  `json:"url"`
		Title         string  `json:"title"`
		Content       string  `json:"content"`
		Score         float64 `json:"score"`
		PublishedDate string  `json:"published_date"`
	} `json:"results"`
}

func (t *tavily) Search(ctx context.Context, q Query) (*Response, error) {
	limit := q.Limit
	if limit > tavilyMaxResults {
		limit = tavilyMaxResults
	}
	body := tavilySearchReq{
		Query:      q.Text,
		MaxResults: limit,
		// The synthesized answer is cheap and some clients surface it. Raw
		// content is NOT requested: it multiplies the response size for
		// results the caller may never open, and the extract path exists for
		// exactly the page they do open.
		IncludeAnswer: true,
	}
	if q.Recency > 0 {
		body.StartDate = t.now().AddDate(0, 0, -q.Recency).Format("2006-01-02")
	}
	var out tavilySearchResp
	if err := t.post(ctx, "/search", body, &out); err != nil {
		return nil, err
	}
	resp := &Response{Answer: out.Answer, Results: make([]Result, 0, len(out.Results))}
	for _, r := range out.Results {
		resp.Results = append(resp.Results, Result{
			URL:       r.URL,
			Title:     r.Title,
			Content:   r.Content,
			Score:     r.Score,
			Published: parseTavilyDate(r.PublishedDate),
		})
	}
	return resp, nil
}

type tavilyExtractReq struct {
	URLs []string `json:"urls"`
}

type tavilyExtractResp struct {
	Results []struct {
		URL        string `json:"url"`
		RawContent string `json:"raw_content"`
	} `json:"results"`
	FailedResults []struct {
		URL   string `json:"url"`
		Error string `json:"error"`
	} `json:"failed_results"`
}

func (t *tavily) Fetch(ctx context.Context, rawURL string) (*Document, error) {
	var out tavilyExtractResp
	if err := t.post(ctx, "/extract", tavilyExtractReq{URLs: []string{rawURL}}, &out); err != nil {
		return nil, err
	}
	if len(out.Results) == 0 {
		// Tavily reports per-URL failures in a separate array with a 200
		// status, so an empty result set is the normal shape of "this page
		// could not be read" and has to be surfaced as an error here.
		if len(out.FailedResults) > 0 {
			return nil, fmt.Errorf("tavily extract %s: %s", rawURL, out.FailedResults[0].Error)
		}
		return nil, fmt.Errorf("tavily extract %s: no content returned", rawURL)
	}
	return &Document{URL: out.Results[0].URL, Text: out.Results[0].RawContent}, nil
}

func (t *tavily) post(ctx context.Context, path string, in, out any) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("tavily: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("tavily: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+t.apiKey)
	resp, err := t.hc.Do(req)
	if err != nil {
		return fmt.Errorf("tavily %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("tavily %s: %w (status %s)", path, ErrUnauthorized, resp.Status)
	}
	if resp.StatusCode != http.StatusOK {
		// Cap the echoed body: an upstream error page can be arbitrarily
		// large and this string ends up in logs.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("tavily %s: status %s: %s", path, resp.Status, bytes.TrimSpace(snippet))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("tavily %s: decode response: %w", path, err)
	}
	return nil
}

// parseTavilyDate accepts the formats Tavily has been observed to emit and
// returns the zero time for anything else. A misparsed date would render as a
// confidently wrong "3y ago" in the client, so unknown formats are dropped
// rather than guessed at.
func parseTavilyDate(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02",
		"Mon, 02 Jan 2006 15:04:05 GMT",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
