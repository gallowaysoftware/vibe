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

func TestToJSONTemplate(t *testing.T) {
	got, err := toJSONTemplate([]float64{0.1, 0.2})
	if err != nil {
		t.Fatal(err)
	}
	if got != "[0.1,0.2]" {
		t.Errorf("got %q, want [0.1,0.2]", got)
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
