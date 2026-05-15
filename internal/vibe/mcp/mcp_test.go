package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupMCPDir points XDG_CONFIG_HOME at a fresh temp dir and creates the
// vibe/mcp subdir. Returns the path to the mcp directory.
func setupMCPDir(t *testing.T) string {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	mcpDir := filepath.Join(xdg, "vibe", "mcp")
	if err := os.MkdirAll(mcpDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return mcpDir
}

func writeMCP(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoad_OK(t *testing.T) {
	dir := setupMCPDir(t)
	writeMCP(t, dir, "datadog", `type: local
command: [npx, -y, "@us-all/datadog-mcp"]
environment:
  DD_APP_KEY: ${env:DD_APP_KEY}
  DD_API_KEY: ${env:DD_API_KEY}
`)
	spec, err := Load("datadog")
	if err != nil {
		t.Fatal(err)
	}
	if spec["type"] != "local" {
		t.Errorf("type = %v, want local", spec["type"])
	}
	cmd, ok := spec["command"].([]any)
	if !ok || len(cmd) != 3 || cmd[0] != "npx" {
		t.Errorf("command = %v (%T), want [npx -y @us-all/datadog-mcp]", spec["command"], spec["command"])
	}
	env, ok := spec["environment"].(map[string]any)
	if !ok {
		t.Fatalf("environment = %v (%T), want map", spec["environment"], spec["environment"])
	}
	if env["DD_API_KEY"] != "${env:DD_API_KEY}" {
		t.Errorf("DD_API_KEY = %v, want literal ${env:DD_API_KEY} (vibe is pass-through)", env["DD_API_KEY"])
	}
}

func TestLoad_Missing(t *testing.T) {
	setupMCPDir(t)
	_, err := Load("datadog")
	if err == nil {
		t.Fatal("expected error for missing mcp file")
	}
	if !strings.Contains(err.Error(), "datadog") {
		t.Errorf("err = %v, want mention of mcp name", err)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v, want 'not found'", err)
	}
}

func TestLoadMany_OK(t *testing.T) {
	dir := setupMCPDir(t)
	writeMCP(t, dir, "a", "type: local\ncommand: [a]\n")
	writeMCP(t, dir, "b", "type: local\ncommand: [b]\n")
	specs, err := LoadMany([]string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 {
		t.Fatalf("len(specs) = %d, want 2", len(specs))
	}
	if specs["a"]["type"] != "local" || specs["b"]["type"] != "local" {
		t.Errorf("specs = %+v", specs)
	}
}

func TestLoadMany_MissingListsAvailable(t *testing.T) {
	dir := setupMCPDir(t)
	writeMCP(t, dir, "jira", "type: local\n")
	writeMCP(t, dir, "github", "type: local\n")

	_, err := LoadMany([]string{"datadog"})
	if err == nil {
		t.Fatal("expected error for missing mcp")
	}
	msg := err.Error()
	if !strings.Contains(msg, "datadog") {
		t.Errorf("err = %v, want mention of missing 'datadog'", err)
	}
	if !strings.Contains(msg, "available:") {
		t.Errorf("err = %v, want 'available:' enumeration", err)
	}
	if !strings.Contains(msg, "github") || !strings.Contains(msg, "jira") {
		t.Errorf("err = %v, want available list to include 'github' and 'jira'", err)
	}
}

func TestLoadMany_MissingWithEmptyDir(t *testing.T) {
	setupMCPDir(t)
	_, err := LoadMany([]string{"datadog"})
	if err == nil {
		t.Fatal("expected error for missing mcp")
	}
	if !strings.Contains(err.Error(), "no mcp definitions found") {
		t.Errorf("err = %v, want empty-dir hint", err)
	}
}

func TestLoad_EmptyFile(t *testing.T) {
	dir := setupMCPDir(t)
	writeMCP(t, dir, "empty", "")
	spec, err := Load("empty")
	if err != nil {
		t.Fatal(err)
	}
	if spec == nil {
		t.Fatal("spec is nil; want empty map")
	}
	if len(spec) != 0 {
		t.Errorf("spec = %v, want empty map", spec)
	}
}
