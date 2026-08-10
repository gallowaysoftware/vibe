package vamp

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"text/template"
	"unicode/utf8"
)

// TestParseSearXNGTemplate_MissingResultsKeyIsAnError separates the two
// shapes of "nothing". A search that found nothing carries a results
// key (empty or null) and is a real answer; a body with no results key
// at all is not a SearXNG response — it is an error page, a proxy body,
// or an LLM echo — and must not be folded into "zero sources found",
// because the research stage downstream reports success on it.
func TestParseSearXNGTemplate_MissingResultsKeyIsAnError(t *testing.T) {
	_, err := parseSearXNGTemplate(`{"error":"engine unavailable","query":"x"}`)
	if err == nil {
		t.Fatal("expected error for a response with no results key, got nil (silent zero sources)")
	}
	if !strings.Contains(err.Error(), "results") {
		t.Errorf("error should name the missing field, got %v", err)
	}
	// The genuinely-empty shapes must still succeed: null and [].
	for _, in := range []string{`{"query":"x","results":null}`, `{"query":"x","results":[]}`} {
		got, err := parseSearXNGTemplate(in)
		if err != nil {
			t.Fatalf("empty-but-well-formed %s: unexpected error %v", in, err)
		}
		if got != `{"items":[]}` {
			t.Errorf("%s: got %s, want empty items", in, got)
		}
	}
}

// TestParseSearXNGTemplate_EmptyInputIsAnError pins that an upstream
// webhook stage which wrote nothing fails the render rather than
// producing an empty-but-successful source list.
func TestParseSearXNGTemplate_EmptyInputIsAnError(t *testing.T) {
	for _, in := range []string{"", "   \n\n  ", "```json\n```"} {
		if _, err := parseSearXNGTemplate(in); err == nil {
			t.Errorf("parseSearXNG(%q) = nil error; want a failure, an empty upstream is not an empty search", in)
		}
	}
}

// TestParseWikipediaSearchTemplate_APIErrorIsNotZeroHits pins that a
// MediaWiki {"error":{...}} body fails the stage. MediaWiki's zero-hit
// shape always carries query.search, so an absent query object is a
// refused request, never an empty result — and the operator needs the
// API's own code/info in the message to act on it.
func TestParseWikipediaSearchTemplate_APIErrorIsNotZeroHits(t *testing.T) {
	in := `{"error":{"code":"invalidparammix","info":"The parameters srsearch, srlimit cannot be used together."},"servedby":"mw-api-ext"}`
	_, err := parseWikipediaSearchTemplate(in)
	if err == nil {
		t.Fatal("expected error for a MediaWiki error body, got nil (silent zero hits)")
	}
	if !strings.Contains(err.Error(), "invalidparammix") {
		t.Errorf("error should carry the MediaWiki code, got %v", err)
	}
	if !strings.Contains(err.Error(), "cannot be used together") {
		t.Errorf("error should carry the MediaWiki info, got %v", err)
	}
	// A body that is JSON but not a MediaWiki response at all also fails,
	// with no error clause to quote.
	if _, err := parseWikipediaSearchTemplate(`{"nothing":"here"}`); err == nil {
		t.Error("expected error for a non-MediaWiki JSON body")
	}
}

// TestParseWikipediaExtractTemplate_APIErrorIsNotZeroPages is the
// sibling of the search-side check: the same guard, in the other
// parser, because one of two is the defect class this package repeats.
func TestParseWikipediaExtractTemplate_APIErrorIsNotZeroPages(t *testing.T) {
	in := `{"error":{"code":"invalidtitle","info":"Bad title \"|\"."}}`
	_, err := parseWikipediaExtractTemplate(in)
	if err == nil {
		t.Fatal("expected error for a MediaWiki error body, got nil (silent zero pages)")
	}
	if !strings.Contains(err.Error(), "invalidtitle") {
		t.Errorf("error should carry the MediaWiki code, got %v", err)
	}
	// query present but pages absent is equally wrong-shaped.
	if _, err := parseWikipediaExtractTemplate(`{"query":{"normalized":[]}}`); err == nil {
		t.Error("expected error when query.pages is absent")
	}
}

// TestFilterByFieldTemplate pins the kept/dropped semantics of
// the generic field filter: bool true keeps, bool false drops,
// missing key drops, non-empty string keeps, empty string drops,
// non-zero number keeps, zero drops, items aren't echoed
// reordered.
func TestFilterByFieldTemplate(t *testing.T) {
	cases := []struct {
		name  string
		field string
		in    string
		want  string
	}{
		{
			name:  "keep bool true, drop bool false",
			field: "keep",
			in:    `[{"id":"a","keep":true},{"id":"b","keep":false},{"id":"c","keep":true}]`,
			want:  `{"items":[{"id":"a","keep":true},{"id":"c","keep":true}]}`,
		},
		{
			name:  "missing field drops",
			field: "keep",
			in:    `[{"id":"a","keep":true},{"id":"b"}]`,
			want:  `{"items":[{"id":"a","keep":true}]}`,
		},
		{
			name:  "wrapped input",
			field: "keep",
			in:    `{"items":[{"x":1,"keep":true},{"x":2,"keep":false}]}`,
			want:  `{"items":[{"keep":true,"x":1}]}`,
		},
		{
			name:  "non-empty string truthy",
			field: "name",
			in:    `[{"name":"hi"},{"name":""}]`,
			want:  `{"items":[{"name":"hi"}]}`,
		},
		{
			name:  "non-zero number truthy",
			field: "score",
			in:    `[{"score":1},{"score":0}]`,
			want:  `{"items":[{"score":1}]}`,
		},
		{
			name:  "empty input",
			field: "keep",
			in:    "",
			want:  `{"items":[]}`,
		},
		{
			name:  "fenced JSON tolerated",
			field: "keep",
			in:    "```json\n[{\"keep\":true,\"id\":\"x\"}]\n```",
			want:  `{"items":[{"id":"x","keep":true}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := filterByFieldTemplate(tc.field, tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("\n got:  %s\n want: %s", got, tc.want)
			}
		})
	}
}

// TestFilterByFieldTemplate_EmptyField rejects an empty field name
// at run time so a typo'd `{{ filterByField "" .x }}` fails loudly
// rather than dropping everything silently.
func TestFilterByFieldTemplate_EmptyField(t *testing.T) {
	_, err := filterByFieldTemplate("", `[{"keep":true}]`)
	if err == nil {
		t.Fatal("expected error on empty field")
	}
}

// TestParseSearXNGTemplate pins the SearXNG response → flat item
// shape transform: each /search?format=json response's results[]
// gets flattened, each result gets a sha256-derived id, url is
// required, content becomes snippet.
func TestParseSearXNGTemplate(t *testing.T) {
	in := `{"query":"x","number_of_results":2,"results":[
		{"title":"A","url":"https://a.example/","content":"snippet a"},
		{"title":"B","url":"https://b.example/","content":"snippet b"}
	]}`
	got, err := parseSearXNGTemplate(in)
	if err != nil {
		t.Fatal(err)
	}
	// Items array with two entries, source_type "web", ids stable
	// across runs (same url → same id).
	if !strings.Contains(got, `"source_type":"web"`) {
		t.Errorf("missing source_type=web in %s", got)
	}
	if !strings.Contains(got, `"title":"A"`) || !strings.Contains(got, `"title":"B"`) {
		t.Errorf("missing titles in %s", got)
	}
	if !strings.Contains(got, `"snippet":"snippet a"`) {
		t.Errorf("missing snippet in %s", got)
	}
}

// TestParseSearXNGTemplate_MultipleResponses verifies that a
// foreach-style concatenated stream of SearXNG responses produces
// a single flat items[] array spanning all responses.
func TestParseSearXNGTemplate_MultipleResponses(t *testing.T) {
	in := `{"results":[{"title":"A","url":"https://a/","content":"a"}]}` +
		"\n\n" +
		`{"results":[{"title":"B","url":"https://b/","content":"b"}]}`
	got, err := parseSearXNGTemplate(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(got, `"title"`) != 2 {
		t.Errorf("want 2 titles, got %s", got)
	}
}

// TestParseSearXNGTemplate_NoResults handles the empty-search case
// (SearXNG returns valid JSON with results: null/missing).
func TestParseSearXNGTemplate_NoResults(t *testing.T) {
	got, err := parseSearXNGTemplate(`{"query":"x","results":null}`)
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"items":[]}` {
		t.Errorf("want empty items, got %s", got)
	}
}

// TestParseWikipediaExtractTemplate decodes the MediaWiki
// titles=query format including the page-not-found (-1 pageid) case.
func TestParseWikipediaExtractTemplate(t *testing.T) {
	in := `{"query":{"pages":{"12345":{
		"pageid":12345,
		"title":"Transformer",
		"fullurl":"https://en.wikipedia.org/wiki/Transformer",
		"extract":"A transformer is a deep learning model..."
	}}}}`
	got, err := parseWikipediaExtractTemplate(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `"source_type":"wikipedia"`) {
		t.Errorf("missing source_type in %s", got)
	}
	if !strings.Contains(got, `"title":"Transformer"`) {
		t.Errorf("missing title in %s", got)
	}
}

// TestParseWikipediaExtractTemplate_NotFound pins the two independent skips
// a nonexistent page has to clear.
//
// The wire fixture alone cannot do it. MediaWiki's missing-page entry has no
// `extract`, so the `pageid < 0` rung and the `extract == ""` rung BOTH fire
// on it and either one is deletable with this test still green — the test
// was named for the pageid rung and was in fact only ever exercising
// whichever ran first. The second fixture is the same page WITH an extract:
// nothing but the pageid check can reject it, so that rung is now pinned on
// its own. Relaxing `extract == ""` is a plausible future edit (exintro
// legitimately returns empty leads) and it must not silently promote a
// nonexistent page to a source titled "DoesNotExist".
func TestParseWikipediaExtractTemplate_NotFound(t *testing.T) {
	cases := map[string]string{
		"wire shape, no extract":  `{"query":{"pages":{"-1":{"pageid":-1,"title":"DoesNotExist","missing":""}}}}`,
		"pageid rung, on its own": `{"query":{"pages":{"-1":{"pageid":-1,"title":"DoesNotExist","fullurl":"https://en.wikipedia.org/wiki/DoesNotExist","extract":"x"}}}}`,
		"empty-extract rung, on its own": `{"query":{"pages":{"7":{"pageid":7,"title":"Stub",` +
			`"fullurl":"https://en.wikipedia.org/wiki/Stub","extract":""}}}}`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := parseWikipediaExtractTemplate(in)
			if err != nil {
				t.Fatal(err)
			}
			if got != `{"items":[]}` {
				t.Errorf("want empty items for missing page, got %s", got)
			}
		})
	}
}

// TestParseWikipediaExtractTemplate_MissingFullurlIsAnError pins the id
// collision. `fullurl` only appears when the query carries inprop=url, which
// this function's own doc comment used to omit — so a pipeline written from
// the comment hashed the empty string for every page and every source got
// the id e3b0c44298fc. The id's stated purpose is stable per-source
// FILENAMES, so three sources wrote to one file, last write won, and two
// sources vanished with no error anywhere. The siblings refuse the identical
// shape (parseSearXNG and parseArxiv both skip an item with no url); this
// one is louder still because the missing parameter is a property of the
// query the author wrote, so the error is deterministic and one-line
// actionable rather than a silent zero.
func TestParseWikipediaExtractTemplate_MissingFullurlIsAnError(t *testing.T) {
	in := `{"query":{"pages":{
		"1":{"pageid":1,"title":"Alpha","extract":"a"},
		"2":{"pageid":2,"title":"Beta","extract":"b"},
		"3":{"pageid":3,"title":"Gamma","extract":"c"}}}}`
	got, err := parseWikipediaExtractTemplate(in)
	if err == nil {
		t.Fatalf("expected an error for pages with no fullurl, got %s", got)
	}
	if !strings.Contains(err.Error(), "inprop=url") {
		t.Errorf("error must name the missing query parameter, got %v", err)
	}
	// The e3b0c44298fc collision must be impossible to reach.
	if strings.Contains(got, "e3b0c44298fc") {
		t.Errorf("sha256(\"\") id leaked into output: %s", got)
	}
	// With inprop=url present, the same pages parse and every id differs.
	ok := `{"query":{"pages":{
		"1":{"pageid":1,"title":"Alpha","fullurl":"https://en.wikipedia.org/wiki/Alpha","extract":"a"},
		"2":{"pageid":2,"title":"Beta","fullurl":"https://en.wikipedia.org/wiki/Beta","extract":"b"}}}}`
	got, err = parseWikipediaExtractTemplate(ok)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(got, `"id":`) != 2 {
		t.Fatalf("want 2 items, got %s", got)
	}
	var parsed struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Items[0].ID == parsed.Items[1].ID {
		t.Errorf("two pages share one id %q — the filename collision is back", parsed.Items[0].ID)
	}
}

// TestParseWikipediaExtractTemplate_MultiPageOrderIsDeterministic pins the
// sort. Go randomises map iteration, so a titles=A|B|C query (legal, up to
// 50 titles) rendered a different prompt every run — measured as 5 distinct
// outputs over 200 parses of one 5-page response. Nothing keyed on the
// rendered prompt could cache, and nothing was reproducible. readFiles sorts
// three hundred lines down for exactly this reason.
func TestParseWikipediaExtractTemplate_MultiPageOrderIsDeterministic(t *testing.T) {
	in := `{"query":{"pages":{
		"1":{"pageid":1,"title":"Alpha","fullurl":"https://en.wikipedia.org/wiki/Alpha","extract":"a"},
		"2":{"pageid":2,"title":"Beta","fullurl":"https://en.wikipedia.org/wiki/Beta","extract":"b"},
		"3":{"pageid":3,"title":"Gamma","fullurl":"https://en.wikipedia.org/wiki/Gamma","extract":"c"},
		"4":{"pageid":4,"title":"Delta","fullurl":"https://en.wikipedia.org/wiki/Delta","extract":"d"},
		"5":{"pageid":5,"title":"Epsilon","fullurl":"https://en.wikipedia.org/wiki/Epsilon","extract":"e"}}}}`
	first, err := parseWikipediaExtractTemplate(in)
	if err != nil {
		t.Fatal(err)
	}
	// 200 parses, one output. Map-order randomisation is per-iteration, so
	// a handful of runs is not evidence; the review measured 5 distinct
	// orderings at this count.
	for i := 0; i < 200; i++ {
		got, err := parseWikipediaExtractTemplate(in)
		if err != nil {
			t.Fatal(err)
		}
		if got != first {
			t.Fatalf("multi-page output is not deterministic:\n run 0: %s\n run %d: %s", first, i, got)
		}
	}
	// All five pages come back — the doc comment used to claim only the
	// first did, which was never true of the code.
	if n := strings.Count(first, `"source_type"`); n != 5 {
		t.Errorf("want all 5 pages, got %d in %s", n, first)
	}
}

// TestParseArxivTemplate_APIErrorFeedIsNotASource pins the guard both
// sibling parsers already had. arXiv has no error channel: a rejected
// request is HTTP 200 carrying a valid one-entry Atom feed whose id is an
// arxiv.org/api/errors# URL and whose title is "Error". Parsed literally,
// a mistyped id_list produced a "paper" called Error whose abstract was the
// API's complaint, and the research stage cited it in a report.
func TestParseArxivTemplate_APIErrorFeedIsNotASource(t *testing.T) {
	in := `<feed xmlns="http://www.w3.org/2005/Atom">
  <entry>
    <id>http://arxiv.org/api/errors#incorrect_id_format_for_1234</id>
    <title>Error</title>
    <summary>incorrect id format for 1234</summary>
  </entry>
</feed>`
	got, err := parseArxivTemplate(in)
	if err == nil {
		t.Fatalf("expected an error for an arXiv API-error feed, got %s", got)
	}
	if !strings.Contains(err.Error(), "incorrect id format") {
		t.Errorf("error must carry the API's own complaint, got %v", err)
	}
	if strings.Contains(got, `"title":"Error"`) {
		t.Errorf("the API error became a source: %s", got)
	}
	// Case-insensitive: the scheme and host of an <id> are.
	if _, err := parseArxivTemplate(`<feed><entry><id>HTTP://ARXIV.ORG/API/ERRORS#bad</id><summary>nope</summary></entry></feed>`); err == nil {
		t.Error("uppercase error id slipped through")
	}
}

// TestParseArxivTemplate_UnreadableEntriesAreNotZeroPapers is the other half
// of finding 1: entries the parser cannot read were dropped one by one, so
// an arXiv namespace or schema change turned N papers into zero sources on a
// green run — the outcome parseSearXNG's nine-line comment exists to prevent,
// one function away.
func TestParseArxivTemplate_UnreadableEntriesAreNotZeroPapers(t *testing.T) {
	in := `<feed xmlns="http://www.w3.org/2005/Atom">
  <entry><title>A paper</title><summary>abstract</summary></entry>
  <entry><title>Another</title><summary>abstract</summary></entry>
</feed>`
	got, err := parseArxivTemplate(in)
	if err == nil {
		t.Fatalf("expected an error when no entry had an <id>, got %s", got)
	}
	if !strings.Contains(err.Error(), "2") {
		t.Errorf("error should say how many entries were dropped, got %v", err)
	}
	// A feed with genuinely zero entries is still a real answer.
	if got, err := parseArxivTemplate(`<feed xmlns="http://www.w3.org/2005/Atom"></feed>`); err != nil || got != `{"items":[]}` {
		t.Errorf("an empty feed is a real zero-hit answer, got %s err=%v", got, err)
	}
}

// TestParseSearXNGTemplate_UnreadableResultsAreNotZeroHits extends the
// empty-vs-missing distinction from the missing KEY to a key that is present
// and unreadable. `{"number_of_results":12,"results":[…renamed url field…]}`
// used to yield {"items":[]} and no error, with the disconfirming evidence
// sitting in the same map. So did a `results` holding an object or a scalar,
// although the comment above that branch said "null or a scalar".
func TestParseSearXNGTemplate_UnreadableResultsAreNotZeroHits(t *testing.T) {
	bad := map[string]string{
		"schema drift: url renamed": `{"number_of_results":12,"results":[{"link":"https://a/","title":"A"},{"link":"https://b/","title":"B"}]}`,
		"url is a number":           `{"results":[{"url":42,"title":"A"}]}`,
		"results is an object":      `{"results":{"0":{"url":"https://a/"}}}`,
		"results is a string":       `{"results":"none"}`,
		"results is a number":       `{"results":0}`,
	}
	for name, in := range bad {
		t.Run(name, func(t *testing.T) {
			got, err := parseSearXNGTemplate(in)
			if err == nil {
				t.Fatalf("expected an error, got %s (12 unreadable results read as a clean zero)", got)
			}
		})
	}
	// null and [] remain real answers, and a partially-readable response is
	// still a success — only "all of them failed" is drift.
	good := map[string]string{
		"null":    `{"results":null}`,
		"empty":   `{"results":[]}`,
		"partial": `{"results":[{"url":"https://a/","title":"A"},{"title":"no url here"}]}`,
	}
	for name, in := range good {
		t.Run(name, func(t *testing.T) {
			if _, err := parseSearXNGTemplate(in); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestParseWikipediaSearchTemplate_TitlesArePathEscaped pins the escaping.
// The title is upstream text going into a URL PATH; concatenated, "C#
// (programming language)" produced a URL whose "#" opens a fragment that is
// never sent to the server, so the citation resolved to the article "C" —
// silently, with a self-consistently wrong id hashed over the broken URL.
func TestParseWikipediaSearchTemplate_TitlesArePathEscaped(t *testing.T) {
	in := `{"query":{"search":[
		{"title":"C# (programming language)","snippet":"x"},
		{"title":"Who's Afraid of Virginia Woolf?","snippet":"y"},
		{"title":"Wikipedia:Sandbox/Test","snippet":"z"}
	]}}`
	got, err := parseWikipediaSearchTemplate(in)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Items []struct {
			Title string `json:"title"`
			URL   string `json:"url"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"https://en.wikipedia.org/wiki/C%23_%28programming_language%29",
		"https://en.wikipedia.org/wiki/Who%27s_Afraid_of_Virginia_Woolf%3F",
		// A subpage "/" and a namespace ":" are legal in a path and must
		// SURVIVE — which is why this is url.URL and not url.PathEscape.
		"https://en.wikipedia.org/wiki/Wikipedia:Sandbox/Test",
	}
	for i, w := range want {
		if parsed.Items[i].URL != w {
			t.Errorf("%q:\n got:  %s\n want: %s", parsed.Items[i].Title, parsed.Items[i].URL, w)
		}
	}
	for _, it := range parsed.Items {
		if u, err := url.Parse(it.URL); err != nil {
			t.Errorf("%q produced an unparseable URL: %v", it.Title, err)
		} else if u.Fragment != "" || u.RawQuery != "" {
			t.Errorf("%q leaked into a fragment/query: fragment=%q query=%q", it.Title, u.Fragment, u.RawQuery)
		}
	}
}

// TestParseWikipediaSearchTemplate_SnippetKeepsProseAndDecodesEntities pins
// both halves of the half-done sanitiser. The old `<[^>]*>` read the prose
// between two comparison operators as a tag: "0 < n and n > 5" came back as
// "0  5", so the snippet handed to the model asserted something the source
// did not say. And entities were never decoded, so the model read "&deg;"
// and "&amp;" literally.
func TestParseWikipediaSearchTemplate_SnippetKeepsProseAndDecodesEntities(t *testing.T) {
	in := `{"query":{"search":[{"title":"T","snippet":` +
		`"for all <span class=\"searchmatch\">n</span> where 0 < n and n > 5 the bound holds at 5 &deg;C &amp; up"}]}}`
	got, err := parseWikipediaSearchTemplate(in)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Items []struct {
			Snippet string `json:"snippet"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatal(err)
	}
	want := "for all n where 0 < n and n > 5 the bound holds at 5 °C & up"
	if parsed.Items[0].Snippet != want {
		t.Errorf("\n got:  %q\n want: %q", parsed.Items[0].Snippet, want)
	}
	// The searchmatch wrapper still goes, entities and all.
	if strings.Contains(parsed.Items[0].Snippet, "<span") || strings.Contains(parsed.Items[0].Snippet, "&amp;") {
		t.Errorf("tags or raw entities survived: %q", parsed.Items[0].Snippet)
	}
	// Decoding happens AFTER stripping, so a `&lt;script&gt;` the source
	// wrote survives as text instead of becoming a tag the stripper eats.
	got, err = parseWikipediaSearchTemplate(`{"query":{"search":[{"title":"T","snippet":"write &lt;b&gt; to embolden"}]}}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `write \u003cb\u003e to embolden`) {
		t.Errorf("entity-encoded markup was eaten by the tag stripper: %s", got)
	}
}

// TestTruncateTemplate covers a helper that had ZERO statement coverage
// while being the one guard between an oversized document and the context
// window.
func TestTruncateTemplate(t *testing.T) {
	// The rune-boundary walk: a 4-byte emoji is never sliced in half, at
	// any offset through it.
	const s = "aaa😀bbb"
	for n := 1; n <= len(s); n++ {
		got, err := truncateTemplate(n, s)
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if !utf8.ValidString(got) {
			t.Errorf("n=%d produced invalid UTF-8: %q", n, got)
		}
	}
	// Under the cap, the input is returned untouched and unmarked.
	if got, err := truncateTemplate(100, "short"); err != nil || got != "short" {
		t.Errorf("got %q err=%v, want the input unchanged", got, err)
	}
	// Over the cap, the marker is appended so the model can see the cut.
	got, err := truncateTemplate(10, strings.Repeat("x", 100))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "content truncated") {
		t.Errorf("no elision marker in %q", got)
	}
	if !strings.HasPrefix(got, strings.Repeat("x", 10)) {
		t.Errorf("content was not clipped to n bytes: %q", got)
	}
	// An input that is all continuation bytes walks cut to 0 without
	// panicking, and yields only the marker.
	if _, err := truncateTemplate(3, "\x80\x80\x80\x80\x80"); err != nil {
		t.Fatal(err)
	}
}

// TestTruncateTemplate_NonPositiveLimitIsAnError pins the failure direction.
// `n <= 0` used to mean "no cap", so `truncate 0` — a typo, or a
// remaining-budget expression that went non-positive — passed a 100KB
// document straight through and reported success. A guard that disarms
// itself on a typo is the shape this package keeps paying for; splitSentences
// answers the same question with a default rather than a bypass.
func TestTruncateTemplate_NonPositiveLimitIsAnError(t *testing.T) {
	big := strings.Repeat("x", 100000)
	for _, n := range []int{0, -1, -100000} {
		got, err := truncateTemplate(n, big)
		if err == nil {
			t.Errorf("truncate(%d, 100KB) = %d bytes, nil error; want a failure, a non-positive cap is not 'no cap'", n, len(got))
		}
		if got != "" {
			t.Errorf("truncate(%d, ...) returned %d bytes alongside its error", n, len(got))
		}
	}
	// And it aborts the render rather than passing the document through:
	// a template-func error stops the stage, which is the whole reason
	// "returns an error" is a stronger outcome than "returns the input".
	tmpl, err := template.New("t").Funcs(templateFuncs()).Parse(`{{ truncate 0 .doc }}`)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := tmpl.Execute(&out, map[string]any{"doc": big}); err == nil {
		t.Errorf("a template using `truncate 0` rendered successfully, emitting %d bytes", out.Len())
	}
}

// TestStripDataURIsTemplate covers a helper that had ZERO test coverage and
// whose whole purpose is a size bound. Deleting its body outright used to
// leave the package green.
func TestStripDataURIsTemplate(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "lowercase scheme, the case that already worked",
			in:   "a ![alt](data:image/png;base64,iVBORw0K) b",
			want: "a [alt] b",
		},
		{
			// RFC 3986 schemes are case-insensitive and browsers accept
			// DATA:. The old regex did not, giving a 0.0% reduction on the
			// 30-reference document the doc comment describes.
			name: "uppercase scheme",
			in:   "a ![alt](DATA:image/svg+xml;base64,PHN2Zz48L3N2Zz4=) b",
			want: "a [alt] b",
		},
		{
			name: "mixed case scheme",
			in:   "a ![alt](Data:image/png;base64,iVBORw0K) b",
			want: "a [alt] b",
		},
		{
			// The corrupting case. `[^)]*` stopped at the rgb( paren, so
			// the payload survived as prose next to broken markup — and
			// something HAD been replaced, so the helper looked like it
			// worked.
			name: "paren inside an un-encoded SVG body",
			in:   `![alt](data:image/svg+xml,<svg><path fill="rgb(1,2,3)" d="M0 0"/></svg>) trailing prose`,
			want: "[alt] trailing prose",
		},
		{
			name: "nested parens",
			in:   `![f](data:image/svg+xml,<g transform="translate(matrix(1,0))"/>) end`,
			want: "[f] end",
		},
		{
			name: "leading whitespace in the destination",
			in:   "![alt]( data:image/png;base64,iVBORw0K) b",
			want: "[alt] b",
		},
		{
			name: "angle-bracket destination",
			in:   "![alt](<data:image/png;base64,iVBORw0K>) b",
			want: "[alt] b",
		},
		{
			name: "raw HTML img",
			in:   `x <img alt="fig 1" src="data:image/png;base64,iVBORw0K"> y`,
			want: "x [fig 1] y",
		},
		{
			name: "raw HTML img with no alt",
			in:   `x <img src="data:image/png;base64,iVBORw0K"> y`,
			want: "x  y",
		},
		{
			name: "reference-style definition",
			in:   "see [fig1]\n\n[fig1]: data:image/png;base64,iVBORw0K",
			want: "see [fig1]\n\n[fig1]: [data uri removed]",
		},
		{
			// File-backed images resolve via image_dir multimodal
			// attachment and must be left ALONE, in every spelling.
			name: "file-backed markdown image is untouched",
			in:   "![alt](images/foo.svg) and ![b](https://x/y.png)",
			want: "![alt](images/foo.svg) and ![b](https://x/y.png)",
		},
		{
			name: "file-backed HTML image is untouched",
			in:   `<img alt="a" src="images/foo.svg">`,
			want: `<img alt="a" src="images/foo.svg">`,
		},
		{
			// An unbalanced destination must be left alone rather than
			// swallowing the rest of the document looking for a ")".
			name: "unterminated destination is left alone",
			in:   "![alt](data:image/png;base64,iVBORw0K\n\nrest of the document survives",
			want: "![alt](data:image/png;base64,iVBORw0K\n\nrest of the document survives",
		},
		{
			name: "two references on one line",
			in:   "![a](data:image/png;base64,AA) mid ![b](data:image/png;base64,BB) end",
			want: "[a] mid [b] end",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripDataURIsTemplate(tc.in); got != tc.want {
				t.Errorf("\n in:   %q\n got:  %q\n want: %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestStripDataURIsTemplate_MeetsItsSizeGoal is the finding stated as the
// doc comment states it: 30 references at ~10KB each, the document this
// helper exists for. Measured before the fix: 0.0% reduction on the
// uppercase document and 0.6% on the paren document.
func TestStripDataURIsTemplate_MeetsItsSizeGoal(t *testing.T) {
	payload := strings.Repeat("A", 10000)
	docs := map[string]string{
		"uppercase scheme": "![formula %d](DATA:image/png;base64," + payload + ")",
		"paren in body":    `![formula %d](data:image/svg+xml,<svg><path fill="rgb(1,2,3)" d="M0 0"/></svg>` + payload + ")",
		"html img":         `<img alt="formula %d" src="data:image/png;base64,` + payload + `">`,
	}
	for name, ref := range docs {
		t.Run(name, func(t *testing.T) {
			var b strings.Builder
			for i := 0; i < 30; i++ {
				fmt.Fprintf(&b, "Some prose.\n\n"+ref+"\n\n", i)
			}
			in := b.String()
			out := stripDataURIsTemplate(in)
			reduction := 100 * (1 - float64(len(out))/float64(len(in)))
			if reduction < 95 {
				t.Errorf("in=%d out=%d reduction=%.1f%% — the size guard did not fire", len(in), len(out), reduction)
			}
			if strings.Contains(out, payload) {
				t.Errorf("the 10KB payload survived in the output")
			}
			// The alt text is the point: it already encodes the math in
			// plain language, so it must still be there for the model.
			if !strings.Contains(out, "formula 29") {
				t.Errorf("alt text was dropped: %q", out[:min(200, len(out))])
			}
		})
	}
}

// TestStripToHeadingTemplate covers the third zero-coverage helper. Its
// contract is a failure direction: if no heading line exists the text comes
// back UNCHANGED, so it can never silently drop a whole section.
func TestStripToHeadingTemplate(t *testing.T) {
	const body = "thinking about ## Section here\n## Section 1\nreal content\n## Section 2\nmore"
	got := stripToHeadingTemplate(body, "## ")
	if !strings.HasPrefix(got, "## Section 1") {
		t.Errorf("want the first heading LINE, got %q", got)
	}
	// HasPrefix, not Contains: a reasoning block that merely mentions a
	// heading mid-line must not be mistaken for one.
	if strings.Contains(got, "thinking about") {
		t.Errorf("preamble survived: %q", got)
	}
	if got := stripToHeadingTemplate("no heading anywhere", "## "); got != "no heading anywhere" {
		t.Errorf("a text with no heading must come back unchanged, got %q", got)
	}
}

// TestMediawikiErrorSuffix pins the three wire shapes MediaWiki actually
// emits. Knowing only the legacy {"error":{code,info}} form meant the modern
// errorformat= shape and the mirrors' string form both returned "" — so the
// operator got "response has no query object" and no reason, which is the
// exact outcome this helper exists to prevent.
func TestMediawikiErrorSuffix(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"legacy code+info", `{"error":{"code":"invalidtitle","info":"Bad title"}}`, " (MediaWiki error invalidtitle: Bad title)"},
		{"legacy code only", `{"error":{"code":"maxlag"}}`, " (MediaWiki error maxlag)"},
		{"legacy info only", `{"error":{"info":"boom"}}`, " (MediaWiki error: boom)"},
		{"legacy empty object", `{"error":{}}`, " (MediaWiki returned an error object)"},
		{"errorformat= array", `{"errors":[{"code":"maxlag","text":"Waiting for a database server"}]}`, " (MediaWiki error maxlag: Waiting for a database server)"},
		{"errorformat= array, html key", `{"errors":[{"code":"readonly","html":"The wiki is read-only"}]}`, " (MediaWiki error readonly: The wiki is read-only)"},
		{"errorformat= array, * key", `{"errors":[{"code":"badtoken","*":"Invalid CSRF token"}]}`, " (MediaWiki error badtoken: Invalid CSRF token)"},
		{"mirror string form", `{"error":"upstream refused"}`, " (MediaWiki error: upstream refused)"},
		{"no error at all", `{"query":{}}`, ""},
		{"empty errors array", `{"errors":[]}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var resp map[string]any
			if err := json.Unmarshal([]byte(tc.in), &resp); err != nil {
				t.Fatal(err)
			}
			if got := mediawikiErrorSuffix(resp); got != tc.want {
				t.Errorf("\n got:  %q\n want: %q", got, tc.want)
			}
		})
	}
	// And it reaches the operator through the parser that calls it.
	_, err := parseWikipediaSearchTemplate(`{"errors":[{"code":"maxlag","text":"Waiting for a database server"}]}`)
	if err == nil || !strings.Contains(err.Error(), "maxlag") {
		t.Errorf("the modern error shape did not reach the stage error: %v", err)
	}
}

// TestParseArxivTemplate decodes the arXiv Atom feed shape and
// collapses wrapped whitespace in title + summary (arXiv columns at
// ~80 chars, which would otherwise interpolate ugly \n into prompts).
func TestParseArxivTemplate(t *testing.T) {
	in := `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <entry>
    <id>http://arxiv.org/abs/1706.03762v5</id>
    <title>Attention Is
       All You Need</title>
    <summary>The dominant sequence transduction models are based on
      complex recurrent or convolutional neural networks...</summary>
  </entry>
  <entry>
    <id>http://arxiv.org/abs/2010.11929v2</id>
    <title>An Image is Worth 16x16 Words</title>
    <summary>While the Transformer architecture has become the
      de-facto standard for natural language processing tasks...</summary>
  </entry>
</feed>`
	got, err := parseArxivTemplate(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(got, `"source_type":"arxiv"`) {
		t.Errorf("missing source_type in %s", got)
	}
	if !strings.Contains(got, `"title":"Attention Is All You Need"`) {
		t.Errorf("title whitespace not collapsed: %s", got)
	}
	// Two <entry> elements, each emitting exactly one item with one
	// "id" key, so the rendered JSON must contain exactly 2 occurrences.
	if n := strings.Count(got, `"id":`); n != 2 {
		t.Errorf("expected 2 item ids, got %d in %s", n, got)
	}
	if !strings.Contains(got, "http://arxiv.org/abs/1706.03762v5") {
		t.Errorf("missing arxiv id url in %s", got)
	}
}

// TestParseWikipediaSearchTemplate decodes the MediaWiki list=search
// response shape (the action=query&list=search&srsearch=... wire
// format) and strips the <span class="searchmatch"> tags Wikipedia
// wraps matched terms in. URLs are reconstructed from title via the
// canonical /wiki/<Title-underscored> path.
func TestParseWikipediaSearchTemplate(t *testing.T) {
	in := `{"query":{"search":[
		{"title":"Transformer (deep learning)","snippet":"the <span class=\"searchmatch\">transformer</span> architecture..."},
		{"title":"Attention Is All You Need","snippet":"a paper introducing <span class=\"searchmatch\">attention</span>"}
	]}}`
	got, err := parseWikipediaSearchTemplate(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `"source_type":"wikipedia"`) {
		t.Errorf("missing source_type in %s", got)
	}
	if !strings.Contains(got, `"title":"Transformer (deep learning)"`) {
		t.Errorf("missing title in %s", got)
	}
	if !strings.Contains(got, "the transformer architecture") {
		t.Errorf("snippet tags not stripped: %s", got)
	}
	// Path-escaped, not concatenated: "(" and ")" are RFC 3986 sub-delims
	// that Go's path encoder escapes. %28/%29 resolve to the same article,
	// and the escaping is what stops a "#" in a title from becoming a
	// fragment — see TestParseWikipediaSearchTemplate_TitlesArePathEscaped.
	if !strings.Contains(got, "Transformer_%28deep_learning%29") {
		t.Errorf("URL not built from title: %s", got)
	}
	if strings.Contains(got, "<span") {
		t.Errorf("HTML tags leaked: %s", got)
	}
}

func TestParseWikipediaSearchTemplate_NoResults(t *testing.T) {
	got, err := parseWikipediaSearchTemplate(`{"query":{"search":[]}}`)
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"items":[]}` {
		t.Errorf("want empty items, got %s", got)
	}
}

func TestParseArxivTemplate_Empty(t *testing.T) {
	in := `<?xml version="1.0" encoding="UTF-8"?><feed xmlns="http://www.w3.org/2005/Atom"></feed>`
	got, err := parseArxivTemplate(in)
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"items":[]}` {
		t.Errorf("want empty items, got %s", got)
	}
}
