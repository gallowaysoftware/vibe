// Token-based authentication for the TCP control plane. The unix-socket
// listener stays unauthenticated — filesystem permissions on the socket
// (0600, same-user-only) are the auth boundary there.
//
// Wire format: every TCP RPC must carry `Authorization: Bearer <token>`.
// The token is a 32-byte cryptographically random value, base64-url-encoded
// without padding, stored at $XDG_STATE_HOME/vibe/token with 0600 perms.
// The daemon generates the token on first start if the file doesn't exist.

package daemon

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
)

// tokenByteLen is the size of the random token before base64-url encoding.
// 32 bytes → 43 base64-url chars (no padding), comfortably above the 128-bit
// brute-force threshold while still copy-pasteable in one terminal line.
const tokenByteLen = 32

// LoadOrCreateToken returns the bearer token at paths.TokenFile(), creating
// it (with 0600 perms) if it doesn't exist. The token survives daemon
// restarts; regeneration is an explicit user action via `vibe token
// --regenerate`. The bool reports whether the token was CREATED fresh on
// this call — fleetd operators need that distinction loud in the logs,
// because a container recreate over an unmounted state dir silently mints
// a new token and every future client then 401s (fleet-control C1).
func LoadOrCreateToken() (string, bool, error) {
	path := paths.TokenFile()
	data, err := os.ReadFile(path)
	if err == nil {
		tok := strings.TrimSpace(string(data))
		if tok == "" {
			return "", false, fmt.Errorf("token file %s is empty; delete it and restart the daemon to regenerate", path)
		}
		return tok, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", false, fmt.Errorf("read token file: %w", err)
	}
	tok, err := generateToken()
	if err != nil {
		return "", false, err
	}
	if err := writeTokenFile(path, tok); err != nil {
		return "", false, err
	}
	return tok, true, nil
}

// RegenerateToken overwrites paths.TokenFile() with a fresh random value.
// The new token is returned. Callers must restart the daemon for the change
// to take effect (the running daemon caches the value in memory).
func RegenerateToken() (string, error) {
	tok, err := generateToken()
	if err != nil {
		return "", err
	}
	if err := writeTokenFile(paths.TokenFile(), tok); err != nil {
		return "", err
	}
	return tok, nil
}

func generateToken() (string, error) {
	buf := make([]byte, tokenByteLen)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func writeTokenFile(path, tok string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir state dir: %w", err)
	}
	// Write to a temp file in the same dir and rename, so a crash mid-write
	// can't leave a partial token visible. Then enforce 0600 via Chmod —
	// O_CREATE's mode is umask-filtered, Chmod isn't.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".token-*")
	if err != nil {
		return fmt.Errorf("create token tmp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.WriteString(tok + "\n"); err != nil {
		tmp.Close()
		return fmt.Errorf("write token tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close token tmp: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("chmod token tmp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename token: %w", err)
	}
	cleanup = false
	// Belt-and-braces: enforce 0600 on the final path too (rename inherits
	// mode but a pre-existing file would have kept its old mode).
	return os.Chmod(path, 0o600)
}

// bearerAuthMiddleware returns an http.Handler that enforces
// `Authorization: Bearer <token>` on every request, returning 401 with a
// plain-text body when the header is missing or wrong. Constant-time
// comparison guards against timing oracles. rejected (when non-nil)
// counts the 401s — on fleetd that count is a status field, so a client
// holding a stale token is visible from `vibe cell status` instead of
// buried in cell-side logs (fleet-control C1).
//
// GET /ui/fleet is the ONE exemption: the fleet page is a static asset
// with no fleet data in it — it must load in a bare browser tab so its
// token prompt can run; every byte of actual state still requires the
// token. Nothing else is exempt.
//
// Used as the TCP listener's outermost handler; the unix listener bypasses
// it entirely (the socket's 0600 perms are the auth boundary there).
func bearerAuthMiddleware(token string, rejected *atomic.Int64, next http.Handler) http.Handler {
	tokenBytes := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/ui/fleet" {
			next.ServeHTTP(w, r)
			return
		}
		got, ok := extractBearer(r.Header.Get("Authorization"))
		if !ok {
			if rejected != nil {
				rejected.Add(1)
			}
			unauthorized(w, "missing or malformed Authorization header (expected `Bearer <token>`)")
			return
		}
		if subtle.ConstantTimeCompare([]byte(got), tokenBytes) != 1 {
			if rejected != nil {
				rejected.Add(1)
			}
			unauthorized(w, "invalid token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// extractBearer pulls the token out of an `Authorization: Bearer <token>`
// header. Case-insensitive on the scheme name (RFC 7235 §2.1), strict on
// whitespace otherwise.
func extractBearer(h string) (string, bool) {
	const prefix = "Bearer "
	if len(h) < len(prefix) {
		return "", false
	}
	if !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(prefix):])
	if tok == "" {
		return "", false
	}
	return tok, true
}

func unauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// Make the realm explicit for any future generic HTTP client.
	w.Header().Set("WWW-Authenticate", `Bearer realm="vibe"`)
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(msg + "\n"))
}
