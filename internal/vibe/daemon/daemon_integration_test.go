package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/daemon"
	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
	"github.com/gallowaysoftware/vibe/internal/vibeclient"
	vibev1 "github.com/gallowaysoftware/vibe/proto/vibe/v1"
)

var fakeBinary string

func TestMain(m *testing.M) {
	bin := filepath.Join(os.TempDir(), fmt.Sprintf("vibe-fake-llama-daemon-%d", os.Getpid()))
	cmd := exec.Command("go", "build", "-o", bin, "../../../testdata/fake-llama-server")
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintln(os.Stderr, "build fake:", err, string(out))
		os.Exit(1)
	}
	fakeBinary = bin
	code := m.Run()
	_ = os.Remove(bin)
	os.Exit(code)
}

// pickFreePort returns a localhost port that's free at the moment of the call.
func pickFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// setupXDG points all vibe paths at fresh temp directories, isolating tests
// from each other and from the user's real config.
func setupXDG(t *testing.T) (cfgHome, stateHome, runtimeDir string) {
	t.Helper()
	cfgHome = t.TempDir()
	stateHome = t.TempDir()
	runtimeDir = t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	if err := paths.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	return
}

// writeProfile drops a profile YAML in the active XDG profiles dir.
func writeProfile(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(paths.ProfilesDir(), name+".yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write profile: %v", err)
	}
	return path
}

// stubModel creates a non-empty file the profile validator will accept as a
// local model path.
func stubModel(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatalf("write model: %v", err)
	}
	return p
}

// externalProfile returns YAML for a minimal `external`-kind profile that
// writes a sidecar file at <stateDir>/frontend/<name>/sidecar.json.
func externalProfile(name, modelPath string) string {
	return fmt.Sprintf(`name: %s
description: %s integration profile
model:
  path: %s
  alias: fake
  context: 1024
  parallel: 1
frontend:
  kind: external
  app: stub
  write_file: ${VIBE_STATE_DIR}/frontend/%s/sidecar.json
  env:
    STUB_CONFIG: ${WRITE_FILE}
  template:
    api: ${VIBE_API}
    alias: ${MODEL_ALIAS}
`, name, name, modelPath, name)
}

// runHandle gives a test access to the daemon's Run() return value while
// letting t.Cleanup tear the daemon down even if the test never reads the
// error itself.
type runHandle struct {
	cancel context.CancelFunc
	done   chan struct{} // closed after Run returns
	mu     sync.Mutex
	err    error
}

func (h *runHandle) wait(timeout time.Duration) (error, bool) {
	select {
	case <-h.done:
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.err, true
	case <-time.After(timeout):
		return nil, false
	}
}

// startDaemon launches d.Run in a goroutine. It returns a vibeclient pointing
// at the daemon's unix socket and a handle the caller can use to await Run.
func startDaemon(t *testing.T, d *daemon.Daemon) (*vibeclient.Client, *runHandle) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h := &runHandle{cancel: cancel, done: make(chan struct{})}
	go func() {
		err := d.Run(ctx)
		h.mu.Lock()
		h.err = err
		h.mu.Unlock()
		close(h.done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(5 * time.Second):
			t.Log("daemon Run did not return within 5s of cancel")
		}
	})

	client := unixClient(t)
	// Poll until daemon is up.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ctxPing, cancelPing := context.WithTimeout(context.Background(), 200*time.Millisecond)
		_, err := client.Status(ctxPing)
		cancelPing()
		if err == nil {
			return client, h
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("daemon did not become reachable on unix socket %s within 5s", paths.Socket())
	return nil, nil
}

func unixClient(t *testing.T) *vibeclient.Client {
	t.Helper()
	sock := paths.Socket()
	hc := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}
	return vibeclient.NewWithHTTPClient("http://vibe.local", hc)
}

func makeDaemon(t *testing.T) *daemon.Daemon {
	t.Helper()
	cfg := daemon.Config{
		ProxyPort:   pickFreePort(t),
		HTTPAddr:    fmt.Sprintf("127.0.0.1:%d", pickFreePort(t)),
		LlamaBinary: fakeBinary,
	}
	return daemon.New(cfg)
}

func TestDaemon_StartStopFlow(t *testing.T) {
	setupXDG(t)
	stub := stubModel(t)
	writeProfile(t, "code", externalProfile("code", stub))

	d := makeDaemon(t)
	client, _ := startDaemon(t, d)

	startCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := client.Start(startCtx, "code")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Status == nil || !res.Status.Running || !res.Status.Ready {
		t.Fatalf("status after Start = %+v; want running+ready", res.Status)
	}
	if res.Status.Profile != "code" {
		t.Errorf("profile = %q, want code", res.Status.Profile)
	}

	s, err := client.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !s.Running || !s.Ready {
		t.Errorf("status %+v; want running+ready", s)
	}

	if _, err := client.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	s, err = client.Status(context.Background())
	if err != nil {
		t.Fatalf("post-Stop Status: %v", err)
	}
	if s.Running {
		t.Errorf("still running after Stop: %+v", s)
	}
}

func TestDaemon_StartActivatesFrontend(t *testing.T) {
	_, stateHome, _ := setupXDG(t)
	stub := stubModel(t)
	writeProfile(t, "code", externalProfile("code", stub))

	d := makeDaemon(t)
	client, _ := startDaemon(t, d)

	startCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := client.Start(startCtx, "code")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _, _ = client.Stop(context.Background()) })

	wantPath := filepath.Join(stateHome, "vibe", "frontend", "code", "sidecar.json")
	if res.Frontend == nil {
		t.Fatalf("frontend info nil")
	}
	if res.Frontend.WroteFile != wantPath {
		t.Errorf("wrote_file = %q, want %q", res.Frontend.WroteFile, wantPath)
	}

	data, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	var sidecar map[string]any
	if err := json.Unmarshal(data, &sidecar); err != nil {
		t.Fatalf("unmarshal sidecar: %v\nbody: %s", err, string(data))
	}
	if sidecar["alias"] != "fake" {
		t.Errorf("sidecar alias = %v, want fake", sidecar["alias"])
	}
	api, ok := sidecar["api"].(string)
	if !ok || !strings.HasPrefix(api, "http://127.0.0.1:") || !strings.HasSuffix(api, "/v1") {
		t.Errorf("sidecar api = %v, want http://127.0.0.1:<port>/v1", sidecar["api"])
	}
}

func TestDaemon_StartConflict(t *testing.T) {
	setupXDG(t)
	stub := stubModel(t)
	writeProfile(t, "code", externalProfile("code", stub))

	d := makeDaemon(t)
	client, _ := startDaemon(t, d)

	startCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.Start(startCtx, "code"); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	t.Cleanup(func() { _, _ = client.Stop(context.Background()) })

	_, err := client.Start(context.Background(), "code")
	if err == nil {
		t.Fatalf("second Start: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "already_exists") && !strings.Contains(err.Error(), "AlreadyExists") {
		t.Errorf("err = %v; want CodeAlreadyExists", err)
	}
}

func TestDaemon_ListProfiles(t *testing.T) {
	setupXDG(t)
	stub := stubModel(t)
	writeProfile(t, "code", externalProfile("code", stub))
	writeProfile(t, "fast", externalProfile("fast", stub))

	d := makeDaemon(t)
	client, _ := startDaemon(t, d)

	profs, err := client.ListProfiles(context.Background())
	if err != nil {
		t.Fatalf("ListProfiles: %v", err)
	}
	if len(profs) != 2 {
		t.Fatalf("got %d profiles, want 2: %+v", len(profs), profs)
	}
	if profs[0].Name != "code" || profs[1].Name != "fast" {
		t.Errorf("profile order = [%s, %s], want [code, fast]", profs[0].Name, profs[1].Name)
	}
}

func TestDaemon_Shutdown(t *testing.T) {
	setupXDG(t)

	d := makeDaemon(t)
	client, h := startDaemon(t, d)

	if err := client.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown rpc: %v", err)
	}

	err, done := h.wait(3 * time.Second)
	if !done {
		t.Fatalf("Run did not return within 3s of Shutdown rpc")
	}
	if err != nil {
		t.Errorf("Run returned err = %v; want nil", err)
	}
}

func TestDaemon_PullForLocalFile(t *testing.T) {
	setupXDG(t)
	stub := stubModel(t)
	writeProfile(t, "code", externalProfile("code", stub))

	d := makeDaemon(t)
	client, _ := startDaemon(t, d)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.Pull(ctx, "code")
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	defer stream.Close()

	var last *vibev1.PullProgress
	for stream.Receive() {
		last = stream.Msg()
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream err: %v", err)
	}
	if last == nil {
		t.Fatalf("no progress messages received")
	}
	if last.Phase != vibev1.PullProgress_PHASE_DONE {
		t.Errorf("phase = %v, want DONE", last.Phase)
	}
	if !strings.Contains(last.Message, "no huggingface block") {
		t.Errorf("message = %q, want \"no huggingface block; nothing to pull\"", last.Message)
	}
}

