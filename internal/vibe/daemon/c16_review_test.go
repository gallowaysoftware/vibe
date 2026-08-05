package daemon

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode"
)

// TestFleetVersions_NeverAnnouncesAValueFleetdWillRefuse.
//
// The cell-side producer reads whatever the local llama-swap says and puts
// it straight in the versions block. fleetd's ingest hygiene
// (fleetapi.validateAnnounce → clean) rejects a control character by
// refusing the WHOLE announce with 400 — which takes presence, the intent
// echo, the usage ledger feed and the probe block down with a cosmetic
// version string, permanently, because llama-swap keeps answering the same
// way. The cell then goes stale and every C9 alarm about it fires.
//
// The first cut truncated an over-long answer to 64 bytes (a guess, and a
// new matrix key that can raise a false ungated-version WARN) and did not
// look at printability at all. Both readers now share fleetapi's, which
// rejects.
func TestFleetVersions_NeverAnnouncesAValueFleetdWillRefuse(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"control character", "{\"version\":\"v2\\n39\"}"},
		{"over long", `{"version":"` + strings.Repeat("v", 500) + `"}`},
		{"not json", `v239`},
		{"nul byte", "{\"version\":\"v239\\u0000\"}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/version" {
					http.NotFound(w, r)
					return
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			v := fleetVersionsAt(t.TempDir(), srv.URL)
			if v.LlamaSwap == "" {
				return // absence is the correct answer
			}
			for _, r := range v.LlamaSwap {
				if !unicode.IsPrint(r) {
					t.Fatalf("announced llama_swap = %q, which fleetd's clean() 400s — and a 400 announce loses "+
						"presence, the intent echo, usage and probes, not just the version", v.LlamaSwap)
				}
			}
			if len(v.LlamaSwap) > 64 {
				t.Fatalf("announced llama_swap is %d bytes; the reader is meant to reject, not truncate", len(v.LlamaSwap))
			}
			t.Fatalf("announced llama_swap = %q from a malformed answer; a guess is not a measurement", v.LlamaSwap)
		})
	}

	// And the healthy path still reports.
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":"v239","commit":"dd81801","build_date":"2026-07-11T21:47:14Z"}`))
	}))
	defer ok.Close()
	if got := fleetVersionsAt(t.TempDir(), ok.URL).LlamaSwap; got != "v239" {
		t.Fatalf("llama_swap = %q, want v239", got)
	}
}
