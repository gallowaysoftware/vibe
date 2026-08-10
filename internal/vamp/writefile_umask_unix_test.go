//go:build unix

package vamp

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestWriteFile_HonoursUmask pins the permission bits of every file the
// executor persists: stage outputs (exec.go's two writeFile call sites) and
// the confirm executor's pending file. One of those stage outputs is a
// webhook stage's raw HTTP response body, which #76 deliberately writes
// verbatim on the reasoning that the run dir is private — a mode the
// operator's umask cannot narrow is what breaks that reasoning.
//
// os.Chmod is not umask-filtered; os.WriteFile(path, data, 0o644) — the call
// the atomic rewrite replaced — is. So the assertion is "same bits the code
// this replaced produced", not a new policy.
//
// The umask is process-global. This test is deliberately NOT t.Parallel, so
// it runs in the sequential phase, and the deferred restore puts it back
// before any parallel test resumes.
func TestWriteFile_HonoursUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)

	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "out.txt")
	if err := writeFile(path, "body"); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	got, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// The reference: what the plain os.WriteFile this function replaced
	// produces under the same umask, read from disk rather than asserted as
	// a constant, so the test states the invariant and not a copy of it.
	ref := filepath.Join(dir, "reference.txt")
	if err := os.WriteFile(ref, []byte("body"), 0o644); err != nil {
		t.Fatal(err)
	}
	want, err := os.Stat(ref)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode().Perm() != want.Mode().Perm() {
		t.Errorf("writeFile produced mode %v under umask 077; os.WriteFile(0644) gives %v",
			got.Mode().Perm(), want.Mode().Perm())
	}

	// No temp-file residue, and the content survived the rename.
	ents, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != "out.txt" {
		names := make([]string, 0, len(ents))
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("unexpected entries after writeFile: %v", names)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "body" {
		t.Errorf("content = %q, want %q", data, "body")
	}
}

// TestWriteFile_CleansUpOnRenameFailure keeps the property the review
// verified and asked not to regress while the temp-file creation changed:
// every error path removes the temp file it created.
func TestWriteFile_CleansUpOnRenameFailure(t *testing.T) {
	dir := t.TempDir()
	// A directory at the target path makes os.Rename fail.
	target := filepath.Join(dir, "busy")
	if err := os.MkdirAll(filepath.Join(target, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(target, "body"); err == nil {
		t.Fatal("writeFile onto a non-empty directory should fail")
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != "busy" {
		names := make([]string, 0, len(ents))
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("temp-file residue after a failed writeFile: %v", names)
	}
}
