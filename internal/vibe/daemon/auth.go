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
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"unicode"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
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

// guestTokenMinLen is the floor for a guest token an operator wrote by
// hand. A credential that gets pasted into a phone browser and lives in
// localStorage on a device that leaves the house does not get to be
// `guest123`; minted tokens are 43 chars and clear it by a mile.
const guestTokenMinLen = 16

// guestAuthHeader tells the fleet page which of the two credentials it is
// holding, so a guest is offered a read-only page instead of buttons that
// 401 and a token prompt that lies about their token being wrong. It is a
// COURTESY, not a boundary: authority is decided here, on every request,
// and a forged header changes nothing.
const guestAuthHeader = "X-Vibe-Auth"

// authGuard is the credential set the TCP listener enforces: the
// control-plane token, the optional guest read-only token
// (fleet-control C12; empty disables guest access entirely), and the two
// counters those rejections land in.
type authGuard struct {
	token      string
	guestToken string
	// rejected counts 401s from a missing, malformed or unknown token —
	// on fleetd that count is a status field, so a client holding a stale
	// token is visible from `vibe cell status` instead of buried in
	// cell-side logs (fleet-control C1).
	rejected *atomic.Int64
	// guestRejected counts 401s from a VALID guest token on a route its
	// allowlist does not name. Separate on purpose: see DaemonInfo.
	guestRejected *atomic.Int64
}

// bearerAuthMiddleware returns an http.Handler that enforces
// `Authorization: Bearer <token>` on every request, returning 401 with a
// plain-text body when the header is missing or wrong. Constant-time
// comparison guards against timing oracles.
//
// What a request may reach without the control-plane token is declared
// route-by-route in fleetapi's table and read here through
// fleetapi.AccessFor — a POSITIVE allowlist keyed on exact
// (method, path), evaluated on the RAW request path before the mux
// cleans anything. Everything undeclared, /mcp and the whole Connect
// mount included, is control-plane-token-only. A denylist would silently
// grant every route added after it was written, and this plan adds
// routes most phases.
//
// Two levels sit below the control-plane token:
//   - AccessPublic (exactly GET /ui/fleet): the fleet page is a static
//     asset with no fleet data in it and must load in a bare browser tab
//     so its token prompt can run. Do NOT widen this to a prefix match or
//     a path.Clean — the boundary is pinned in
//     daemon/fleet_registry_test.go.
//   - AccessGuest (exactly GET /api/fleet/state + /events): the guest
//     read-only bearer, when one is configured.
//
// Used as the TCP listener's outermost handler; the unix listener bypasses
// it entirely (the socket's 0600 perms are the auth boundary there).
func bearerAuthMiddleware(g authGuard, next http.Handler) http.Handler {
	tokenBytes := []byte(g.token)
	guestBytes := []byte(g.guestToken)
	guestOn := g.guestToken != ""
	count := func(c *atomic.Int64) {
		if c != nil {
			c.Add(1)
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		access := fleetapi.AccessFor(r.Method, r.URL.Path)
		if access == fleetapi.AccessPublic {
			next.ServeHTTP(w, r)
			return
		}
		got, ok := extractBearer(r.Header.Get("Authorization"))
		if !ok {
			count(g.rejected)
			unauthorized(w, "missing or malformed Authorization header (expected `Bearer <token>`)")
			return
		}
		if subtle.ConstantTimeCompare([]byte(got), tokenBytes) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		if guestOn && subtle.ConstantTimeCompare([]byte(got), guestBytes) == 1 {
			if access != fleetapi.AccessGuest {
				// 401 rather than 403, with the same challenge every other
				// rejection carries: one status code for "this credential
				// does not open this door" means a guest token cannot be
				// used to map which routes exist. The BODY may differ —
				// telling an operator their working token is invalid is
				// the one answer that is definitely wrong.
				count(g.guestRejected)
				unauthorized(w, "read-only token: not permitted on this path")
				return
			}
			w.Header().Set(guestAuthHeader, "guest")
			next.ServeHTTP(w, r)
			return
		}
		count(g.rejected)
		unauthorized(w, "invalid token")
	})
}

// LoadOrCreateGuestToken returns the guest read-only bearer at path,
// minting one (32 random bytes, 0600, atomic tmp+rename — the same
// writer the control-plane token uses) when the file does not exist:
// naming the path in config.yaml IS the opt-in, and making the operator
// also invent a random string invites `guest123`. The bool reports
// whether it was created fresh on this call.
//
// Every failure is a refusal to enable guest access, never a weaker
// grant, and no error mentions the token VALUE. controlToken is compared
// so a copy of the control-plane token can never be served as a
// read-only credential — that mistake would authenticate on the first
// comparison in the middleware and grant every verb.
func LoadOrCreateGuestToken(path, controlToken string) (string, bool, error) {
	data, readErr := os.ReadFile(path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return "", false, fmt.Errorf("read guest token file: %w", readErr)
	}
	created := false
	tok := strings.TrimSpace(string(data))
	if errors.Is(readErr, os.ErrNotExist) {
		fresh, err := generateToken()
		if err != nil {
			return "", false, err
		}
		if err := writeTokenFile(path, fresh); err != nil {
			return "", false, fmt.Errorf("create guest token file: %w", err)
		}
		tok, created = fresh, true
	}
	if err := validateGuestToken(tok, controlToken); err != nil {
		return "", false, err
	}
	return tok, created, nil
}

// loadGuestToken resolves the optional guest read-only bearer and
// reports it (empty means guest access stays off). Every failure is
// non-fatal and fails CLOSED: fleetd is read-and-request-only (design
// invariant 4), so killing the fleet's observability because a shared
// read token was mis-mounted trades a real outage for a feature nobody
// has used yet. The log names the path and the reason, never the value.
func (d *Daemon) loadGuestToken(controlToken string) string {
	path := d.cfg.Fleet.GuestTokenFile
	if path == "" {
		return ""
	}
	tok, created, err := LoadOrCreateGuestToken(path, controlToken)
	if err != nil {
		slog.Error("guest read-only token DISABLED (the control-plane token is unaffected)", "path", path, "err", err)
		return ""
	}
	if created {
		slog.Warn("guest read-only token CREATED (new) — share this value, not the control-plane token", "path", path)
	} else {
		slog.Info("guest read-only token loaded", "path", path)
	}
	d.guestEnabled.Store(true)
	return tok
}

// RegenerateGuestToken overwrites path with a fresh random value and
// returns it. Every guest holding the old one is revoked at the daemon's
// next start; there is no per-guest revocation because there are no
// per-guest tokens.
func RegenerateGuestToken(path string) (string, error) {
	tok, err := generateToken()
	if err != nil {
		return "", err
	}
	if err := writeTokenFile(path, tok); err != nil {
		return "", err
	}
	return tok, nil
}

// validateGuestToken is the fail-closed ladder. No message may quote the
// token: these strings reach the daemon log.
func validateGuestToken(tok, controlToken string) error {
	if tok == "" {
		return errors.New("guest token file is empty")
	}
	if len(tok) < guestTokenMinLen {
		return fmt.Errorf("guest token is shorter than %d characters", guestTokenMinLen)
	}
	if strings.ContainsFunc(tok, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return errors.New("guest token contains whitespace or control characters and cannot survive an Authorization header")
	}
	if subtle.ConstantTimeCompare([]byte(tok), []byte(controlToken)) == 1 {
		return errors.New("guest token is identical to the control-plane token; as a guest credential it would grant every verb")
	}
	return nil
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
