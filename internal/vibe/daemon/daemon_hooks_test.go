package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRunHooks(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// All commands run, in order.
	marker := filepath.Join(dir, "ran")
	if err := runHooks(ctx, "p", "pre_start", []string{"touch " + marker, "echo ok"}, true); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("hook command did not run: %v", err)
	}

	// abortOnErr=true: a failing command returns an error (start would abort).
	if err := runHooks(ctx, "p", "pre_start", []string{"false"}, true); err == nil {
		t.Fatal("expected error from a failing pre_start hook")
	}

	// abortOnErr=false: failure is swallowed and later hooks still run.
	marker2 := filepath.Join(dir, "ran2")
	if err := runHooks(ctx, "p", "post_stop", []string{"false", "touch " + marker2}, false); err != nil {
		t.Fatalf("post_stop must not return an error, got %v", err)
	}
	if _, err := os.Stat(marker2); err != nil {
		t.Fatalf("later hook did not run after an earlier failure: %v", err)
	}
}
