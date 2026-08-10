package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/vamp"
)

// TestRunPipeline_DryRunWritesNothing pins `--dry-run`'s own promise.
//
// The flag's help text and RunPipeline's own refusal of --run-dir both say
// "no files are written", and one thing did: the cache store was
// constructed unconditionally, and cache.New MkdirAlls its root. One
// mkdir is not a disaster; a flag whose stated contract is "writes
// nothing" and which writes something is, because that contract is the
// entire reason to reach for the flag on an unfamiliar pipeline. DryRun
// never consults e.Cache, so the store was pure side effect.
//
// Hermetic by construction: all three XDG roots point at temp dirs, so
// the test neither reads nor writes anything of the operator's.
func TestRunPipeline_DryRunWritesNothing(t *testing.T) {
	cfg, state, cacheRoot := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("XDG_CACHE_HOME", cacheRoot)
	if err := os.MkdirAll(filepath.Join(cfg, "vamp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "vamp", "capabilities.yaml"),
		[]byte("capabilities:\n  reasoning: code\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &vamp.Pipeline{
		Name: "dry",
		Stages: []vamp.Stage{
			{ID: "a", Capability: "reasoning", Prompt: "hello {{.inputs.topic}}", Output: "a.md"},
		},
	}
	var out bytes.Buffer
	err := RunPipeline(context.Background(), p, t.TempDir(), nil,
		RunOptions{DryRun: true, Inputs: []string{"topic=otters"}}, &out, &out)
	if err != nil {
		t.Fatalf("RunPipeline --dry-run: %v\noutput:\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "hello otters") {
		t.Fatalf("the dry run did not render the plan:\n%s", out.String())
	}
	for what, root := range map[string]string{
		"the cache root": filepath.Join(cacheRoot, "vamp"),
		"the runs dir":   filepath.Join(state, "vamp", "runs"),
	} {
		if _, err := os.Stat(root); err == nil {
			t.Errorf("--dry-run created %s (%s); the flag's contract is that no files are written", what, root)
		}
	}
}
