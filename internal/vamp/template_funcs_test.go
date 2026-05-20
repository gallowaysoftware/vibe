package vamp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
)

// TestReadFileTemplate covers the happy path + error-on-missing.
func TestReadFileTemplate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hi.txt")
	if err := os.WriteFile(p, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readFileTemplate(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello\n" {
		t.Errorf("got %q, want %q", got, "hello\n")
	}

	if _, err := readFileTemplate(filepath.Join(dir, "missing.txt")); err == nil {
		t.Error("expected error on missing file")
	}
}

func TestParseJSONTemplate(t *testing.T) {
	v, err := parseJSONTemplate(`{"data":[{"embedding":[0.1,0.2,0.3]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("top-level = %T, want map", v)
	}
	if _, ok := m["data"].([]any); !ok {
		t.Errorf("data = %T, want []any", m["data"])
	}

	if _, err := parseJSONTemplate("not-json"); err == nil {
		t.Error("expected error on invalid JSON")
	}
}

// TestUrlencodeTemplate covers the cases that motivated adding the helper:
// reserved characters that would otherwise corrupt the request (& + % space),
// non-string inputs (stringified via fmt.Sprint), and a benign empty string.
func TestUrlencodeTemplate(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"plain", "hello", "hello"},
		{"space", "hello world", "hello+world"},
		{"ampersand", "a&b", "a%26b"},
		{"plus", "a+b", "a%2Bb"},
		{"percent", "100%", "100%25"},
		{"mixed", "fermentation & wort production", "fermentation+%26+wort+production"},
		{"empty", "", ""},
		{"int", 42, "42"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := urlencodeTemplate(tc.in); got != tc.want {
				t.Errorf("urlencodeTemplate(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestUrlencodeTemplate_Pipe exercises the helper as it would appear in a
// real webhook stage's URL template — piped from the foreach var into the
// query string. The output is the exact byte sequence a downstream HTTP
// client would receive, so a regression that broke URL templating would
// be caught here.
func TestUrlencodeTemplate_Pipe(t *testing.T) {
	tmpl, err := template.New("u").Funcs(templateFuncs()).Parse(
		`http://x/?q={{ .topic | urlencode }}+suffix`,
	)
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	if err := tmpl.Execute(&sb, map[string]any{"topic": "a&b c"}); err != nil {
		t.Fatal(err)
	}
	if want := "http://x/?q=a%26b+c+suffix"; sb.String() != want {
		t.Errorf("got %q, want %q", sb.String(), want)
	}
}

func TestToJSONTemplate(t *testing.T) {
	got, err := toJSONTemplate([]float64{0.1, 0.2})
	if err != nil {
		t.Fatal(err)
	}
	if got != "[0.1,0.2]" {
		t.Errorf("got %q, want [0.1,0.2]", got)
	}
}

func TestReadFilesTemplate(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("contents of A"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.md"), []byte("contents of B"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := readFilesTemplate(filepath.Join(dir, "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "--- a.md ---") {
		t.Errorf("missing a.md header: %s", got)
	}
	if !strings.Contains(got, "contents of A") {
		t.Errorf("missing a.md content: %s", got)
	}
	if !strings.Contains(got, "--- b.md ---") {
		t.Errorf("missing b.md header: %s", got)
	}
	if !strings.Contains(got, "contents of B") {
		t.Errorf("missing b.md content: %s", got)
	}

	if _, err := readFilesTemplate(filepath.Join(dir, "nonexistent*.md")); err == nil {
		t.Error("expected error on no matches")
	}
}

// TestFlattenItemsTemplate covers the per-unit-script → flat-segments
// transform: concatenated `{unit_id, items}` objects in, flat array of
// items-with-unit_id-stamped out. Markdown fences and items-less objects
// are tolerated.
func TestFlattenItemsTemplate(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "two units flatten with stamped extras",
			in:   `{"unit_id":"a","items":[{"t":"1"},{"t":"2"}]}` + "\n\n" + `{"unit_id":"b","items":[{"t":"3"}]}`,
			want: `[{"t":"1","unit_id":"a"},{"t":"2","unit_id":"a"},{"t":"3","unit_id":"b"}]`,
		},
		{
			name: "no items key passes through",
			in:   `{"x":1}`,
			want: `[{"x":1}]`,
		},
		{
			name: "fenced",
			in:   "```json\n" + `{"unit_id":"a","items":[{"k":1}]}` + "\n```",
			want: `[{"k":1,"unit_id":"a"}]`,
		},
		{
			name: "empty",
			in:   "",
			want: `[]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := flattenItemsTemplate(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestUniqueByKeyTemplate covers the parent-deduplication transform: a
// flat array of sub-unit items, each with a parent_unit_id, collapses
// to one item per distinct parent (preserving input order, keeping
// first occurrence).
func TestUniqueByKeyTemplate(t *testing.T) {
	cases := []struct {
		name string
		key  string
		in   string
		want string
	}{
		{
			name: "two parents three sub-units",
			key:  "parent_unit_id",
			in:   `[{"unit_id":"a_part1","parent_unit_id":"a"},{"unit_id":"a_part2","parent_unit_id":"a"},{"unit_id":"b","parent_unit_id":"b"}]`,
			want: `{"items":[{"parent_unit_id":"a","unit_id":"a_part1"},{"parent_unit_id":"b","unit_id":"b"}]}`,
		},
		{
			name: "wrapped object input",
			key:  "parent_unit_id",
			in:   `{"items":[{"x":1,"parent_unit_id":"a"},{"x":2,"parent_unit_id":"a"}]}`,
			want: `{"items":[{"parent_unit_id":"a","x":1}]}`,
		},
		{
			name: "missing key passes through",
			key:  "parent_unit_id",
			in:   `[{"unit_id":"x"}]`,
			want: `{"items":[{"unit_id":"x"}]}`,
		},
		{
			name: "empty",
			key:  "parent_unit_id",
			in:   "",
			want: `{"items":[]}`,
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
	if _, err := uniqueByKeyTemplate("", "[]"); err == nil {
		t.Error("expected error on empty key")
	}
}

// TestStageCacheKey_ImageHashesInvalidate locks down that text-stage cache
// keys differ when the attached image bytes differ. Without this, a lesson
// dir with an updated SVG (e.g. a corrected diagram) would serve the
// previous run's stale output on re-execution.
func TestStageCacheKey_ImageHashesInvalidate(t *testing.T) {
	base := keyInput{
		StageType:      StageTypeText,
		Capability:     "vision",
		RenderedPrompt: "describe these images",
		ModelID:        "gemma-3-27b-it",
	}
	keyNoImages, err := stageCacheKey(base)
	if err != nil {
		t.Fatal(err)
	}
	withA := base
	withA.ImageHashes = []string{"hashA"}
	keyA, err := stageCacheKey(withA)
	if err != nil {
		t.Fatal(err)
	}
	withB := base
	withB.ImageHashes = []string{"hashB"}
	keyB, err := stageCacheKey(withB)
	if err != nil {
		t.Fatal(err)
	}
	if keyNoImages == keyA || keyA == keyB || keyNoImages == keyB {
		t.Errorf("expected three distinct keys, got noImages=%s A=%s B=%s", keyNoImages, keyA, keyB)
	}
}

// TestMergeJSONTemplate exercises the foreach-merge path: per-item JSON
// outputs come in compact, pretty-printed, and markdown-fenced forms, all of
// which must merge into one array. Real per-lesson LLM output adds ```json
// fences even when prompts forbid them, and pretty-prints across many lines,
// so a line-by-line parser doesn't cut it.
func TestMergeJSONTemplate(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "compact one-per-line",
			in:   `{"a":1}` + "\n\n" + `{"b":2}`,
			want: `[{"a":1},{"b":2}]`,
		},
		{
			name: "pretty-printed multi-line",
			in:   "{\n  \"a\": 1\n}\n\n{\n  \"b\": 2\n}",
			want: `[{"a":1},{"b":2}]`,
		},
		{
			name: "fenced",
			in:   "```json\n{\"a\":1}\n```\n\n```json\n{\"b\":2}\n```",
			want: `[{"a":1},{"b":2}]`,
		},
		{
			name: "empty input",
			in:   "",
			want: `[]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mergeJSONTemplate(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}

	if _, err := mergeJSONTemplate(`{"a":1} {"b":`); err == nil {
		t.Error("expected error on truncated trailing object")
	}
}

// TestTemplateChain_ReadFileParseJSONToJSON exercises the full
// readFile → parseJSON → field-access → toJSON path the RAG-eval
// pipelines depend on for chaining webhook responses.
func TestTemplateChain_ReadFileParseJSONToJSON(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "embed.json")
	body := `{"data":[{"embedding":[0.5,0.6,0.7]}]}`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	tmpl, err := template.New("t").Funcs(templateFuncs()).Parse(
		`{{ index (index (index (readFile .path | parseJSON) "data") 0) "embedding" | toJSON }}`,
	)
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	if err := tmpl.Execute(&sb, map[string]any{"path": p}); err != nil {
		t.Fatal(err)
	}
	if sb.String() != "[0.5,0.6,0.7]" {
		t.Errorf("got %q, want [0.5,0.6,0.7]", sb.String())
	}
}
