package usagemeter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fleet-control C15: /api/metrics/activity is gated by llama-swap's
// apiKeys like every route except /health, so the collector carries a
// key when one is declared. fleetd runs this collector against the FRONT
// for C7b's actual cloud spend, and a silent 401 there renders as "not
// measured" — indistinguishable from a fleet nobody used.

func TestActivityPollCarriesTheAPIKey(t *testing.T) {
	const key = "swap-key-usage"
	var seen string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		if r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, `{"error":"unauthorized: invalid or missing API key"}`, http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":[],"page":1,"limit":25,"total":0,"totalPages":0}`))
	}))
	defer ts.Close()

	dir := t.TempDir()
	keyPath := filepath.Join(dir, "swap.key")
	if err := os.WriteFile(keyPath, []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newCollector(t, ts.URL, func(cfg *Config) { cfg.APIKeyFile = keyPath })
	if err := c.Poll(context.Background()); err != nil {
		t.Fatalf("poll against a keyed llama-swap failed: %v", err)
	}
	if seen != "Bearer "+key {
		t.Fatalf("Authorization = %q, want the declared key", seen)
	}
}

// A declared key file that will not read fails the poll rather than
// sending the request unauthenticated: an operator typo must not present
// as an empty activity log.
func TestActivityPollFailsClosedOnAnUnreadableKeyFile(t *testing.T) {
	requests := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = w.Write([]byte(`{"data":[],"page":1,"limit":25,"total":0,"totalPages":0}`))
	}))
	defer ts.Close()

	missing := filepath.Join(t.TempDir(), "gone.key")
	c := newCollector(t, ts.URL, func(cfg *Config) { cfg.APIKeyFile = missing })
	err := c.Poll(context.Background())
	if err == nil {
		t.Fatal("poll succeeded with an unreadable key file")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error does not name the file: %v", err)
	}
	if requests != 0 {
		t.Fatalf("%d request(s) were sent unauthenticated", requests)
	}
}
