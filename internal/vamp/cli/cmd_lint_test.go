package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vamp"
)

func TestLintPipeline_CleanPipeline(t *testing.T) {
	// A pipeline whose only webhook is declared and whose only JSON
	// text stage retries on invalid_output should lint clean.
	p := &vamp.Pipeline{
		Name: "clean",
		RequiredServices: []vamp.ServiceRequirement{
			{Name: "search", URL: "http://127.0.0.1:14002", SetupHint: "vibe start searxng"},
		},
		Stages: []vamp.Stage{
			{ID: "fetch", Type: vamp.StageTypeWebhook, URL: "http://127.0.0.1:14002/search?q=hi"},
			{ID: "extract", Type: vamp.StageTypeText, OutputFormat: "json", Retry: &vamp.RetryPolicy{
				MaxAttempts:    3,
				InitialBackoff: time.Second,
				RetryOn:        []string{"invalid_output", "transient"},
			}},
		},
	}
	var buf bytes.Buffer
	if err := LintPipeline(p, &buf); err != nil {
		t.Fatalf("LintPipeline: %v", err)
	}
	if got := buf.String(); !strings.Contains(got, "clean") {
		t.Errorf("clean pipeline should report 'clean'; got: %q", got)
	}
	if strings.Contains(buf.String(), "⚠") {
		t.Errorf("clean pipeline should not surface findings; got: %q", buf.String())
	}
}

func TestLintPipeline_WebhookMissingService(t *testing.T) {
	p := &vamp.Pipeline{
		Name: "missing-svc",
		// No RequireService declared.
		Stages: []vamp.Stage{
			{ID: "fetch", Type: vamp.StageTypeWebhook, URL: "http://127.0.0.1:14002/search?q=test"},
		},
	}
	var buf bytes.Buffer
	_ = LintPipeline(p, &buf)
	got := buf.String()
	if !strings.Contains(got, "fetch") {
		t.Errorf("finding should reference the stage id; got: %q", got)
	}
	if !strings.Contains(got, "RequireService") {
		t.Errorf("finding should mention RequireService; got: %q", got)
	}
}

func TestLintPipeline_WebhookOnRemoteHostSkipped(t *testing.T) {
	// Public services aren't expected to be RequireService entries.
	p := &vamp.Pipeline{
		Name: "remote",
		Stages: []vamp.Stage{
			{ID: "fetch", Type: vamp.StageTypeWebhook, URL: "https://api.openai.com/v1/whatever"},
		},
	}
	var buf bytes.Buffer
	_ = LintPipeline(p, &buf)
	if strings.Contains(buf.String(), "⚠") {
		t.Errorf("remote webhook URL should not trip the linter; got: %q", buf.String())
	}
}

func TestLintPipeline_WebhookTemplateSkipped(t *testing.T) {
	// We can't statically resolve template-bearing URLs, so skip them.
	p := &vamp.Pipeline{
		Name: "templated",
		Stages: []vamp.Stage{
			{ID: "fetch", Type: vamp.StageTypeWebhook, URL: "http://127.0.0.1:14002/search?q={{.inputs.query}}"},
		},
	}
	var buf bytes.Buffer
	_ = LintPipeline(p, &buf)
	// Even though no RequireService is declared, the URL is templated
	// so the linter shouldn't second-guess it.
	if strings.Contains(buf.String(), "fetch") {
		t.Errorf("templated webhook URL should not produce a finding; got: %q", buf.String())
	}
}

func TestLintPipeline_JSONStageWithoutInvalidOutputRetry(t *testing.T) {
	p := &vamp.Pipeline{
		Name: "no-retry",
		Stages: []vamp.Stage{
			{ID: "extract", Type: vamp.StageTypeText, OutputFormat: "json"},
		},
	}
	var buf bytes.Buffer
	_ = LintPipeline(p, &buf)
	got := buf.String()
	if !strings.Contains(got, "extract") {
		t.Errorf("finding should reference the stage id; got: %q", got)
	}
	if !strings.Contains(got, "invalid_output") {
		t.Errorf("finding should name the missing retry token; got: %q", got)
	}
}

func TestLintPipeline_JSONStageWithRetryOnlyTransientFlags(t *testing.T) {
	// A Retry block that doesn't list invalid_output still fires the
	// warning — transient retries don't help when the model returns
	// successfully but with malformed JSON.
	p := &vamp.Pipeline{
		Name: "partial-retry",
		Stages: []vamp.Stage{
			{ID: "extract", Type: vamp.StageTypeText, OutputFormat: "json", Retry: &vamp.RetryPolicy{
				MaxAttempts: 2,
				RetryOn:     []string{"transient"},
			}},
		},
	}
	var buf bytes.Buffer
	_ = LintPipeline(p, &buf)
	if !strings.Contains(buf.String(), "extract") {
		t.Errorf("retry-only-transient should still trip the linter; got: %q", buf.String())
	}
}

func TestLintPipeline_TrivialRetry(t *testing.T) {
	p := &vamp.Pipeline{
		Name: "trivial-retry",
		Stages: []vamp.Stage{
			// MaxAttempts=1 means "try once then give up" — defining a
			// retry block at all is misleading.
			{ID: "fetch", Type: vamp.StageTypeWebhook, URL: "https://api.example.com/x",
				Retry: &vamp.RetryPolicy{MaxAttempts: 1, RetryOn: []string{"transient"}}},
		},
	}
	var buf bytes.Buffer
	_ = LintPipeline(p, &buf)
	got := buf.String()
	if !strings.Contains(got, "fetch") || !strings.Contains(got, "MaxAttempts=1") {
		t.Errorf("finding should name the stage + attempts count; got: %q", got)
	}
}

func TestLintPipeline_MissingCapabilityHint(t *testing.T) {
	p := &vamp.Pipeline{
		Name: "no-hint",
		Stages: []vamp.Stage{
			{ID: "plan", Type: vamp.StageTypeText, Capability: "reasoning", Prompt: "hi"},
			{ID: "plan2", Type: vamp.StageTypeText, Capability: "reasoning", Prompt: "hi"},
		},
		// CapabilityModelHints intentionally absent.
	}
	var buf bytes.Buffer
	_ = LintPipeline(p, &buf)
	got := buf.String()
	if !strings.Contains(got, `"reasoning"`) {
		t.Errorf("finding should name the capability; got: %q", got)
	}
	// Deduplicated: two stages share the capability but the finding fires once.
	if strings.Count(got, `capability "reasoning"`) != 1 {
		t.Errorf("missing-hint finding should be deduplicated per capability; got %d occurrences in %q",
			strings.Count(got, `capability "reasoning"`), got)
	}
}

func TestLintPipeline_CapabilityWithHintIsClean(t *testing.T) {
	p := &vamp.Pipeline{
		Name: "hinted",
		Stages: []vamp.Stage{
			{ID: "plan", Type: vamp.StageTypeText, Capability: "reasoning", Prompt: "hi"},
		},
		CapabilityModelHints: map[string]vamp.ModelHint{
			"reasoning": {MinParams: "7B", MinContext: 8192},
		},
	}
	var buf bytes.Buffer
	_ = LintPipeline(p, &buf)
	if strings.Contains(buf.String(), "no CapabilityModelHints") {
		t.Errorf("documented capability should not trip the hint check; got: %q", buf.String())
	}
}

func TestHostPortOf(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want string
		ok   bool
	}{
		{"http://127.0.0.1:14002/search", "127.0.0.1:14002", true},
		{"https://api.openai.com/v1/chat", "api.openai.com:443", true},
		{"http://localhost/", "localhost:80", true},
		{"ftp://localhost", "", false},
		{"", "", false},
		{"not a url", "", false},
	} {
		got, ok := hostPortOf(tc.raw)
		if got != tc.want || ok != tc.ok {
			t.Errorf("hostPortOf(%q) = (%q, %v), want (%q, %v)", tc.raw, got, ok, tc.want, tc.ok)
		}
	}
}
