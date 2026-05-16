package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// TestPipelineCandidates_XDG_AndLocal drops two pipeline-ish yaml files
// in an "XDG" dir and one in a "local" dir and confirms the completer
// returns the union with the expected prefixes — full path for XDG hits,
// "./<name>" for local hits — and ignores non-yaml siblings.
func TestPipelineCandidates_XDG_AndLocal(t *testing.T) {
	xdg := t.TempDir()
	local := t.TempDir()

	must := func(path string) {
		t.Helper()
		if err := os.WriteFile(path, []byte("name: stub\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must(filepath.Join(xdg, "one.yaml"))
	must(filepath.Join(xdg, "two.yaml"))
	must(filepath.Join(xdg, "README.md")) // ignored
	must(filepath.Join(local, "pipeline.yaml"))
	must(filepath.Join(local, "notes.txt")) // ignored

	got := pipelineCandidates(xdg, local)
	want := []string{
		"./pipeline.yaml",
		filepath.Join(xdg, "one.yaml"),
		filepath.Join(xdg, "two.yaml"),
	}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pipelineCandidates() = %v, want %v", got, want)
	}
}

// TestPipelineCandidates_MissingDirs verifies both sources are optional:
// nothing exists, nothing is returned, no error.
func TestPipelineCandidates_MissingDirs(t *testing.T) {
	got := pipelineCandidates(
		filepath.Join(t.TempDir(), "no-xdg"),
		filepath.Join(t.TempDir(), "no-local"),
	)
	if len(got) != 0 {
		t.Errorf("expected empty slice for missing dirs, got %v", got)
	}
}
