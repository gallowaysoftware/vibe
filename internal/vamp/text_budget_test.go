package vamp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// ── the budget, as a post-condition ──────────────────────────────────
//
// The review's headline: every documented budget in this region was
// advisory. Detection failed — a CRLF blank line, a non-ASCII space —
// and the chunker read "no split was needed" out of "I found no
// boundary", so a 100KB document came back as one chunk and the caller
// learned about it as a rejected embedding request at 2am. These are
// property tests rather than fixtures on purpose: a fixture pins the
// shapes somebody thought of, and the shapes nobody thought of are
// exactly where the budget stopped binding.

// realisticShapes is the input corpus both chunkers are held to. Each
// entry is a shape the review executed against the old code; 8 of 9
// blew the budget, worst 9.0x.
func realisticShapes(t *testing.T) map[string]string {
	t.Helper()
	para := strings.Repeat("0123456789", 80) // 800 bytes, no interior break
	sentence := strings.Repeat("word ", 39) + "end."
	prose := strings.Repeat(sentence+" ", 12)
	return map[string]string{
		"proper LF paragraphs":     para + "\n\n" + para + "\n\n" + para,
		"CRLF paragraphs":          para + "\r\n\r\n" + para + "\r\n\r\n" + para,
		"blank line with a space":  para + "\n \n" + para + "\n \n" + para,
		"blank line with a tab":    para + "\n\t\n" + para + "\n\t\n" + para,
		"blank line with an NBSP":  para + "\n\u00a0\n" + para + "\n\u00a0\n" + para,
		"lone CR paragraphs":       para + "\r\r" + para,
		"triple newline":           para + "\n\n\n" + para,
		"one unparagraphed blob":   strings.Repeat(para+" ", 12),
		"normal english prose":     prose,
		"prose split by NBSP":      strings.ReplaceAll(prose, ". ", ".\u00a0"),
		"prose split by U+202F":    strings.ReplaceAll(prose, ". ", ".\u202f"),
		"prose split by U+3000":    strings.ReplaceAll(prose, ". ", ".\u3000"),
		"CJK with full-width stop": strings.Repeat("这是一个很长的句子没有空格但是有很多字。", 40),
		"markdown bullets":         strings.Repeat("- an item with no terminal punctuation\n", 40),
		"no terminal punctuation":  strings.Repeat("aaaa bbbb cccc dddd eeee ffff ", 60),
		"one enormous token":       strings.Repeat("x", 4000),
		"decimal numbers only":     strings.Repeat("3.14159 2.71828 1.61803 ", 60),
		"periods with no space":    strings.Repeat("one.two.three.four.five.", 60),
	}
}

// TestChunkParagraphs_BudgetIsAPostCondition is the property the
// AGENTS.md wording ("under maxChars") always claimed and the code did
// not have. Against the pre-fix helper the CRLF row alone returned ONE
// 7596-byte chunk for a 2400-byte budget — a correctly paragraphed
// Windows document, unchunked.
func TestChunkParagraphs_BudgetIsAPostCondition(t *testing.T) {
	for _, budget := range []int{4, 17, 100, 300, 900, 2400} {
		for name, in := range realisticShapes(t) {
			raw, err := chunkParagraphsTemplate(in, budget)
			if err != nil {
				t.Fatalf("%s @%d: %v", name, budget, err)
			}
			var chunks []struct {
				Idx  int    `json:"idx"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal([]byte(raw), &chunks); err != nil {
				t.Fatalf("%s @%d: %v", name, budget, err)
			}
			var rejoined strings.Builder
			for i, c := range chunks {
				if len(c.Text) > budget {
					t.Errorf("%s @%d: chunk %d is %d bytes (%.1fx over)",
						name, budget, i, len(c.Text), float64(len(c.Text))/float64(budget))
				}
				if c.Idx != i {
					t.Errorf("%s @%d: idx %d at position %d — idx must be dense", name, budget, c.Idx, i)
				}
				rejoined.WriteString(c.Text)
			}
			// Nothing is invented and nothing is lost: the chunks'
			// non-whitespace content is the input's, in order.
			wantRunes := stripSpace(in)
			if got := stripSpace(rejoined.String()); got != wantRunes {
				t.Errorf("%s @%d: content changed (%d -> %d non-space runes)",
					name, budget, len([]rune(wantRunes)), len([]rune(got)))
			}
		}
	}
}

// TestSplitSentences_BudgetIsAPostCondition is the same property for
// the TTS chunker, where the pre-fix worst case was 9.0x.
func TestSplitSentences_BudgetIsAPostCondition(t *testing.T) {
	for _, budget := range []int{4, 17, 100, 300, 900, 2400} {
		for name, in := range realisticShapes(t) {
			var chunks []string
			raw := splitSentencesTemplate(in, budget)
			if err := json.Unmarshal([]byte(raw), &chunks); err != nil {
				t.Fatalf("%s @%d: %v", name, budget, err)
			}
			var rejoined strings.Builder
			for i, c := range chunks {
				if len(c) > budget {
					t.Errorf("%s @%d: chunk %d is %d bytes (%.1fx over)",
						name, budget, i, len(c), float64(len(c))/float64(budget))
				}
				rejoined.WriteString(c)
			}
			if got, want := stripSpace(rejoined.String()), stripSpace(in); got != want {
				t.Errorf("%s @%d: content changed (%d -> %d non-space runes)",
					name, budget, len([]rune(want)), len([]rune(got)))
			}
		}
	}
}

func stripSpace(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

// TestSplitParagraphs_BlankLineFormsAllSeparate is the detection half,
// asserted against the splitter DIRECTLY rather than through
// chunkParagraphs. That is deliberate and it is the second thing this
// pass learned from the mutation harness: routed through the chunker,
// every one of these spellings produced identical output even with
// detection broken, because splitToBudget's post-condition re-cut the
// merged blob at the same newlines. Two guards, one of which can mask
// the other — so each is asserted where it lives.
//
// Same document, same expected paragraphs, only the blank-line
// spelling differs. strings.Split(text, "\n\n") saw exactly one of
// these and returned the whole document as ONE paragraph for the rest.
func TestSplitParagraphs_BlankLineFormsAllSeparate(t *testing.T) {
	want := []string{"Para one is here", "Para two is here", "Para three"}
	seps := map[string]string{
		"LF LF":        "\n\n",
		"CRLF CRLF":    "\r\n\r\n",
		"CR CR":        "\r\r",
		"LF sp LF":     "\n \n",
		"LF tab LF":    "\n\t\n",
		"LF NBSP LF":   "\n\u00a0\n",
		"LF U+3000":    "\n\u3000\n",
		"LF LF LF":     "\n\n\n",
		"CRLF sp CRLF": "\r\n \r\n",
	}
	for name, sep := range seps {
		in := want[0] + sep + want[1] + sep + want[2]
		got := splitParagraphs(in)
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%s:\n got:  %q\n want: %q", name, got, want)
		}
	}
	// A blank line INSIDE a paragraph is the only thing that separates;
	// a plain line break does not.
	if got := splitParagraphs("line one\nline two\n\nsecond para"); len(got) != 2 {
		t.Errorf("a single newline must not separate paragraphs: %q", got)
	}
	// Line endings are normalised, so a paragraph never carries a CR.
	for _, p := range splitParagraphs("a\r\nb\r\n\r\nc") {
		if strings.Contains(p, "\r") {
			t.Errorf("paragraph %q still carries a CR", p)
		}
	}
	if got := splitParagraphs("   \n \n\t\n"); len(got) != 0 {
		t.Errorf("whitespace-only input must yield no paragraphs: %q", got)
	}
}

// TestChunkParagraphs_BlankLineFormsReachTheChunker is the end-to-end
// half: detection and the budget composed, at a budget small enough
// that a missed blank line changes the answer.
func TestChunkParagraphs_BlankLineFormsReachTheChunker(t *testing.T) {
	want := `[{"idx":0,"text":"Para one is here."},{"idx":1,"text":"Para two is here."},{"idx":2,"text":"Para three."}]`
	for name, sep := range map[string]string{
		"LF LF": "\n\n", "CRLF CRLF": "\r\n\r\n", "CR CR": "\r\r",
		"LF sp LF": "\n \n", "LF tab LF": "\n\t\n", "LF NBSP LF": "\n\u00a0\n",
	} {
		in := "Para one is here." + sep + "Para two is here." + sep + "Para three."
		got, err := chunkParagraphsTemplate(in, 20)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s:\n got:  %s\n want: %s", name, got, want)
		}
	}
}

// TestSplitSentences_NonASCIISpaceIsASentenceBoundary pins the
// isSpaceRune / TrimSpace agreement. NBSP is not exotic — it is what
// PDF extraction and a good deal of LLM output emit after an
// abbreviation — and the four-rune isSpaceRune turned six chunks into
// one, silently, while reporting a healthy result.
//
// It asserts the chunk CONTENT, not the count. Counting alone does not
// discriminate: with detection broken the text falls to the
// no-boundary fallback, splitToBudget cuts it at word boundaries, and
// for these inputs that happens to produce the same NUMBER of chunks,
// cut in the wrong places. A count-only assertion passed the mutant —
// which it did, once, before this comment existed.
func TestSplitSentences_NonASCIISpaceIsASentenceBoundary(t *testing.T) {
	sentence := strings.Repeat("word ", 19) + "end."
	want := []string{sentence, sentence, sentence}
	for _, sep := range []string{" ", "\u00a0", "\u202f", "\u3000", "\u2007", "\v", "\f"} {
		in := sentence + sep + sentence + sep + sentence
		var chunks []string
		if err := json.Unmarshal([]byte(splitSentencesTemplate(in, 120)), &chunks); err != nil {
			t.Fatal(err)
		}
		if strings.Join(chunks, "|") != strings.Join(want, "|") {
			t.Errorf("separator %q: chunks are not one-sentence-each:\n got:  %q", sep, chunks)
		}
	}
}

// TestIsSpaceRune_AgreesWithUnicodeIsSpace states the invariant
// directly: the whole defect was one function in this file disagreeing
// with the strings.TrimSpace two lines above it about what a space is.
func TestIsSpaceRune_AgreesWithUnicodeIsSpace(t *testing.T) {
	for r := rune(0); r < 0x3001; r++ {
		if isSpaceRune(r) != unicode.IsSpace(r) {
			t.Errorf("%U: isSpaceRune=%v unicode.IsSpace=%v", r, isSpaceRune(r), unicode.IsSpace(r))
		}
	}
}

// TestChunkers_MaxCharsDefaults defends the two `maxChars <= 0`
// defaults. Both were mutation-survivors: correct, and asserted
// nowhere.
func TestChunkers_MaxCharsDefaults(t *testing.T) {
	// 2400 for paragraphs: 3000 bytes of one paragraph must split,
	// 2000 must not.
	for _, tc := range []struct{ size, wantChunks int }{{2000, 1}, {3000, 2}} {
		in := strings.Repeat("ab ", tc.size/3)
		for _, maxChars := range []int{0, -1, -1000} {
			raw, err := chunkParagraphsTemplate(in, maxChars)
			if err != nil {
				t.Fatal(err)
			}
			var chunks []map[string]any
			if err := json.Unmarshal([]byte(raw), &chunks); err != nil {
				t.Fatal(err)
			}
			if len(chunks) != tc.wantChunks {
				t.Errorf("chunkParagraphs(%dB, maxChars=%d): %d chunks, want %d (default is 2400)",
					len(in), maxChars, len(chunks), tc.wantChunks)
			}
		}
	}
	// 300 for sentences.
	for _, tc := range []struct{ sentences, wantChunks int }{{5, 1}, {40, 2}} {
		in := strings.Repeat("aaaaaaaa. ", tc.sentences)
		for _, maxChars := range []int{0, -1, -1000} {
			var chunks []string
			if err := json.Unmarshal([]byte(splitSentencesTemplate(in, maxChars)), &chunks); err != nil {
				t.Fatal(err)
			}
			if len(chunks) != tc.wantChunks {
				t.Errorf("splitSentences(%dB, maxChars=%d): %d chunks, want %d (default is 300)",
					len(in), maxChars, len(chunks), tc.wantChunks)
			}
		}
	}
}

// TestSplitToBudget_BreaksAtTheBestBoundaryAvailable pins the cascade,
// which is the argument for enforcing the budget at all: the old
// behaviour's defence was "splitting mid-clause reads worse than one
// long sentence", and the answer is to not split mid-clause unless
// there is nothing better.
func TestSplitToBudget_BreaksAtTheBestBoundaryAvailable(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		budget int
		want   []string
	}{
		{
			name: "sentence boundary preferred",
			in:   "Alpha beta gamma delta. Epsilon zeta eta theta iota kappa.", budget: 40,
			want: []string{"Alpha beta gamma delta.", "Epsilon zeta eta theta iota kappa."},
		},
		{
			name: "clause boundary when no sentence end fits",
			in:   "Alpha beta gamma delta, epsilon zeta eta theta iota kappa lambda.", budget: 40,
			want: []string{"Alpha beta gamma delta,", "epsilon zeta eta theta iota kappa", "lambda."},
		},
		{
			name: "word boundary when neither fits",
			in:   "alpha beta gamma delta epsilon zeta eta theta", budget: 20,
			want: []string{"alpha beta gamma", "delta epsilon zeta", "eta theta"},
		},
		{
			name: "rune boundary when there is no whitespace at all",
			in:   "这是一个很长的句子没有空格", budget: 12,
			want: []string{"这是一个", "很长的句", "子没有空", "格"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitToBudget(tc.in, tc.budget)
			for i, p := range got {
				if len(p) > tc.budget {
					t.Errorf("piece %d is %d bytes over a %d budget: %q", i, len(p), tc.budget, p)
				}
			}
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("\n got:  %q\n want: %q", got, tc.want)
			}
			if got2 := strings.Join(splitToBudget(tc.in, len(tc.in)), "|"); got2 != tc.in {
				t.Errorf("in-budget input must pass through whole: %q", got2)
			}
		})
	}
}

// TestSplitToBudget_NeverSplitsARune is the one documented exception,
// stated as the guarantee it actually is: a piece may exceed a budget
// smaller than a single rune, and it may never contain half of one.
func TestSplitToBudget_NeverSplitsARune(t *testing.T) {
	in := "日本語のテキストです"
	for budget := 1; budget < 12; budget++ {
		pieces := splitToBudget(in, budget)
		for _, p := range pieces {
			if !utf8.ValidString(p) {
				t.Fatalf("budget %d: piece %q is not valid UTF-8", budget, p)
			}
			// The documented exception: below one rune's width a piece
			// is one whole rune and no wider.
			if len(p) > budget && utf8.RuneCountInString(p) != 1 {
				t.Errorf("budget %d: piece %q is over budget and is not a single rune", budget, p)
			}
		}
		if strings.Join(pieces, "") != in {
			t.Errorf("budget %d: pieces do not rejoin to the input: %q", budget, pieces)
		}
	}
}

// TestSplitSentences_NoSentenceBoundaryAnywhere covers the fallback
// branch — another mutation survivor. Text with no terminal
// punctuation used to come back whole, at any size.
func TestSplitSentences_NoSentenceBoundaryAnywhere(t *testing.T) {
	in := strings.Repeat("alpha beta gamma delta ", 40) // 920 bytes, no . ! ?
	var chunks []string
	if err := json.Unmarshal([]byte(splitSentencesTemplate(in, 100)), &chunks); err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 9 {
		t.Fatalf("920 bytes at a 100-byte budget gave %d chunks", len(chunks))
	}
	for i, c := range chunks {
		if len(c) > 100 {
			t.Errorf("chunk %d is %d bytes", i, len(c))
		}
	}
}

// ── record filtering ─────────────────────────────────────────────────

// TestFilterByField_LLMFalseStringsAreRejections is F4. The producer
// filterByField's own doc names is an LLM, and under JSON-mode pressure
// models emit "false" / "no" / "0" for a boolean field routinely. Every
// one of those used to be a KEEP: four items in, four out, including
// all three the model had rejected, and the pipeline reported success.
func TestFilterByField_LLMFalseStringsAreRejections(t *testing.T) {
	in := `[{"id":"a","keep":"false"},{"id":"b","keep":"no"},{"id":"c","keep":"0"},
	       {"id":"d","keep":"FALSE"},{"id":"e","keep":" False "},{"id":"f","keep":"off"},
	       {"id":"g","keep":"null"},{"id":"h","keep":"none"},{"id":"i","keep":""},
	       {"id":"j","keep":"true"},{"id":"k","keep":"yes"},{"id":"l","keep":"1"}]`
	got, err := filterByFieldTemplate("keep", in)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"items":[{"id":"j","keep":"true"},{"id":"k","keep":"yes"},{"id":"l","keep":"1"}]}`
	if got != want {
		t.Errorf("\n got:  %s\n want: %s", got, want)
	}
}

// TestIsTruthy_EmptyResultSetValues asserts the three answers that
// decide whether an EMPTY LLM result reads as kept. All three were
// correct and all three were mutation-survivors: `isTruthy nil case ->
// true` and `isTruthy empty slice/map -> true` both survived a green
// suite, so a `"keep": null` silently becoming a keep was undefended.
func TestIsTruthy_EmptyResultSetValues(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{`null`, false}, {`[]`, false}, {`{}`, false},
		{`[0]`, true}, {`{"a":1}`, true},
		{`true`, true}, {`false`, false},
		{`0`, false}, {`0.0`, false}, {`-0.0`, false}, {`1`, true}, {`-1`, true},
		{`""`, false}, {`"x"`, true},
	}
	for _, tc := range cases {
		var v any
		if err := json.Unmarshal([]byte(tc.raw), &v); err != nil {
			t.Fatal(err)
		}
		if got := isTruthy(v); got != tc.want {
			t.Errorf("isTruthy(json %s) = %v, want %v", tc.raw, got, tc.want)
		}
	}
	// Through the helper, which is where it matters: a null / [] / {}
	// `keep` field drops the item.
	got, err := filterByFieldTemplate("keep",
		`[{"id":"a","keep":null},{"id":"b","keep":[]},{"id":"c","keep":{}},{"id":"d","keep":true}]`)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"items":[{"id":"d","keep":true}]}`; got != want {
		t.Errorf("\n got:  %s\n want: %s", got, want)
	}
}

// TestUniqueByKey_NonIdentifyingKeysPassThrough is F5, and it carries
// at least TWO records of each shape on purpose. The test this replaces
// used a single item for "missing key passes through", and one item
// cannot demonstrate a passthrough — deduping one item also yields one
// item. The one-record fixture is why `uniqueByKey missing-key
// passthrough removed` survived.
func TestUniqueByKey_NonIdentifyingKeysPassThrough(t *testing.T) {
	cases := []struct{ name, key, in, want string }{
		{
			"key missing from every record", "id",
			`[{"n":1},{"n":2},{"n":3}]`,
			`{"items":[{"n":1},{"n":2},{"n":3}]}`,
		},
		{
			"key present but JSON null", "id",
			`[{"id":null,"n":1},{"id":null,"n":2},{"id":null,"n":3}]`,
			`{"items":[{"id":null,"n":1},{"id":null,"n":2},{"id":null,"n":3}]}`,
		},
		{
			"key present but empty string", "id",
			`[{"id":"","n":1},{"id":"","n":2}]`,
			`{"items":[{"id":"","n":1},{"id":"","n":2}]}`,
		},
		{
			"key present but blank string", "id",
			`[{"id":"  ","n":1},{"id":"  ","n":2}]`,
			`{"items":[{"id":"  ","n":1},{"id":"  ","n":2}]}`,
		},
		{
			"non-object items pass through", "id",
			`["scalar","scalar",{"id":"a"},{"id":"a"}]`,
			`{"items":["scalar","scalar",{"id":"a"}]}`,
		},
		{
			"a real key still dedupes", "id",
			`[{"id":"a","n":1},{"id":"a","n":2},{"id":"b","n":3}]`,
			`{"items":[{"id":"a","n":1},{"id":"b","n":3}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := uniqueByKeyTemplate(tc.key, tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("\n got:  %s\n want: %s", got, tc.want)
			}
		})
	}
}

// TestUniqueByKey_CrossTypeKeysDoNotCollide is the other half of F5:
// the map key was fmt.Sprint(value), so a JSON number and the string
// spelling of it were the same record, as were null and the literal
// string "<nil>".
func TestUniqueByKey_CrossTypeKeysDoNotCollide(t *testing.T) {
	cases := []struct {
		name, in  string
		wantItems int
	}{
		{"1 vs \"1\"", `[{"id":1,"n":"num"},{"id":"1","n":"str"}]`, 2},
		{"true vs \"true\"", `[{"id":true,"n":"b"},{"id":"true","n":"s"}]`, 2},
		{"null vs \"<nil>\"", `[{"id":null,"n":"a"},{"id":"<nil>","n":"b"}]`, 2},
		{"1 vs 1 (same type)", `[{"id":1,"n":"a"},{"id":1,"n":"b"}]`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := uniqueByKeyTemplate("id", tc.in)
			if err != nil {
				t.Fatal(err)
			}
			var wrapped struct {
				Items []any `json:"items"`
			}
			if err := json.Unmarshal([]byte(got), &wrapped); err != nil {
				t.Fatal(err)
			}
			if len(wrapped.Items) != tc.wantItems {
				t.Errorf("got %d items, want %d: %s", len(wrapped.Items), tc.wantItems, got)
			}
		})
	}
}

// ── the two helpers that had no tests at all ─────────────────────────

// TestFilterByValueTemplate covers a function that was at 0.0%. You
// could invert its `==` to `!=` — the comparison that decides which
// segments reach which TTS voice — and the whole suite stayed green.
func TestFilterByValueTemplate(t *testing.T) {
	cases := []struct{ name, field, want, in, out string }{
		{
			"keeps matches, drops non-matches", "host", "aria",
			`[{"host":"aria","t":1},{"host":"atlas","t":2},{"host":"aria","t":3}]`,
			`{"items":[{"host":"aria","t":1},{"host":"aria","t":3}]}`,
		},
		{
			"nothing matches", "host", "nobody",
			`[{"host":"aria"},{"host":"atlas"}]`,
			`{"items":[]}`,
		},
		{
			"missing field drops", "host", "aria",
			`[{"host":"aria"},{"t":1}]`,
			`{"items":[{"host":"aria"}]}`,
		},
		{
			"non-object items drop", "host", "aria",
			`["scalar",{"host":"aria"}]`,
			`{"items":[{"host":"aria"}]}`,
		},
		{
			"numbers stringify", "n", "1",
			`[{"n":1},{"n":2}]`,
			`{"items":[{"n":1}]}`,
		},
		{
			"comparison is case-sensitive", "host", "aria",
			`[{"host":"Aria"},{"host":"aria"}]`,
			`{"items":[{"host":"aria"}]}`,
		},
		{
			"wrapped input", "host", "aria",
			`{"items":[{"host":"aria"},{"host":"atlas"}]}`,
			`{"items":[{"host":"aria"}]}`,
		},
		{
			"empty input", "host", "aria", "", `{"items":[]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := filterByValueTemplate(tc.field, tc.want, tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.out {
				t.Errorf("\n got:  %s\n want: %s", got, tc.out)
			}
		})
	}
	if _, err := filterByValueTemplate("", "x", `[{"a":1}]`); err == nil {
		t.Error("expected an error on an empty field name")
	}
}

// TestJoinByFieldTemplate covers the other 0.0% function and pins every
// clause of the semantics its doc comment now spells out — the ones a
// caller cannot guess, and which cost nothing to change silently while
// the suite was green: you could convert the left outer join to an
// inner join and nothing noticed.
func TestJoinByFieldTemplate(t *testing.T) {
	cases := []struct{ name, left, right, out string }{
		{
			"left row is decorated with the right row's fields",
			`[{"id":"a"}]`, `[{"id":"a","snip":"A"}]`,
			`{"items":[{"id":"a","snip":"A"}]}`,
		},
		{
			"LEFT OUTER: an unmatched left row survives, undecorated",
			`[{"id":"a"},{"id":"zz"}]`, `[{"id":"a","snip":"A"}]`,
			`{"items":[{"id":"a","snip":"A"},{"id":"zz"}]}`,
		},
		{
			"right-only rows are dropped",
			`[{"id":"a"}]`, `[{"id":"a","snip":"A"},{"id":"b","snip":"B"}]`,
			`{"items":[{"id":"a","snip":"A"}]}`,
		},
		{
			"duplicate right keys: FIRST wins, matching uniqueByKey",
			`[{"id":"a"}]`, `[{"id":"a","snip":"first"},{"id":"a","snip":"second"}]`,
			`{"items":[{"id":"a","snip":"first"}]}`,
		},
		{
			"left wins on a field-name collision",
			`[{"id":"a","snip":"mine"}]`, `[{"id":"a","snip":"theirs"}]`,
			`{"items":[{"id":"a","snip":"mine"}]}`,
		},
		{
			"non-object LEFT items are dropped",
			`["scalar",{"id":"a"}]`, `[{"id":"a","snip":"A"}]`,
			`{"items":[{"id":"a","snip":"A"}]}`,
		},
		{
			"non-object RIGHT items are ignored",
			`[{"id":"a"}]`, `["scalar",{"id":"a","snip":"A"}]`,
			`{"items":[{"id":"a","snip":"A"}]}`,
		},
		{
			"a left row without the key passes through",
			`[{"n":1}]`, `[{"id":"a","snip":"A"}]`,
			`{"items":[{"n":1}]}`,
		},
		{
			"a null key joins nothing on either side",
			`[{"id":null,"n":"L"}]`, `[{"id":null,"snip":"R"}]`,
			`{"items":[{"id":null,"n":"L"}]}`,
		},
		{
			"a blank-string key joins nothing",
			`[{"id":"","n":"L"}]`, `[{"id":"","snip":"R"}]`,
			`{"items":[{"id":"","n":"L"}]}`,
		},
		{
			"the documented use case: kept subset rejoined to payloads",
			`[{"keep":true,"id":"x"},{"keep":true,"id":"y"}]`,
			`[{"id":"x","snippet":"X"},{"id":"y","snippet":"Y"},{"id":"z","snippet":"Z"}]`,
			`{"items":[{"id":"x","keep":true,"snippet":"X"},{"id":"y","keep":true,"snippet":"Y"}]}`,
		},
		{
			"empty inputs", "", "", `{"items":[]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := joinByFieldTemplate("id", tc.left, tc.right)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.out {
				t.Errorf("\n got:  %s\n want: %s", got, tc.out)
			}
		})
	}
	if _, err := joinByFieldTemplate("", `[{"id":"a"}]`, `[{"id":"a"}]`); err == nil {
		t.Error("expected an error on an empty field name")
	}
	if _, err := joinByFieldTemplate("id", `{{{`, `[]`); err == nil {
		t.Error("expected an error on unparseable left input")
	}
	if _, err := joinByFieldTemplate("id", `[]`, `{{{`); err == nil {
		t.Error("expected an error on unparseable right input")
	}
}

// TestAddIntTemplate: the function this repo added specifically to give
// templates arithmetic was at 0.0%, and `a + b` -> `a - b` survived.
func TestAddIntTemplate(t *testing.T) {
	for _, tc := range []struct{ a, b, want int }{
		{2, 3, 5}, {0, 0, 0}, {-1, 1, 0}, {7, -9, -2},
	} {
		if got := addIntTemplate(tc.a, tc.b); got != tc.want {
			t.Errorf("addInt(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
	// Reachable through the template surface, which is the only way a
	// pipeline reaches it.
	if got := fmt.Sprint(templateFuncMap["addInt"].(func(int, int) int)(4, 5)); got != "9" {
		t.Errorf("addInt via the FuncMap = %s, want 9", got)
	}
}
