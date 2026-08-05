package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testGuestToken = "guest-tok-0123456789abcdef"

// vibeConfigDir points XDG at a temp dir and returns $XDG_CONFIG_HOME/vibe.
func vibeConfigDir(t *testing.T) string {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	dir := filepath.Join(xdg, "vibe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeFileOrFail(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// guestConfigured writes a config.yaml pointing at a guest token file
// that already holds testGuestToken, and returns that path.
func guestConfigured(t *testing.T) string {
	t.Helper()
	dir := vibeConfigDir(t)
	path := filepath.Join(dir, "guest-token")
	writeFileOrFail(t, filepath.Join(dir, "config.yaml"), "fleet:\n  guest_token_file: \""+path+"\"\n")
	writeFileOrFail(t, path, testGuestToken+"\n")
	return path
}

func runTokenCmd(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	cmd := tokenCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// TestTokenGuest_UnconfiguredSaysHowToConfigureIt: guest access is off by
// default, so an unset key is a configuration answer, not a missing
// file. C6's fail-fast rule — the error names the fix.
func TestTokenGuest_UnconfiguredSaysHowToConfigureIt(t *testing.T) {
	dir := vibeConfigDir(t)
	writeFileOrFail(t, filepath.Join(dir, "config.yaml"), "proxy_port: 9000\n")
	out, err := runTokenCmd(t, "", "--guest")
	if err == nil {
		t.Fatalf("printed something for an unconfigured guest token: %q", out)
	}
	if !strings.Contains(err.Error(), "guest_token_file") {
		t.Errorf("err = %v, want it to name the config key", err)
	}
}

// TestTokenGuest_PrintsTheConfiguredFile: this token exists to be
// SHARED, so reading it must not require knowing where the daemon keeps
// it.
func TestTokenGuest_PrintsTheConfiguredFile(t *testing.T) {
	guestConfigured(t)
	out, err := runTokenCmd(t, "", "--guest")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if strings.TrimSpace(out) != testGuestToken {
		t.Errorf("out = %q, want the guest token", out)
	}
}

// TestTokenGuest_RegenerateRevokesEveryGuestAndSaysSo: there are no
// per-guest credentials, so rotation is fleet-wide — the prompt says
// that before it happens, and declining changes nothing.
func TestTokenGuest_RegenerateRevokesEveryGuestAndSaysSo(t *testing.T) {
	path := guestConfigured(t)

	out, err := runTokenCmd(t, "n\n", "--guest", "--regenerate")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "EVERY guest") || !strings.Contains(out, "aborted") {
		t.Errorf("out = %q, want the fleet-wide revocation warning and an abort on 'n'", out)
	}
	if data, _ := os.ReadFile(path); strings.TrimSpace(string(data)) != testGuestToken {
		t.Error("declining the prompt still rewrote the token file")
	}

	out, err = runTokenCmd(t, "", "--guest", "--regenerate", "--yes")
	if err != nil {
		t.Fatalf("execute --yes: %v", err)
	}
	fresh, _ := os.ReadFile(path)
	rotated := strings.TrimSpace(string(fresh))
	if rotated == testGuestToken {
		t.Fatal("--yes did not rotate the token")
	}
	if !strings.Contains(out, rotated) {
		t.Error("the new token was not printed, so it cannot be shared")
	}
	if !strings.Contains(out, "Restart the daemon") {
		t.Error("rotation does not take effect until a restart; the output must say so")
	}
}
