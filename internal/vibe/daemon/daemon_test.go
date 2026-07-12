package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	vibev1 "github.com/gallowaysoftware/vibe/proto/vibe/v1"
)

// TestLoadConfig_DisableProxy pins the config surface for opting out of the
// reverse proxy (external router owns :9000): disable_proxy must round-trip
// from YAML while the other defaults (proxy port, control-plane addr) stay
// untouched — ${VIBE_API} and Status.ProxyAddr keep deriving from ProxyPort
// even when the proxy never binds.
func TestLoadConfig_DisableProxy(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	dir := filepath.Join(tmp, "vibe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("disable_proxy: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.DisableProxy {
		t.Error("DisableProxy = false, want true")
	}
	if cfg.ProxyPort != defaultProxyPort {
		t.Errorf("ProxyPort = %d, want default %d", cfg.ProxyPort, defaultProxyPort)
	}
	if cfg.HTTPAddr != defaultHTTPAddr {
		t.Errorf("HTTPAddr = %q, want default %q", cfg.HTTPAddr, defaultHTTPAddr)
	}
}

// The default (no config file) must leave the proxy enabled — the disable
// path is an explicit opt-out.
func TestLoadConfig_ProxyEnabledByDefault(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DisableProxy {
		t.Error("DisableProxy = true with no config file, want false")
	}
}

// TestLoadProfileByName_RejectsTraversal pins the daemon's control-plane
// hardening against path-traversal in the profile-name field. A
// remote-but-token-authenticated caller submitting a Start RPC with a
// profile name like "../foo" must NOT be able to read arbitrary YAML
// files off the host — even though the bearer-token gate keeps random
// internet traffic out, the daemon should refuse a syntactically
// invalid name regardless of where the request came from.
func TestLoadProfileByName_RejectsTraversal(t *testing.T) {
	cases := []string{
		"../etc-passwd",
		"foo/bar",
		"foo bar",
		"foo.yaml",
		"foo$",
		"",
		"a\nb",
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := loadProfileByName(name)
			if err == nil {
				t.Fatalf("loadProfileByName(%q): expected validation error", name)
			}
			if !strings.Contains(err.Error(), "invalid") {
				t.Fatalf("loadProfileByName(%q): error %q must mention validation, not just missing file", name, err.Error())
			}
		})
	}
}

// TestStart_RejectsBackendTraversal pins the same control-plane invariant
// for StartRequest.backend: the newer backend-activation path must not be
// able to load YAML from outside backends/ via a traversal-shaped name.
// Dotted names stay legal — real backends are named like qwen3.6-27b.
func TestStart_RejectsBackendTraversal(t *testing.T) {
	d := New(Config{})
	cases := []string{
		"../profiles/foo",
		"../../../home/user/x",
		"sub/foo",
		"foo bar",
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := d.Start(context.Background(), connect.NewRequest(&vibev1.StartRequest{Backend: name}))
			if err == nil {
				t.Fatalf("Start(backend=%q): expected error", name)
			}
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Fatalf("Start(backend=%q): code = %v, want InvalidArgument (err: %v)", name, connect.CodeOf(err), err)
			}
		})
	}
}
