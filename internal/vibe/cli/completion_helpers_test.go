package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestListValidProfileNames drops two well-formed profile YAML files and one
// intentionally-malformed file under a temp profiles dir, and asserts the
// completer returns only the two valid basenames in sorted order.
//
// The third file exercises the "silent filter" requirement: a profile that
// fails to parse must not break completion for its siblings.
//
// We use comfyui-backed profiles for the "valid" fixtures because they
// require no frontend block (ComfyUI ships its own UI), which keeps the
// inline YAML small. The validation surface we care about for completion
// is the same regardless of backend kind: profile.Load returns nil or
// not-nil, and the completer filters on that.
func TestListValidProfileNames(t *testing.T) {
	comfyDir := writeComfyDirStub(t)
	dir := t.TempDir()

	good := func(name string) string {
		return "" +
			"name: " + name + "\n" +
			"description: test\n" +
			"backend:\n" +
			"  comfyui:\n" +
			"    dir: " + comfyDir + "\n"
	}

	must := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must(filepath.Join(dir, "alpha.yaml"), good("alpha"))
	must(filepath.Join(dir, "beta.yaml"), good("beta"))
	// Invalid: bogus top-level key. profile.Load returns an error (yaml's
	// KnownFields(true)); the completer must drop this silently.
	must(filepath.Join(dir, "broken.yaml"), "not_a_profile: true\n")
	// Non-yaml files (README etc.) must be ignored too.
	must(filepath.Join(dir, "README.md"), "ignore me\n")

	got := listValidProfileNames(dir)
	want := []string{"alpha", "beta"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("listValidProfileNames() = %v, want %v", got, want)
	}
}

// TestListValidProfileNames_MissingDir confirms the completer treats a
// missing profiles dir as "no profiles" rather than an error. New users who
// have never run `vibe profile init` should still get clean completion
// behavior (empty list, no panic).
func TestListValidProfileNames_MissingDir(t *testing.T) {
	got := listValidProfileNames(filepath.Join(t.TempDir(), "does-not-exist"))
	if len(got) != 0 {
		t.Errorf("expected empty slice for missing dir, got %v", got)
	}
}
