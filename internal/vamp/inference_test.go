package vamp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestResolveModelID_Prefer pins the multi-model-catalog behavior: behind an
// external router (llama-swap) /v1/models lists every configured model, and
// resolution must pick the one the caller activated — not whichever happens
// to be listed first — falling back to first-id when there's no match (the
// exact single-model-proxy behavior this grew out of).
func TestResolveModelID_Prefer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"first-model"},{"id":"wanted-model"}]}`))
	}))
	defer srv.Close()

	cases := []struct {
		name   string
		prefer string
		want   string
	}{
		{"prefer present in catalog", "wanted-model", "wanted-model"},
		{"prefer absent falls back to first", "no-such-model", "first-model"},
		{"empty prefer keeps first-id behavior", "", "first-model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveModelID(context.Background(), srv.Client(), srv.URL, tc.prefer)
			if err != nil {
				t.Fatalf("ResolveModelID: %v", err)
			}
			if got != tc.want {
				t.Errorf("ResolveModelID(prefer=%q) = %q, want %q", tc.prefer, got, tc.want)
			}
		})
	}
}
