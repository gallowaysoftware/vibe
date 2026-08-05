package daemon

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
)

// setupXDGForToken points the token path at a temp dir so tests don't touch
// the user's real $XDG_STATE_HOME/vibe/token.
func setupXDGForToken(t *testing.T) string {
	t.Helper()
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	return state
}

func TestLoadOrCreateToken_GeneratesFreshTokenOnFirstCall(t *testing.T) {
	setupXDGForToken(t)
	tok, _, err := LoadOrCreateToken()
	if err != nil {
		t.Fatalf("LoadOrCreateToken: %v", err)
	}
	// 32 random bytes encode to 43 base64-url chars (no padding).
	if len(tok) != 43 {
		t.Errorf("token length = %d, want 43 (32 bytes base64url-no-pad)", len(tok))
	}
	decoded, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		t.Errorf("token is not valid base64url-no-pad: %v", err)
	}
	if len(decoded) != tokenByteLen {
		t.Errorf("decoded token = %d bytes, want %d", len(decoded), tokenByteLen)
	}
}

func TestLoadOrCreateToken_WritesFileWith0600(t *testing.T) {
	setupXDGForToken(t)
	if _, _, err := LoadOrCreateToken(); err != nil {
		t.Fatalf("LoadOrCreateToken: %v", err)
	}
	info, err := os.Stat(paths.TokenFile())
	if err != nil {
		t.Fatalf("stat token: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("token file mode = %o, want 0600", got)
	}
}

func TestLoadOrCreateToken_StableAcrossCalls(t *testing.T) {
	setupXDGForToken(t)
	a, _, err := LoadOrCreateToken()
	if err != nil {
		t.Fatalf("first LoadOrCreateToken: %v", err)
	}
	b, _, err := LoadOrCreateToken()
	if err != nil {
		t.Fatalf("second LoadOrCreateToken: %v", err)
	}
	if a != b {
		t.Errorf("token changed across calls: %q vs %q", a, b)
	}
}

func TestRegenerateToken_OverwritesAndPreservesMode(t *testing.T) {
	setupXDGForToken(t)
	first, _, err := LoadOrCreateToken()
	if err != nil {
		t.Fatalf("LoadOrCreateToken: %v", err)
	}
	second, err := RegenerateToken()
	if err != nil {
		t.Fatalf("RegenerateToken: %v", err)
	}
	if first == second {
		t.Errorf("regenerated token equals previous; expected fresh value")
	}
	// Mode must still be 0600 after the atomic rename.
	info, err := os.Stat(paths.TokenFile())
	if err != nil {
		t.Fatalf("stat token: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("token file mode after regenerate = %o, want 0600", got)
	}
	// Subsequent LoadOrCreateToken must return the regenerated value, not
	// the original.
	got, _, err := LoadOrCreateToken()
	if err != nil {
		t.Fatalf("post-regen LoadOrCreateToken: %v", err)
	}
	if got != second {
		t.Errorf("post-regen Load returned %q, want %q", got, second)
	}
}

func TestLoadOrCreateToken_RejectsEmptyFile(t *testing.T) {
	state := setupXDGForToken(t)
	if err := os.MkdirAll(state+"/vibe", 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-create an empty token file. LoadOrCreateToken must refuse to
	// silently regenerate (the user might be debugging a misplaced file)
	// and instead surface a clear error.
	if err := os.WriteFile(paths.TokenFile(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreateToken(); err == nil {
		t.Errorf("LoadOrCreateToken accepted empty token file; want error")
	}
}

func TestExtractBearer(t *testing.T) {
	cases := []struct {
		header  string
		want    string
		wantErr bool
	}{
		{"Bearer abc123", "abc123", false},
		{"bearer abc123", "abc123", false}, // case-insensitive scheme
		{"BEARER abc123", "abc123", false},
		{"Bearer  abc123  ", "abc123", false}, // trims surrounding whitespace
		{"", "", true},
		{"Bearer", "", true},
		{"Bearer ", "", true},
		{"Basic abc123", "", true},
		{"Token abc123", "", true},
	}
	for _, tc := range cases {
		got, ok := extractBearer(tc.header)
		if tc.wantErr {
			if ok {
				t.Errorf("extractBearer(%q) = (%q, true); want (_, false)", tc.header, got)
			}
			continue
		}
		if !ok || got != tc.want {
			t.Errorf("extractBearer(%q) = (%q, %v); want (%q, true)", tc.header, got, ok, tc.want)
		}
	}
}

func TestBearerAuthMiddleware_AcceptsCorrectToken(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(bearerAuthMiddleware(authGuard{token: "s3cret"}, next))
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if !called {
		t.Errorf("next handler not called")
	}
}

func TestBearerAuthMiddleware_RejectsMissingHeader(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })
	srv := httptest.NewServer(bearerAuthMiddleware(authGuard{token: "s3cret"}, next))
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
		t.Errorf("WWW-Authenticate = %q, want Bearer challenge", got)
	}
	if called {
		t.Errorf("next handler called despite missing auth")
	}
}

func TestBearerAuthMiddleware_RejectsWrongToken(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := httptest.NewServer(bearerAuthMiddleware(authGuard{token: "s3cret"}, next))
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}
