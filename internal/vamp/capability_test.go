package vamp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadCapabilities(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	dir := filepath.Join(tmp, "vamp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := `
capabilities:
  reasoning: code
  creative_writing: chat
`
	if err := os.WriteFile(filepath.Join(dir, "capabilities.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCapabilities()
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Profile("reasoning"); got != "code" {
		t.Errorf("reasoning -> %q, want code", got)
	}
}

func TestLoadCapabilities_MissingFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, err := LoadCapabilities()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v", err)
	}
}

func TestCapabilities_UnknownReturnsListOfKnown(t *testing.T) {
	c := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}, "vision": {Profile: "vis"}}}
	_, err := c.Profile("missing")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "reasoning") && !strings.Contains(err.Error(), "vision") {
		t.Errorf("err = %v should list known capabilities", err)
	}
}

// TestLoadCapabilities_Shorthand confirms that the historic string form
// (`reasoning: code`) keeps working: it parses, Profile returns the bound
// profile, and Profiles returns a single-element list.
func TestLoadCapabilities_Shorthand(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	dir := filepath.Join(tmp, "vamp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := `
capabilities:
  reasoning: code
`
	if err := os.WriteFile(filepath.Join(dir, "capabilities.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCapabilities()
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Profiles("reasoning")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "code" {
		t.Errorf("Profiles(reasoning) = %v, want [code]", got)
	}
	if p, err := c.Profile("reasoning"); err != nil || p != "code" {
		t.Errorf("Profile(reasoning) = %q, %v; want code, nil", p, err)
	}
}

// TestLoadCapabilities_LongForm covers the new candidates form. The order in
// YAML must be preserved (biggest first) so the executor's fallback walks
// candidates in the operator's intended priority.
func TestLoadCapabilities_LongForm(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	dir := filepath.Join(tmp, "vamp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := `
capabilities:
  reasoning:
    candidates: [code, code_small]
  vision:
    candidates:
      - vis_big
      - vis_small
`
	if err := os.WriteFile(filepath.Join(dir, "capabilities.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCapabilities()
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Profiles("reasoning")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"code", "code_small"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Profiles(reasoning) = %v, want %v", got, want)
	}
	// Profile() returns the first candidate (backward compat: keeps the
	// "single profile" mental model intact for callers that don't care
	// about fallback).
	if p, err := c.Profile("reasoning"); err != nil || p != "code" {
		t.Errorf("Profile(reasoning) = %q, %v; want code, nil", p, err)
	}
	gotVis, err := c.Profiles("vision")
	if err != nil {
		t.Fatal(err)
	}
	if len(gotVis) != 2 || gotVis[0] != "vis_big" || gotVis[1] != "vis_small" {
		t.Errorf("Profiles(vision) = %v, want [vis_big vis_small]", gotVis)
	}
}

// TestLoadCapabilities_MixedForms verifies a single file can mix shorthand
// and long-form bindings — the schema's whole reason for existing is that
// adopters can adopt candidates one capability at a time.
func TestLoadCapabilities_MixedForms(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	dir := filepath.Join(tmp, "vamp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := `
capabilities:
  reasoning:
    candidates: [code, code_small]
  creative_writing: chat
`
	if err := os.WriteFile(filepath.Join(dir, "capabilities.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCapabilities()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := c.Profiles("reasoning"); err != nil || len(got) != 2 {
		t.Errorf("Profiles(reasoning) = %v, %v; want [code code_small]", got, err)
	}
	if got, err := c.Profiles("creative_writing"); err != nil || len(got) != 1 || got[0] != "chat" {
		t.Errorf("Profiles(creative_writing) = %v, %v; want [chat]", got, err)
	}
}

// TestLoadCapabilities_EmptyCandidates makes sure misshapen YAML (a
// `candidates:` key with an empty list) is rejected at parse time. We don't
// want a silently-empty binding to surface later as a "this capability has
// no profile" error far from the YAML it came from.
func TestLoadCapabilities_EmptyCandidates(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	dir := filepath.Join(tmp, "vamp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := `
capabilities:
  reasoning:
    candidates: []
`
	if err := os.WriteFile(filepath.Join(dir, "capabilities.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCapabilities(); err == nil {
		t.Fatal("expected error on empty candidates list")
	}
}
