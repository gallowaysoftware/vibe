package vamp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func mustCaps(t *testing.T, body string) *Capabilities {
	t.Helper()
	var c Capabilities
	if err := yaml.Unmarshal([]byte(body), &c); err != nil {
		t.Fatalf("parse capabilities: %v", err)
	}
	return &c
}

// TestCheckRunnable_CapabilityMissing locks in the fail-fast behavior the
// validator now provides. A pipeline that references an unmapped capability
// must error at validation time, before any vibe activation happens.
func TestCheckRunnable_CapabilityMissing(t *testing.T) {
	p := &Pipeline{
		Name: "p",
		Stages: []Stage{
			{ID: "a", Type: StageTypeText, Capability: "not_a_thing", Output: "out.txt", Prompt: "hi"},
		},
	}
	caps := mustCaps(t, "capabilities:\n  reasoning: code\n  long_form: longform\n")
	err := CheckRunnable(p, t.TempDir(), caps)
	if err == nil {
		t.Fatal("expected error for unmapped capability")
	}
	if !strings.Contains(err.Error(), "not_a_thing") {
		t.Errorf("error should name the bad capability: %v", err)
	}
}

// TestCheckRunnable_PromptFileMissing covers a common dev-ex pothole: a
// prompts/ path typo currently only surfaces when the stage actually runs.
// Validate has to catch it up-front so the user sees the error in their
// editor's lint pass, not 30s into a `vamp run`.
func TestCheckRunnable_PromptFileMissing(t *testing.T) {
	dir := t.TempDir()
	p := &Pipeline{
		Name: "p",
		Stages: []Stage{
			{ID: "a", Type: StageTypeText, Capability: "reasoning", Output: "out.txt", PromptFile: "prompts/nope.md"},
		},
	}
	caps := mustCaps(t, "capabilities:\n  reasoning: code\n")
	err := CheckRunnable(p, dir, caps)
	if err == nil {
		t.Fatal("expected error for missing prompt_file")
	}
	if !strings.Contains(err.Error(), "prompts/nope.md") {
		t.Errorf("error should name the missing file: %v", err)
	}
}

// TestCheckRunnable_PromptFileTemplateBroken covers the case where the file
// exists but contains a template syntax error or an undefined function name.
// Both should fail at validate time rather than the per-stage render path.
func TestCheckRunnable_PromptFileTemplateBroken(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.md")
	if err := os.WriteFile(bad, []byte(`{{ no_such_func "x" }}`), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &Pipeline{
		Name: "p",
		Stages: []Stage{
			{ID: "a", Type: StageTypeText, Capability: "reasoning", Output: "out.txt", PromptFile: "bad.md"},
		},
	}
	caps := mustCaps(t, "capabilities:\n  reasoning: code\n")
	err := CheckRunnable(p, dir, caps)
	if err == nil {
		t.Fatal("expected error for broken prompt_file template")
	}
	if !strings.Contains(err.Error(), "no_such_func") {
		t.Errorf("error should surface the undefined function name: %v", err)
	}
}

// TestCheckRunnable_NoCapabilityCheck supports the `--no-capability-check`
// flag: when callers pass nil caps, capability lookups are skipped (other
// checks still run). This is what lets a pipeline authored for a different
// machine's capabilities config still be shape-validated locally.
func TestCheckRunnable_NoCapabilityCheck(t *testing.T) {
	p := &Pipeline{
		Name: "p",
		Stages: []Stage{
			{ID: "a", Type: StageTypeText, Capability: "made_up", Output: "out.txt", Prompt: "hi"},
		},
	}
	if err := CheckRunnable(p, t.TempDir(), nil); err != nil {
		t.Errorf("expected ok with nil caps, got %v", err)
	}
}

// TestCheckRunnable_NoCapabilityRequired covers stage types that don't
// activate a profile (audio/ffmpeg/webhook/youtube/confirm/render). Even when
// capabilities config is loaded, a render stage with no capability must
// validate clean.
func TestCheckRunnable_NoCapabilityRequired(t *testing.T) {
	p := &Pipeline{
		Name: "p",
		Stages: []Stage{
			{ID: "r", Type: StageTypeRender, Output: "out.txt", Prompt: "static"},
		},
	}
	caps := mustCaps(t, "capabilities:\n  reasoning: code\n")
	if err := CheckRunnable(p, t.TempDir(), caps); err != nil {
		t.Errorf("render stage shouldn't need a capability mapping: %v", err)
	}
}

// TestSuggestCapability_DidYouMean checks the typo-helper that powers the
// runtime "did you mean: ..." hint in Capabilities.Profiles.
func TestSuggestCapability_DidYouMean(t *testing.T) {
	caps := mustCaps(t, "capabilities:\n  long_form: longform\n  reasoning: code\n  vision: longform-vision\n")
	got := SuggestCapability(caps, "longform")
	if len(got) == 0 || got[0] != "long_form" {
		t.Errorf("got %v, want long_form first", got)
	}
}
