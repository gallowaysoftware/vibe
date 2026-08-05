package daemon

// C12, the half that needs the package's internals: the fail-closed
// configuration ladder and the promise that a credential never reaches a
// log line or an error string.

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
)

const goodGuest = "guest-tok-0123456789abcdef"

// TestGuestToken_MisconfigurationFailsClosed is gate 9. Every rung
// disables guest access; none of them produces a weaker grant, and the
// last one is the one that matters — a copy of the control-plane token
// in the guest file would authenticate on the FIRST comparison in the
// middleware and grant every verb in the fleet.
func TestGuestToken_MisconfigurationFailsClosed(t *testing.T) {
	const control = "control-plane-token-value-here"
	for _, tc := range []struct {
		name, contents, want string
	}{
		{"empty", "", "empty"},
		{"whitespace only", "   \n\t ", "empty"},
		{"too short", "short", "shorter than"},
		{"interior whitespace", "guest tok 0123456789abcdef", "whitespace or control"},
		{"control character", "guest-tok-0123456789ab\x07cdef", "whitespace or control"},
		{"identical to the control-plane token", control, "identical to the control-plane token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "guest-token")
			if err := os.WriteFile(path, []byte(tc.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			tok, created, err := LoadOrCreateGuestToken(path, control)
			if err == nil {
				t.Fatalf("accepted a %s guest token (created=%v)", tc.name, created)
			}
			if tok != "" {
				t.Errorf("returned a token alongside an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
			// Only meaningful for values long enough not to be a substring
			// of the explanation itself ("short" is in "shorter than").
			if v := strings.TrimSpace(tc.contents); len(v) >= guestTokenMinLen && strings.Contains(err.Error(), v) {
				t.Errorf("the error quotes the token value: %v", err)
			}
		})
	}
}

// TestGuestToken_MintsWhenMissing: naming the path is the opt-in, so the
// daemon supplies the randomness rather than inviting `guest123`.
func TestGuestToken_MintsWhenMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "guest-token")
	tok, created, err := LoadOrCreateGuestToken(path, "control-plane-token-value-here")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !created {
		t.Error("created = false for a file that did not exist")
	}
	if len(tok) < 40 {
		t.Errorf("minted token is %d chars; the control-plane token's 43 is the standard here", len(tok))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("guest token file mode = %o, want 600", perm)
	}
	again, created2, err := LoadOrCreateGuestToken(path, "control-plane-token-value-here")
	if err != nil || created2 || again != tok {
		t.Errorf("second load = (%q, %v, %v), want the same token and created=false", again, created2, err)
	}
}

// TestGuestToken_NeverAppearsInLogsErrorsOrState is gate 10's log half.
// The daemon logs the PATH and the created/loaded distinction (C1's
// rule); the value is a credential and stays out of the record.
func TestGuestToken_NeverAppearsInLogsErrorsOrState(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	dir := t.TempDir()
	path := filepath.Join(dir, "guest-token")
	if err := os.WriteFile(path, []byte(goodGuest), 0o600); err != nil {
		t.Fatal(err)
	}
	d := New(Config{Fleet: FleetConfig{GuestTokenFile: path}})
	if got := d.loadGuestToken("control-plane-token-value-here"); got != goodGuest {
		t.Fatalf("loadGuestToken = %q", got)
	}
	if !d.guestEnabled.Load() {
		t.Error("guestEnabled not set after a successful load")
	}
	// The minting path logs too — exercise it in the same buffer.
	minted := filepath.Join(dir, "minted-token")
	d2 := New(Config{Fleet: FleetConfig{GuestTokenFile: minted}})
	fresh := d2.loadGuestToken("control-plane-token-value-here")
	if fresh == "" {
		t.Fatal("minting path returned no token")
	}
	logs := buf.String()
	for _, secret := range []string{goodGuest, fresh} {
		if strings.Contains(logs, secret) {
			t.Errorf("a guest token value reached the log:\n%s", logs)
		}
	}
	if !strings.Contains(logs, path) || !strings.Contains(logs, "CREATED (new)") {
		t.Errorf("the log should name the path and the created/loaded distinction:\n%s", logs)
	}
}

// TestGuestToken_DisabledLeavesTheDaemonServing: a mis-mounted share
// token must not cost the fleet its registry. fleetd is
// read-and-request-only; refusing to start would trade a real outage for
// a feature nobody has used yet.
func TestGuestToken_DisabledLeavesTheDaemonServing(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	path := filepath.Join(t.TempDir(), "guest-token")
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := New(Config{Fleet: FleetConfig{GuestTokenFile: path}})
	if got := d.loadGuestToken("control-plane-token-value-here"); got != "" {
		t.Errorf("loadGuestToken = %q, want empty (disabled)", got)
	}
	if d.guestEnabled.Load() {
		t.Error("guestEnabled set despite a refused token")
	}
	if !strings.Contains(buf.String(), "DISABLED") {
		t.Errorf("a refused guest token must say so loudly:\n%s", buf.String())
	}
}

// TestGuestToken_ControlPlaneTokenFileIsRefusedBeforeItIsRead is the
// review's path-guard case. LoadOrCreateGuestToken MINTS into a missing
// file, so a guest_token_file pointed at the control-plane token file
// would overwrite the control-plane token with a fresh random value —
// every client 401s, and the log calls it a guest token being created.
// The value comparison in the ladder cannot see this: it runs AFTER the
// minting branch, on the value the minting branch just wrote.
func TestGuestToken_ControlPlaneTokenFileIsRefusedBeforeItIsRead(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	control := paths.TokenFile()
	if err := os.MkdirAll(filepath.Dir(control), 0o755); err != nil {
		t.Fatal(err)
	}
	// Deliberately ABSENT: this is the ordering the value comparison
	// cannot defend, and the only one where the damage is silent.
	if _, err := os.Stat(control); !os.IsNotExist(err) {
		t.Fatalf("control token file already exists at %s", control)
	}

	d := New(Config{Fleet: FleetConfig{GuestTokenFile: control}})
	if got := d.loadGuestToken("control-plane-token-value-here"); got != "" {
		t.Errorf("loadGuestToken = %q, want empty: guest_token_file is the control-plane token file", got)
	}
	if d.guestEnabled.Load() {
		t.Error("guestEnabled set for a guest file that is the control-plane token file")
	}
	if _, err := os.Stat(control); !os.IsNotExist(err) {
		t.Fatalf("the guest loader MINTED over the control-plane token file at %s; a read-only credential "+
			"must never write the credential it is not", control)
	}
	if !strings.Contains(buf.String(), "control-plane token file") {
		t.Errorf("the log must name the actual reason (the path), not the value comparison:\n%s", buf.String())
	}
}

// TestGuestToken_UnsetReadsNoFile: unset is off, and off touches
// nothing.
func TestGuestToken_UnsetReadsNoFile(t *testing.T) {
	d := New(Config{})
	if got := d.loadGuestToken("control-plane-token-value-here"); got != "" {
		t.Errorf("loadGuestToken = %q with no guest_token_file", got)
	}
	if d.guestEnabled.Load() {
		t.Error("guestEnabled set with no guest_token_file")
	}
}
