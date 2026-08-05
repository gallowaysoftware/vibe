package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
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

func readFileOrFail(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
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

// TestTokenGuest_RefusesTheControlPlaneTokenFile is the review's
// destructive case. `guest_token_file: <the control-plane token file>`
// is a configuration the phase doc already lists as reachable — the
// daemon refuses it and says so, and the operator's next move is this
// command. Without a guard, `--guest` PRINTS the control-plane token
// under a banner that says share it, and `--guest --regenerate` rotates
// the control-plane token from a command whose name says guest: every
// client 401s and nothing in the output says what happened.
func TestTokenGuest_RefusesTheControlPlaneTokenFile(t *testing.T) {
	dir := vibeConfigDir(t)
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	control := paths.TokenFile()
	if err := os.MkdirAll(filepath.Dir(control), 0o755); err != nil {
		t.Fatal(err)
	}
	const controlValue = "control-plane-token-value-here"
	writeFileOrFail(t, control, controlValue+"\n")
	writeFileOrFail(t, filepath.Join(dir, "config.yaml"), "fleet:\n  guest_token_file: \""+control+"\"\n")

	// Both halves run unconditionally: the disclosure and the rotation are
	// separate failures and each has to be observable on its own.
	out, err := runTokenCmd(t, "", "--guest")
	if err == nil {
		t.Errorf("`vibe token --guest` printed something: %q", out)
	}
	if strings.Contains(out, controlValue) || (err != nil && strings.Contains(err.Error(), controlValue)) {
		t.Errorf("the control-plane token was handed out as a value to share: %q / %v", out, err)
	}
	if err != nil && !strings.Contains(err.Error(), "control-plane token file") {
		t.Errorf("err = %v, want it to name what is wrong", err)
	}

	out, err = runTokenCmd(t, "", "--guest", "--regenerate", "--yes")
	if err == nil {
		t.Errorf("`vibe token --guest --regenerate` succeeded: %q", out)
	}
	if got := strings.TrimSpace(readFileOrFail(t, control)); got != controlValue {
		t.Fatalf("the control-plane token file was ROTATED by a --guest command (%q -> %q): every "+
			"control-plane client is now locked out, from a command whose name says guest", controlValue, got)
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
