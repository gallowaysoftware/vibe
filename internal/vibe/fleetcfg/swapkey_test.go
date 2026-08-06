package fleetcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fleet-control C15: the llama-swap credential's resolution rules.

func writeSwapHosts(t *testing.T, keyLine string) *File {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "hosts.yaml")
	body := "cells:\n  front:\n    url: http://front.invalid\n    class: always_on\n" + keyLine +
		"  heavy:\n    url: http://heavy.invalid\n    class: opportunistic\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := LoadFrom(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return f
}

func TestSwapCredentialFor_UndeclaredIsNotAnError(t *testing.T) {
	f := writeSwapHosts(t, "")
	for _, cell := range []string{FrontCell, "heavy", "no-such-cell"} {
		cred, err := f.SwapCredentialFor(cell)
		if err != nil {
			t.Fatalf("%s: %v", cell, err)
		}
		if cred.Configured || cred.Key != "" {
			t.Errorf("%s resolved a key from a file that declares none: %+v", cell, cred)
		}
	}
	// A nil file is the single-box daemon: no hosts.yaml, no keys, no
	// error. Anything else would break every non-fleet daemon.
	var nilFile *File
	if cred, err := nilFile.SwapCredentialFor(FrontCell); err != nil || cred.Configured {
		t.Errorf("nil hosts.yaml: %+v, %v", cred, err)
	}
}

func TestSwapCredentialFor_ReadsTheFileAndTrims(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "front.key")
	if err := os.WriteFile(p, []byte("  sk-lab-front-key  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := writeSwapHosts(t, "    swap_key_file: "+p+"\n")
	cred, err := f.SwapCredentialFor(FrontCell)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cred.Key != "sk-lab-front-key" {
		t.Errorf("key = %q, want the trimmed value", cred.Key)
	}
	if !cred.Configured {
		t.Error("Configured is false on a declared, readable key")
	}
	if !strings.Contains(cred.Source, "swap_key_file") || !strings.Contains(cred.Source, p) {
		t.Errorf("Source = %q, want the config key and the path", cred.Source)
	}
	if strings.Contains(cred.Source, cred.Key) {
		t.Errorf("Source carries the key value: %q", cred.Source)
	}
}

// The key file path is tilde-expanded at load like token_file's: the two
// are provisioned by the same hand, into the same kind of path, and a
// `~/` that worked for one and not the other is a trap.
func TestSwapKeyFileIsTildeExpanded(t *testing.T) {
	f := writeSwapHosts(t, "    swap_key_file: ~/lab/front.key\n")
	got := f.Cells[FrontCell].SwapKeyFile
	if strings.HasPrefix(got, "~") {
		t.Fatalf("swap_key_file = %q, want an expanded path", got)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	if want := filepath.Join(home, "lab/front.key"); got != want {
		t.Errorf("swap_key_file = %q, want %q", got, want)
	}
}

// A typo'd key must fail the whole file at load, like every other
// hosts.yaml key: KnownFields(true) is what stops a misspelled
// credential from silently degrading to "no auth".
func TestSwapKeyFileTypoFailsTheLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hosts.yaml")
	if err := os.WriteFile(p, []byte("cells:\n  front:\n    url: http://x.invalid\n    class: always_on\n    swap_keyfile: /tmp/k\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFrom(p); err == nil {
		t.Fatal("a misspelled swap_key_file loaded silently")
	}
}
