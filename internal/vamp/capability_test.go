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
	c := &Capabilities{Mapping: map[string]string{"reasoning": "code", "vision": "vis"}}
	_, err := c.Profile("missing")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "reasoning") && !strings.Contains(err.Error(), "vision") {
		t.Errorf("err = %v should list known capabilities", err)
	}
}
