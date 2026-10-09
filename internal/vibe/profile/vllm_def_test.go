package profile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeVllmDefDir writes a def named "m" into a fresh dir and returns the
// dir, so LoadBackendFrom — the exact loader the router and the daemon use,
// unknown-fields-rejected — runs against it.
func writeVllmDefDir(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "m.yaml"), []byte("name: m\nbackend:\n  vllm:\n"+body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The vllm kind declares an externally managed engine: url is the only
// load-bearing field, external is forced, and the def-level blocks that
// would silently no-op (lifecycle, router, fingerprint) are load errors.
func TestLoadBackend_VllmForcesExternal(t *testing.T) {
	def, err := LoadBackendFrom(writeVllmDefDir(t, "    url: http://127.0.0.1:8888\n    context: 850000\n    timeouts:\n      responseHeader: 600\n"), "m")
	if err != nil {
		t.Fatal(err)
	}
	if def.Backend.VLLM == nil {
		t.Fatal("vllm block did not parse")
	}
	if !def.Backend.External {
		t.Error("normalize() must force backend.external for vllm: every downstream check (render selection, gatherModels, readiness) keys on the single boolean")
	}
	if def.Backend.VLLM.URL != "http://127.0.0.1:8888" {
		t.Errorf("url = %q", def.Backend.VLLM.URL)
	}
	if def.Backend.VLLM.Context != 850000 {
		t.Errorf("context = %d, want 850000", def.Backend.VLLM.Context)
	}
	if def.Backend.VLLM.Timeouts == nil || def.Backend.VLLM.Timeouts.ResponseHeader != 600 {
		t.Errorf("timeouts did not parse: %+v", def.Backend.VLLM.Timeouts)
	}
}

func TestLoadBackend_VllmValidation(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"url required", "    url: \"\"\n", "url is required"},
		{"bad scheme", "    url: \"ftp://x\"\n", "must be an http(s) URL"},
		{"negative context", "    url: http://x\n    context: -1\n", "context must be > 0 when set"},
		{"bad api_key_env", "    url: http://x\n    api_key_env: \"9bad\"\n", "not a valid environment variable name"},
		{"negative timeout", "    url: http://x\n    timeouts: {responseHeader: -5}\n", "responseHeader must be >= 0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadBackendFrom(writeVllmDefDir(t, tc.body), "m")
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoadBackend_VllmRejectsInapplicableDefBlocks(t *testing.T) {
	cases := []struct {
		name    string
		block   string
		wantErr string
	}{
		{"lifecycle", "    url: \"http://127.0.0.1:8888\"\nlifecycle:\n  ttl: 30m\n", "lifecycle does not apply to a vllm def"},
		{"router", "    url: \"http://127.0.0.1:8888\"\nrouter:\n  aliases: [best-coder]\n", "router does not apply to a vllm def"},
		{"fingerprint", "    url: \"http://127.0.0.1:8888\"\nfingerprint: strict\n", "fingerprint does not apply to a vllm def"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "m.yaml"), []byte("name: m\nbackend:\n  vllm:\n"+tc.block), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := LoadBackendFrom(dir, "m")
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}
