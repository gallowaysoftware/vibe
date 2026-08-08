package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
)

// ─── checkLlamaBinary ───────────────────────────────────────────────────────

// The four shapes a box can be in. Before C26a this check ran the top two
// rows only — it asked $PATH and nothing else — so row three FAILED doctor
// (exit non-zero) on a laptop whose only profiles are cloud_peer, pointed
// at a peer through a remote front. The same mis-fire hit comfyui-only and
// mlx-only boxes, which is why the applicable set is computed from what is
// DECLARED rather than from any one backend kind.

func TestCheckLlamaBinary_DeclaredAndPresent(t *testing.T) {
	env := &doctorEnv{lookPath: func(name string) (string, error) {
		if name != "llama-server" {
			t.Fatalf("unexpected lookup of %q", name)
		}
		return "/opt/bin/llama-server", nil
	}}
	r, ok := checkLlamaBinary(env, []string{"profile coder"})
	if !ok {
		t.Fatal("a box that declares a llama_server backend must be told about the binary")
	}
	if r.Status != statusOK {
		t.Fatalf("status = %v, want OK", r.Status)
	}
	if !strings.Contains(r.Message, "/opt/bin/llama-server") || !strings.Contains(r.Message, "profile coder") {
		t.Fatalf("message = %q, want the path AND what needs it", r.Message)
	}
}

func TestCheckLlamaBinary_DeclaredAndMissingStillFails(t *testing.T) {
	env := &doctorEnv{lookPath: func(string) (string, error) {
		return "", errors.New("not found")
	}}
	r, ok := checkLlamaBinary(env, []string{"profile coder"})
	if !ok {
		t.Fatal("not-applicable on a box that declares a llama_server backend: the check would never fire again")
	}
	if r.Status != statusFail {
		t.Fatalf("status = %v, want FAIL — a declared llama_server backend with no binary cannot start", r.Status)
	}
	if !strings.Contains(r.Message, "install llama.cpp") {
		t.Fatalf("message lacks install hint: %q", r.Message)
	}
	if !strings.Contains(r.Message, "profile coder") {
		t.Fatalf("message does not name what needs it: %q", r.Message)
	}
}

// The finding itself: nothing on disk spawns llama-server, so its absence
// is not a fault. Not-applicable, NOT a pass — a green "llama-server: ok"
// on a box with no binary would be a claim doctor cannot make.
func TestCheckLlamaBinary_NothingDeclaresItIsNotApplicable(t *testing.T) {
	env := &doctorEnv{lookPath: func(string) (string, error) {
		t.Fatal("$PATH was consulted for a binary nothing on this box would ever invoke")
		return "", nil
	}}
	if _, ok := checkLlamaBinary(env, nil); ok {
		t.Fatal("a cloud_peer-only (or comfyui-only, or mlx-only) box was failed for a missing llama-server it will never invoke")
	}
}

// ─── llamaServerUsers: the declaration scan ─────────────────────────────────

// TestLlamaServerUsers_TheFourShapesOnDisk is the scan the check is gated
// on. The mixed box is the row that matters: one cloud_peer profile and one
// llama_server backend def must still produce a user, or a fleet box would
// stop being told its binary is missing.
func TestLlamaServerUsers_TheFourShapesOnDisk(t *testing.T) {
	const cloudPeer = `name: peer
backend:
  cloud_peer:
    base_url: https://api.example.com
    api_key_env: EXAMPLE_API_KEY
    models: [example-model]
`
	const llamaProfile = `name: coder
backend:
  llama_server:
    path: /nonexistent/model.gguf
    alias: coder
    context: 4096
`
	// A definition that pins its own binary never consults $PATH.
	const pinnedBinary = `name: custom
backend:
  llama_server:
    binary: /opt/llama/bin/llama-server
    path: /nonexistent/model.gguf
    alias: custom
    context: 4096
`
	for _, tc := range []struct {
		name     string
		profiles map[string]string
		backends map[string]string
		want     []string
	}{
		{"nothing at all", nil, nil, nil},
		{"a cloud_peer-only box", map[string]string{"peer.yaml": cloudPeer}, nil, nil},
		{"a llama_server profile", map[string]string{"coder.yaml": llamaProfile}, nil, []string{"profile coder"}},
		{"a llama_server backend def", nil, map[string]string{"gpu.yaml": llamaProfile}, []string{"backend gpu"}},
		{"a mixed box", map[string]string{"peer.yaml": cloudPeer}, map[string]string{"gpu.yaml": llamaProfile},
			[]string{"backend gpu"}},
		{"a pinned binary is not a $PATH user", map[string]string{"custom.yaml": pinnedBinary}, nil, nil},
		{"an unparseable file is skipped, not fatal", map[string]string{"junk.yaml": "::: not yaml"}, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backendsDir, profilesDir := setDoctorFixtureDirs(t)
			for n, body := range tc.profiles {
				if err := os.WriteFile(filepath.Join(profilesDir, n), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			for n, body := range tc.backends {
				if err := os.WriteFile(filepath.Join(backendsDir, n), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got := llamaServerUsers(profilesDir, backendsDir)
			if len(got) != len(tc.want) {
				t.Fatalf("users = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("users = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// A llama_server profile whose model file is not on disk yet still DECLARES
// the binary. Scanning through profile.Load (which validates the path) would
// drop it and quietly make the check not-applicable on exactly the box that
// needs it — the disarm this repo keeps finding, one layer down.
func TestLlamaServerUsers_AnUnpulledModelStillDeclaresTheBinary(t *testing.T) {
	_, profilesDir := setDoctorFixtureDirs(t)
	body := `name: coder
backend:
  llama_server:
    path: ` + filepath.Join(t.TempDir(), "not-downloaded-yet.gguf") + `
    alias: coder
    context: 4096
`
	path := filepath.Join(profilesDir, "coder.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := profile.Load(path); err == nil {
		t.Fatal("the premise of this test is gone: the loader now accepts a missing model path")
	}
	if got := llamaServerUsers(profilesDir, paths.BackendsDir()); len(got) != 1 {
		t.Fatalf("users = %v, want the profile counted despite its model not being pulled yet", got)
	}
}

// ─── checkLlamaVersion ──────────────────────────────────────────────────────

func TestCheckLlamaVersion_OK(t *testing.T) {
	env := &doctorEnv{
		lookPath: func(string) (string, error) { return "/usr/bin/llama-server", nil },
		run: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
			return []byte("version: b9159-88b55f4\n"), nil
		},
	}
	r, ok := checkLlamaVersion(context.Background(), env, []string{"profile coder"})
	if !ok {
		t.Fatal("not-applicable on a box that declares a llama_server backend")
	}
	if r.Status != statusOK {
		t.Fatalf("status = %v, want OK; message=%q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "b9159") {
		t.Fatalf("message = %q", r.Message)
	}
}

func TestCheckLlamaVersion_NonZeroExitWithOutput(t *testing.T) {
	env := &doctorEnv{
		lookPath: func(string) (string, error) { return "/usr/bin/llama-server", nil },
		run: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
			return []byte("version foo\n"), errors.New("exit status 1")
		},
	}
	r, _ := checkLlamaVersion(context.Background(), env, []string{"profile coder"})
	if r.Status != statusWarn {
		t.Fatalf("status = %v, want WARN", r.Status)
	}
}

func TestCheckLlamaVersion_Skipped(t *testing.T) {
	env := &doctorEnv{
		lookPath: func(string) (string, error) { return "", errors.New("nope") },
		run: func(context.Context, string, ...string) ([]byte, error) {
			t.Fatal("run should not be called when binary missing")
			return nil, nil
		},
	}
	r, ok := checkLlamaVersion(context.Background(), env, []string{"profile coder"})
	if !ok || r.Status != statusWarn {
		t.Fatalf("ok=%v status = %v, want ok=true WARN (skipped)", ok, r.Status)
	}
}

// The version check is gated on the SAME scan as the binary check: warning
// "skipped (llama-server not on $PATH)" on a box that never spawns it is
// the same mis-fire one line down, and gating one and not the other is how
// a guard ends up on some of its call paths.
func TestCheckLlamaVersion_NothingDeclaresItIsNotApplicable(t *testing.T) {
	env := &doctorEnv{
		lookPath: func(string) (string, error) {
			t.Fatal("$PATH was consulted for a binary nothing on this box would ever invoke")
			return "", nil
		},
	}
	if _, ok := checkLlamaVersion(context.Background(), env, nil); ok {
		t.Fatal("a cloud_peer-only box was warned about a llama-server version it will never ask for")
	}
}

// ─── checkHFBinary / checkHFAuth ────────────────────────────────────────────

func TestCheckHFBinary_Missing(t *testing.T) {
	env := &doctorEnv{lookPath: func(string) (string, error) { return "", errors.New("x") }}
	r := checkHFBinary(env)
	if r.Status != statusWarn {
		t.Fatalf("status = %v, want WARN", r.Status)
	}
}

func TestCheckHFBinary_Found(t *testing.T) {
	env := &doctorEnv{lookPath: func(string) (string, error) { return "/usr/bin/hf", nil }}
	r := checkHFBinary(env)
	if r.Status != statusOK {
		t.Fatalf("status = %v, want OK", r.Status)
	}
}

func TestCheckHFAuth_NotLoggedIn(t *testing.T) {
	env := &doctorEnv{
		lookPath: func(string) (string, error) { return "/usr/bin/hf", nil },
		run: func(context.Context, string, ...string) ([]byte, error) {
			return []byte("Not logged in\n"), nil
		},
	}
	r := checkHFAuth(context.Background(), env)
	if r.Status != statusWarn {
		t.Fatalf("status = %v, want WARN", r.Status)
	}
	if !strings.Contains(r.Message, "hf auth login") {
		t.Fatalf("message lacks hint: %q", r.Message)
	}
}

func TestCheckHFAuth_LoggedIn(t *testing.T) {
	env := &doctorEnv{
		lookPath: func(string) (string, error) { return "/usr/bin/hf", nil },
		run: func(context.Context, string, ...string) ([]byte, error) {
			return []byte("kyle-galloway\n"), nil
		},
	}
	r := checkHFAuth(context.Background(), env)
	if r.Status != statusOK {
		t.Fatalf("status = %v, want OK; msg=%q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "kyle-galloway") {
		t.Fatalf("message = %q", r.Message)
	}
}

func TestCheckHFAuth_Skipped(t *testing.T) {
	env := &doctorEnv{lookPath: func(string) (string, error) { return "", errors.New("x") }}
	r := checkHFAuth(context.Background(), env)
	if r.Status != statusWarn {
		t.Fatalf("status = %v, want WARN (skipped)", r.Status)
	}
}

// ─── checkXDGDirs ───────────────────────────────────────────────────────────

func TestCheckXDGDirs_OK(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(tmp, "runtime"))
	r := checkXDGDirs()
	if r.Status != statusOK {
		t.Fatalf("status = %v, want OK; msg=%q", r.Status, r.Message)
	}
}

func TestProbeDirWritable(t *testing.T) {
	dir := t.TempDir()
	if err := probeDirWritable(dir); err != nil {
		t.Fatalf("writable temp dir failed: %v", err)
	}
	// Non-existent path.
	if err := probeDirWritable(filepath.Join(dir, "nope")); err == nil {
		t.Fatal("expected error for missing dir")
	}
	// A regular file is not a directory.
	f := filepath.Join(dir, "f")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := probeDirWritable(f); err == nil {
		t.Fatal("expected 'not a directory' error")
	}
}

// ─── port checks ────────────────────────────────────────────────────────────

func TestCheckControlPlanePort_Free(t *testing.T) {
	// :9001 may be free or in-use; we exercise the success path on a
	// disposable port by overriding the function's bound address would
	// require restructuring. Instead exercise the helper layer directly.
	free, err := tryBind("127.0.0.1:0")
	if !free || err != nil {
		t.Fatalf("tryBind on ephemeral port failed: %v / err=%v", free, err)
	}
}

func TestCheckControlPlanePort_InUseDaemon(t *testing.T) {
	// Bind an ephemeral port to simulate "in use", then drive the check
	// against that address.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()

	// Drive checkControlPlanePort indirectly via a parallel helper that
	// uses our test-controlled doctorEnv. We replicate the logic surface
	// here because checkControlPlanePort hardcodes :9001 — we test the
	// composable inputs (tryBind, isAddrInUse, statusFn) instead.
	_, bindErr := tryBind(addr)
	if !isAddrInUse(bindErr) {
		t.Fatalf("expected address-in-use, got %v", bindErr)
	}

	called := false
	env := &doctorEnv{
		daemonAddr: addr,
		statusFn: func(_ context.Context, gotAddr string) (string, error) {
			called = true
			if gotAddr != addr {
				t.Fatalf("statusFn got %q, want %q", gotAddr, addr)
			}
			return "code", nil
		},
	}
	prof, err := env.statusFn(context.Background(), env.daemonAddr)
	if err != nil || prof != "code" {
		t.Fatalf("statusFn = (%q, %v)", prof, err)
	}
	if !called {
		t.Fatal("statusFn not called")
	}
}

func TestCheckProxyPort_DaemonHoldsIt(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, bindErr := tryBind(ln.Addr().String())
	if !isAddrInUse(bindErr) {
		t.Fatalf("expected address-in-use, got %v", bindErr)
	}
	r := checkProxyPortAt(ln.Addr().String(), true, false)
	if r.Status != statusOK {
		t.Errorf("status = %v, want OK; msg=%q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "vibe daemon") {
		t.Errorf("message = %q, want daemon attribution", r.Message)
	}
}

func TestCheckProxyPortAt_InUseNoDaemon_Fails(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	r := checkProxyPortAt(ln.Addr().String(), false, false)
	if r.Status != statusFail {
		t.Errorf("status = %v, want FAIL; msg=%q", r.Status, r.Message)
	}
}

// disable_proxy inverts the check: an external router holding the port is
// the healthy state, not a conflict.
func TestCheckProxyPortAt_DisabledExternalRouterHoldsIt(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	r := checkProxyPortAt(ln.Addr().String(), false, true)
	if r.Status != statusOK {
		t.Errorf("status = %v, want OK; msg=%q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "external router") {
		t.Errorf("message = %q, want external-router attribution", r.Message)
	}
}

func TestCheckProxyPortAt_DisabledButFree_Warns(t *testing.T) {
	// Grab a free port, then release it so the check sees it unbound.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	r := checkProxyPortAt(addr, false, true)
	if r.Status != statusWarn {
		t.Errorf("status = %v, want WARN (router expected but absent); msg=%q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "disable_proxy") {
		t.Errorf("message = %q, want disable_proxy mention", r.Message)
	}
}

// setDoctorFixtureDirs points the XDG dirs at a fresh temp tree and
// returns the (created) backends + profiles dirs, so checks that read
// paths.BackendsDir()/paths.ProfilesDir() see a controlled inventory.
func setDoctorFixtureDirs(t *testing.T) (backendsDir, profilesDir string) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "c"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "s"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(tmp, "r"))
	backendsDir = paths.BackendsDir()
	profilesDir = paths.ProfilesDir()
	for _, d := range []string{backendsDir, profilesDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return backendsDir, profilesDir
}

// TestCheckCommonPorts_DerivedFromDisk asserts the probe list comes from
// the on-disk backend/profile inventory: a port declared in a backend
// yaml and held by a live listener must be reported in-use, labeled with
// the declaring file.
func TestCheckCommonPorts_DerivedFromDisk(t *testing.T) {
	backendsDir, _ := setDoctorFixtureDirs(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	backend := fmt.Sprintf(`name: embed
backend:
  http_server:
    image: ghcr.io/example/embed:latest
    port: %d
`, port)
	if err := os.WriteFile(filepath.Join(backendsDir, "embed.yaml"), []byte(backend), 0o644); err != nil {
		t.Fatal(err)
	}
	r := checkCommonPorts()
	if r.Name != "common ports" {
		t.Errorf("Name = %q, want 'common ports'", r.Name)
	}
	if r.Status != statusInfo {
		t.Fatalf("status = %v, want INFO (declared port is bound); msg=%q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, fmt.Sprintf(":%d", port)) {
		t.Errorf("message missing bound port %d: %q", port, r.Message)
	}
	if !strings.Contains(r.Message, "backend embed") {
		t.Errorf("message missing declaring-file label: %q", r.Message)
	}
}

func TestLoopbackPort(t *testing.T) {
	cases := []struct {
		url  string
		want int
	}{
		{"http://127.0.0.1:8080/health", 8080},
		{"http://localhost:14001/", 14001},
		{"http://example.com:8080/", 0}, // non-loopback must not be probed
		{"http://127.0.0.1/", 0},        // no explicit port
		{"not a url", 0},
	}
	for _, c := range cases {
		if got := loopbackPort(c.url); got != c.want {
			t.Errorf("loopbackPort(%q) = %d, want %d", c.url, got, c.want)
		}
	}
}

func TestIsAddrInUse(t *testing.T) {
	if isAddrInUse(nil) {
		t.Fatal("nil should not be addr-in-use")
	}
	if !isAddrInUse(errors.New("listen tcp: bind: address already in use")) {
		t.Fatal("standard error text should match")
	}
	if isAddrInUse(errors.New("listen tcp: some other error")) {
		t.Fatal("unrelated error should not match")
	}
}

// ─── profiles ───────────────────────────────────────────────────────────────

func TestCheckProfilesAt_DirMissing(t *testing.T) {
	r := checkProfilesAt(filepath.Join(t.TempDir(), "nope"))
	if r.Status != statusWarn {
		t.Fatalf("status = %v, want WARN", r.Status)
	}
}

func TestCheckProfilesAt_EmptyDir(t *testing.T) {
	r := checkProfilesAt(t.TempDir())
	if r.Status != statusWarn {
		t.Fatalf("status = %v, want WARN", r.Status)
	}
}

func TestCheckProfilesAt_ValidAndInvalid(t *testing.T) {
	dir := t.TempDir()
	// Stub model file for the valid profile.
	model := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(model, []byte("stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	good := fmt.Sprintf(`name: testprof
backend:
  llama_server:
    path: %s
    alias: testalias
    context: 4096
frontend:
  kind: external
  write_file: /tmp/opencode.json
  template:
    foo: bar
`, model)
	if err := os.WriteFile(filepath.Join(dir, "good.yaml"), []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte("not: a: profile:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := checkProfilesAt(dir)
	if r.Status != statusWarn {
		t.Fatalf("status = %v, want WARN (mixed valid+invalid); msg=%q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "testprof") {
		t.Fatalf("message missing valid profile name: %q", r.Message)
	}
	if !strings.Contains(r.Message, "bad.yaml") {
		t.Fatalf("message missing invalid filename: %q", r.Message)
	}
}

func TestCheckProfilesAt_AllInvalid(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte("bogus: ::: bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := checkProfilesAt(dir)
	if r.Status != statusFail {
		t.Fatalf("status = %v, want FAIL", r.Status)
	}
}

// ─── backends ───────────────────────────────────────────────────────────────

// TestCheckBackends_NoneSkipped asserts the check disappears entirely for
// users who don't use the backends/ abstraction — no noise.
func TestCheckBackends_NoneSkipped(t *testing.T) {
	_, profilesDir := setDoctorFixtureDirs(t)
	if _, ok := checkBackends(profilesDir); ok {
		t.Fatal("expected ok=false when no backends are defined")
	}
}

// TestCheckBackends_InvalidAndUnreferenced covers the check's two reasons
// to exist: a broken backends/<name>.yaml (which otherwise only fails
// mid-pipeline) and a valid backend no profile references (capability-only
// target, invisible to checkProfilesAt).
func TestCheckBackends_InvalidAndUnreferenced(t *testing.T) {
	backendsDir, profilesDir := setDoctorFixtureDirs(t)
	good := `name: embed
backend:
  http_server:
    image: ghcr.io/example/embed:latest
    port: 18099
`
	if err := os.WriteFile(filepath.Join(backendsDir, "embed.yaml"), []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backendsDir, "bad.yaml"), []byte("bogus: ::: bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, ok := checkBackends(profilesDir)
	if !ok {
		t.Fatal("expected ok=true when backends exist")
	}
	if r.Status != statusWarn {
		t.Fatalf("status = %v, want WARN (invalid backend present); msg=%q", r.Status, r.Message)
	}
	for _, want := range []string{"embed", "bad", "capability-only"} {
		if !strings.Contains(r.Message, want) {
			t.Errorf("message missing %q: %q", want, r.Message)
		}
	}
}

// ─── docker ─────────────────────────────────────────────────────────────────

// TestCheckDocker_SkippedWithoutDockerProfiles asserts machines with no
// docker-using profiles see no output at all.
func TestCheckDocker_SkippedWithoutDockerProfiles(t *testing.T) {
	_, profilesDir := setDoctorFixtureDirs(t)
	env := &doctorEnv{lookPath: func(string) (string, error) {
		t.Fatal("lookPath should not be called when nothing needs docker")
		return "", nil
	}}
	if _, ok := checkDockerForProfiles(context.Background(), env, profilesDir); ok {
		t.Fatal("expected ok=false when nothing on disk needs docker")
	}
}

// TestCheckDocker_WarnWhenMissing asserts a WARN (never FAIL) naming the
// docker-needing definition when the docker CLI is absent.
func TestCheckDocker_WarnWhenMissing(t *testing.T) {
	backendsDir, profilesDir := setDoctorFixtureDirs(t)
	backend := `name: tts
backend:
  http_server:
    image: ghcr.io/example/tts:latest
    port: 18098
`
	if err := os.WriteFile(filepath.Join(backendsDir, "tts.yaml"), []byte(backend), 0o644); err != nil {
		t.Fatal(err)
	}
	env := &doctorEnv{lookPath: func(string) (string, error) { return "", errors.New("not found") }}
	r, ok := checkDockerForProfiles(context.Background(), env, profilesDir)
	if !ok {
		t.Fatal("expected ok=true when a docker-mode backend exists")
	}
	if r.Status != statusWarn {
		t.Fatalf("status = %v, want WARN", r.Status)
	}
	if !strings.Contains(r.Message, "backend tts") {
		t.Errorf("message missing the docker-needing definition: %q", r.Message)
	}
}

// TestCheckDocker_DaemonUnreachable asserts a present CLI whose daemon
// doesn't answer `docker info` still degrades to WARN with the error.
func TestCheckDocker_DaemonUnreachable(t *testing.T) {
	backendsDir, profilesDir := setDoctorFixtureDirs(t)
	backend := `name: tts
backend:
  http_server:
    image: ghcr.io/example/tts:latest
    port: 18098
`
	if err := os.WriteFile(filepath.Join(backendsDir, "tts.yaml"), []byte(backend), 0o644); err != nil {
		t.Fatal(err)
	}
	env := &doctorEnv{
		lookPath: func(string) (string, error) { return "/usr/bin/docker", nil },
		run: func(context.Context, string, ...string) ([]byte, error) {
			return nil, errors.New("cannot connect to the docker daemon")
		},
	}
	r, ok := checkDockerForProfiles(context.Background(), env, profilesDir)
	if !ok || r.Status != statusWarn {
		t.Fatalf("ok=%v status=%v, want ok=true WARN; msg=%q", ok, r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "docker daemon") {
		t.Errorf("message missing run error: %q", r.Message)
	}
}

// ─── mcps ───────────────────────────────────────────────────────────────────

func TestCheckMCPsAt_Missing(t *testing.T) {
	r := checkMCPsAt(filepath.Join(t.TempDir(), "nope"))
	if r.Status != statusInfo {
		t.Fatalf("status = %v, want INFO", r.Status)
	}
	if !strings.Contains(r.Message, "0 definitions") {
		t.Fatalf("message = %q", r.Message)
	}
}

func TestCheckMCPsAt_Counts(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a.yaml", "b.yaml", "ignored.txt"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("name: x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := checkMCPsAt(dir)
	if r.Status != statusInfo {
		t.Fatalf("status = %v, want INFO", r.Status)
	}
	if !strings.Contains(r.Message, "2 definitions") {
		t.Fatalf("message = %q", r.Message)
	}
}

// ─── gpu ────────────────────────────────────────────────────────────────────

func TestCheckGPU_NoNvidiaSmi(t *testing.T) {
	env := &doctorEnv{lookPath: func(string) (string, error) { return "", errors.New("x") }}
	r := checkGPU(context.Background(), env)
	if r.Status != statusInfo {
		t.Fatalf("status = %v, want INFO", r.Status)
	}
}

func TestCheckGPU_Output(t *testing.T) {
	env := &doctorEnv{
		lookPath: func(string) (string, error) { return "/usr/bin/nvidia-smi", nil },
		run: func(context.Context, string, ...string) ([]byte, error) {
			return []byte("NVIDIA GeForce RTX 5090, 30447 MiB\n"), nil
		},
	}
	r := checkGPU(context.Background(), env)
	if r.Status != statusInfo {
		t.Fatalf("status = %v, want INFO", r.Status)
	}
	if !strings.Contains(r.Message, "RTX 5090") {
		t.Fatalf("message = %q", r.Message)
	}
}

func TestCheckGPU_RunFails(t *testing.T) {
	env := &doctorEnv{
		lookPath: func(string) (string, error) { return "/usr/bin/nvidia-smi", nil },
		run: func(context.Context, string, ...string) ([]byte, error) {
			return nil, errors.New("driver not loaded")
		},
	}
	r := checkGPU(context.Background(), env)
	if r.Status != statusWarn {
		t.Fatalf("status = %v, want WARN", r.Status)
	}
}

// ─── helpers ────────────────────────────────────────────────────────────────

func TestFirstNonEmptyLine(t *testing.T) {
	if got := firstNonEmptyLine([]byte("\n\nhello\nworld\n")); got != "hello" {
		t.Fatalf("got %q", got)
	}
	if got := firstNonEmptyLine([]byte("")); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestNonEmptyLines(t *testing.T) {
	got := nonEmptyLines([]byte("a\n\nb\n c \n"))
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("got %v", got)
	}
}

func TestStatusTag(t *testing.T) {
	cases := []struct {
		s    checkStatus
		want string
	}{
		{statusOK, "[ OK ]"},
		{statusWarn, "[WARN]"},
		{statusFail, "[FAIL]"},
		{statusInfo, "[INFO]"},
	}
	for _, c := range cases {
		if got := c.s.tag(); got != c.want {
			t.Fatalf("tag(%d) = %q, want %q", c.s, got, c.want)
		}
	}
}

// ─── rsvg-convert (vision) ──────────────────────────────────────────────────

// TestCheckRSVG_NoMMProjProfiles asserts the check returns ok=false when
// no installed profile declares an mmproj path; we don't want a warning
// for users who only run text-only profiles.
func TestCheckRSVG_NoMMProjProfiles(t *testing.T) {
	dir := t.TempDir()
	// Text-only profile (no mmproj).
	model := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(model, []byte("stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	textProfile := fmt.Sprintf(`name: textonly
backend:
  llama_server:
    path: %s
    alias: t
    context: 1024
frontend:
  kind: external
  write_file: /tmp/x
  template:
    a: 1
`, model)
	if err := os.WriteFile(filepath.Join(dir, "text.yaml"), []byte(textProfile), 0o644); err != nil {
		t.Fatal(err)
	}
	env := &doctorEnv{lookPath: func(string) (string, error) { return "", errors.New("absent") }}
	_, ok := checkRSVGForVision(env, dir)
	if ok {
		t.Fatal("expected ok=false when no mmproj profile installed")
	}
}

// TestCheckRSVG_MissingWithMMProj asserts WARN when an mmproj profile
// exists but rsvg-convert is unavailable. This is the case the user
// explicitly requested coverage for.
func TestCheckRSVG_MissingWithMMProj(t *testing.T) {
	dir := t.TempDir()
	model := filepath.Join(dir, "model.gguf")
	mmproj := filepath.Join(dir, "mmproj.gguf")
	for _, p := range []string{model, mmproj} {
		if err := os.WriteFile(p, []byte("stub"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	visionProfile := fmt.Sprintf(`name: visionprof
backend:
  llama_server:
    path: %s
    mmproj: %s
    alias: v
    context: 1024
frontend:
  kind: external
  write_file: /tmp/x
  template:
    a: 1
`, model, mmproj)
	if err := os.WriteFile(filepath.Join(dir, "vision.yaml"), []byte(visionProfile), 0o644); err != nil {
		t.Fatal(err)
	}
	env := &doctorEnv{lookPath: func(name string) (string, error) {
		if name == "rsvg-convert" {
			return "", errors.New("not found")
		}
		t.Fatalf("unexpected lookup of %q", name)
		return "", nil
	}}
	r, ok := checkRSVGForVision(env, dir)
	if !ok {
		t.Fatal("expected ok=true when mmproj profile present")
	}
	if r.Status != statusWarn {
		t.Fatalf("status = %v, want WARN", r.Status)
	}
	if !strings.Contains(r.Message, "visionprof") {
		t.Errorf("message missing profile name: %q", r.Message)
	}
	if !strings.Contains(r.Message, "librsvg2-bin") {
		t.Errorf("message missing install hint: %q", r.Message)
	}
}

// TestCheckRSVG_PresentWithMMProj asserts OK when both rsvg-convert and
// an mmproj profile are present. Keeps the success path covered so a
// regression to "always warn" would fail.
func TestCheckRSVG_PresentWithMMProj(t *testing.T) {
	dir := t.TempDir()
	model := filepath.Join(dir, "model.gguf")
	mmproj := filepath.Join(dir, "mmproj.gguf")
	for _, p := range []string{model, mmproj} {
		if err := os.WriteFile(p, []byte("stub"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	visionProfile := fmt.Sprintf(`name: visionprof
backend:
  llama_server:
    path: %s
    mmproj: %s
    alias: v
    context: 1024
frontend:
  kind: external
  write_file: /tmp/x
  template:
    a: 1
`, model, mmproj)
	if err := os.WriteFile(filepath.Join(dir, "vision.yaml"), []byte(visionProfile), 0o644); err != nil {
		t.Fatal(err)
	}
	env := &doctorEnv{lookPath: func(name string) (string, error) {
		if name == "rsvg-convert" {
			return "/usr/bin/rsvg-convert", nil
		}
		return "", errors.New("absent")
	}}
	r, ok := checkRSVGForVision(env, dir)
	if !ok {
		t.Fatal("expected ok=true when mmproj profile present")
	}
	if r.Status != statusOK {
		t.Fatalf("status = %v, want OK; msg=%q", r.Status, r.Message)
	}
}

// TestCheckRSVG_MissingProfilesDir guards against a runtime panic when
// $XDG_CONFIG_HOME/vibe/profiles doesn't exist yet (first-run users).
func TestCheckRSVG_MissingProfilesDir(t *testing.T) {
	env := &doctorEnv{lookPath: func(string) (string, error) { return "", errors.New("absent") }}
	_, ok := checkRSVGForVision(env, filepath.Join(t.TempDir(), "nope"))
	if ok {
		t.Fatal("expected ok=false when profiles dir missing")
	}
}

// ─── end-to-end: doctorCmd registers and runs without panicking ─────────────

func TestRunChecks_Smoke(t *testing.T) {
	// Drive runChecks against a tightly stubbed env so the test doesn't
	// depend on host binaries or :9001.
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "c"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "s"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(tmp, "r"))
	env := &doctorEnv{
		lookPath: func(name string) (string, error) {
			return "", errors.New("missing: " + name)
		},
		run: func(context.Context, string, ...string) ([]byte, error) {
			return nil, errors.New("not invoked in test")
		},
		statusFn: func(context.Context, string) (string, error) {
			return "", errors.New("no daemon")
		},
		daemonAddr: "127.0.0.1:9001",
	}
	results := runChecks(context.Background(), env)
	if len(results) == 0 {
		t.Fatal("no results returned")
	}
	// Every result must have a Name; statuses must be one of the known
	// values.
	for _, r := range results {
		if r.Name == "" {
			t.Fatalf("result with empty name: %+v", r)
		}
		switch r.Status {
		case statusOK, statusWarn, statusFail, statusInfo:
		default:
			t.Fatalf("unknown status in result %+v", r)
		}
	}
}
