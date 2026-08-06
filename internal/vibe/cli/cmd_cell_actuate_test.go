package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	vibev1 "github.com/gallowaysoftware/vibe/proto/vibe/v1"
	"github.com/gallowaysoftware/vibe/proto/vibe/v1/vibev1connect"
)

// cellVerbFake records drain/resume RPCs over the real Connect handler,
// optionally serving from a unix socket (the local-daemon path).
type cellVerbFake struct {
	mu          sync.Mutex
	drains      int
	resumes     int
	suspends    int
	lastReason  string
	lastRequire bool
	failDrain   bool
	failSuspend bool
}

func (f *cellVerbFake) CellDrain(_ context.Context, req *connect.Request[vibev1.CellDrainRequest]) (*connect.Response[vibev1.CellDrainResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drains++
	f.lastReason = req.Msg.Reason
	if f.failDrain {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("drain command failed: exit 1"))
	}
	return connect.NewResponse(&vibev1.CellDrainResponse{ResidentModels: []string{"qwen"}}), nil
}

func (f *cellVerbFake) CellResume(_ context.Context, _ *connect.Request[vibev1.CellResumeRequest]) (*connect.Response[vibev1.CellResumeResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumes++
	return connect.NewResponse(&vibev1.CellResumeResponse{}), nil
}

func (f *cellVerbFake) CellSuspend(_ context.Context, req *connect.Request[vibev1.CellSuspendRequest]) (*connect.Response[vibev1.CellSuspendResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.suspends++
	f.lastReason = req.Msg.Reason
	f.lastRequire = req.Msg.RequireIdle
	if f.failSuspend {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("2 request(s) in flight; not suspending"))
	}
	idle := fleetapi.SuspendIdleNotRequired
	if req.Msg.RequireIdle {
		idle = fleetapi.SuspendIdleVerified
	}
	return connect.NewResponse(&vibev1.CellSuspendResponse{ResidentModels: []string{"qwen"}, IdleStatus: idle}), nil
}

func (f *cellVerbFake) Status(context.Context, *connect.Request[vibev1.StatusRequest]) (*connect.Response[vibev1.StatusResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("nope"))
}
func (f *cellVerbFake) ListProfiles(context.Context, *connect.Request[vibev1.ListProfilesRequest]) (*connect.Response[vibev1.ListProfilesResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("nope"))
}
func (f *cellVerbFake) Start(context.Context, *connect.Request[vibev1.StartRequest]) (*connect.Response[vibev1.StartResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("nope"))
}
func (f *cellVerbFake) Stop(context.Context, *connect.Request[vibev1.StopRequest]) (*connect.Response[vibev1.StopResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("nope"))
}
func (f *cellVerbFake) Shutdown(context.Context, *connect.Request[vibev1.ShutdownRequest]) (*connect.Response[vibev1.ShutdownResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("nope"))
}
func (f *cellVerbFake) Logs(context.Context, *connect.Request[vibev1.LogsRequest]) (*connect.Response[vibev1.LogsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("nope"))
}
func (f *cellVerbFake) Pull(context.Context, *connect.Request[vibev1.PullRequest], *connect.ServerStream[vibev1.PullProgress]) error {
	return connect.NewError(connect.CodeUnimplemented, fmt.Errorf("nope"))
}

// serveCellVerbsOnUnix starts the Connect handler on vibe's unix socket
// path inside a temp XDG_RUNTIME_DIR (the local-daemon drain path).
func serveCellVerbsOnUnix(t *testing.T, f *cellVerbFake) {
	t.Helper()
	runtime := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtime)
	if err := os.MkdirAll(filepath.Join(runtime, "vibe"), 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(runtime, "vibe", "vibe.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	path, handler := vibev1connect.NewControlServiceHandler(f)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, path) {
			handler.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
}

// serveCellVerbsTCP starts the handler on httptest (the remote-cell path).
func serveCellVerbsTCP(t *testing.T, f *cellVerbFake) *httptest.Server {
	t.Helper()
	path, handler := vibev1connect.NewControlServiceHandler(f)
	ts := httptest.NewServer(handler)
	_ = path
	t.Cleanup(ts.Close)
	return ts
}

func writeHosts(t *testing.T, body string) {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if err := os.MkdirAll(filepath.Join(xdg, "vibe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(xdg, "vibe", "hosts.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestParseDrainArgs(t *testing.T) {
	cases := []struct {
		args    []string
		dash    int
		cell    string
		command []string
	}{
		{[]string{}, -1, "", nil},
		{[]string{"gpu-cell"}, -1, "gpu-cell", nil},
		{[]string{"bash", "-c", "x"}, 0, "", []string{"bash", "-c", "x"}},
		{[]string{"gpu-cell", "bash", "-c", "x"}, 1, "gpu-cell", []string{"bash", "-c", "x"}},
	}
	for _, tc := range cases {
		cell, command := parseDrainArgs(tc.args, tc.dash)
		if cell != tc.cell {
			t.Errorf("args=%v dash=%d: cell = %q, want %q", tc.args, tc.dash, cell, tc.cell)
		}
		if strings.Join(command, " ") != strings.Join(tc.command, " ") {
			t.Errorf("args=%v dash=%d: command = %v, want %v", tc.args, tc.dash, command, tc.command)
		}
	}
}

func TestCellDrainLocalUnix(t *testing.T) {
	fake := &cellVerbFake{}
	serveCellVerbsOnUnix(t, fake)
	// No leases configured anywhere — fleetd resolution will fail, which
	// the lease pre-check tolerates (best-effort).
	var out bytes.Buffer
	if err := drainCell(t.Context(), &out, "", "gaming", "23:00", true, 0); err != nil {
		t.Fatal(err)
	}
	if fake.drains != 1 || fake.lastReason != "gaming" {
		t.Errorf("drains=%d reason=%q", fake.drains, fake.lastReason)
	}
	if !strings.Contains(out.String(), "drained") || !strings.Contains(out.String(), "qwen") {
		t.Errorf("out = %q", out.String())
	}
}

// cliIntentSink records intent POSTs (fleetd's role) for the remote
// drain/resume paths.
type cliIntentSink struct {
	srv   *httptest.Server
	mu    sync.Mutex
	posts []map[string]any
}

func newCLIIntentSink(t *testing.T) *cliIntentSink {
	t.Helper()
	s := &cliIntentSink{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/fleet/intent", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.posts = append(s.posts, body)
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"state": "ok"})
	})
	mux.HandleFunc("GET /api/fleet/leases", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"leases":[]}`))
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *cliIntentSink) calls() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any{}, s.posts...)
}

func TestCellDrainRemoteWritesIntentAtFleetd(t *testing.T) {
	// The transport split writes nothing for TCP callers, and the CLI is
	// not fleetd — so a CLI-driven remote drain must POST intent itself,
	// or the fleet never learns why (review finding C2-SEC-002).
	sink := newCLIIntentSink(t)
	fake := &cellVerbFake{}
	cellSrv := serveCellVerbsTCP(t, fake)
	writeHosts(t, fmt.Sprintf(`
fleetd_url: "%s"
cells:
  front:    { url: "http://127.0.0.1:1", class: always_on }
  gpu-cell: { url: "http://127.0.0.1:1", class: opportunistic, daemon_url: "%s" }
`, sink.srv.URL, cellSrv.URL))

	var out bytes.Buffer
	if err := drainCell(t.Context(), &out, "gpu-cell", "gaming", "23:00", true, 0); err != nil {
		t.Fatal(err)
	}
	got := sink.calls()
	if len(got) != 1 || got[0]["cell"] != "gpu-cell" || got[0]["state"] != "drained" || got[0]["reason"] != "gaming" || got[0]["eta"] != "23:00" {
		t.Errorf("intent posts = %v, want one drained/gaming/23:00", got)
	}

	// And remote resume clears it.
	client, _, err := resolveCellClient("gpu-cell")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.CellResume(t.Context()); err != nil {
		t.Fatal(err)
	}
	postCLIIntent(&out, "gpu-cell", "serving", "", "")
	got = sink.calls()
	if len(got) != 2 || got[1]["state"] != "serving" {
		t.Errorf("after resume: posts = %v, want a serving post", got)
	}
}

func TestDrainUntilExitResumesOnContextCancel(t *testing.T) {
	// The signal path, testable: a canceled parent ctx (what SIGINT
	// does via NotifyContext) kills the child, and the deferred resume
	// must still fire.
	fake := &cellVerbFake{}
	serveCellVerbsOnUnix(t, fake)
	ctx, cancel := context.WithCancel(context.Background())
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- drainUntilExit(ctx, &out, "", "gaming", "", true, 0, []string{"sleep", "30"})
	}()
	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("wrapper did not exit after cancel")
	}
	if fake.drains != 1 || fake.resumes != 1 {
		t.Errorf("drains=%d resumes=%d — resume must fire when the wrapper is signaled", fake.drains, fake.resumes)
	}
}

func TestCellDrainRemoteCell(t *testing.T) {
	fake := &cellVerbFake{}
	ts := serveCellVerbsTCP(t, fake)
	tokFile := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(tokFile, []byte("tok123"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeHosts(t, fmt.Sprintf(`
fleetd_url: "http://127.0.0.1:1"
cells:
  front:    { url: "http://127.0.0.1:1", class: always_on }
  gpu-cell: { url: "http://127.0.0.1:1", class: opportunistic,
              daemon_url: "%s", token_file: "%s" }
`, ts.URL, tokFile))

	var out bytes.Buffer
	if err := drainCell(t.Context(), &out, "gpu-cell", "gaming", "", true, 0); err != nil {
		t.Fatal(err)
	}
	if fake.drains != 1 {
		t.Errorf("drains = %d", fake.drains)
	}
}

func TestCellDrainUnknownCell(t *testing.T) {
	writeHosts(t, `
cells:
  front: { url: "http://127.0.0.1:1", class: always_on }
`)
	var out bytes.Buffer
	err := drainCell(t.Context(), &out, "nope", "", "", true, 0)
	if err == nil || !strings.Contains(err.Error(), "unknown cell") {
		t.Errorf("got %v, want unknown cell", err)
	}
}

func TestCellDrainLeasePromptRequiresYesOffTTY(t *testing.T) {
	// Fleetd serving an active lease; stdin is not a TTY in tests, so
	// without --yes the drain must refuse rather than hang.
	leaseFleetd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"leases":[{"cell":"gpu-cell","model":"qwen","holder":"batch","note":"mid-batch","expires_at":"2099-01-01T00:00:00Z"}]}`))
	}))
	t.Cleanup(leaseFleetd.Close)
	fake := &cellVerbFake{}
	ts := serveCellVerbsTCP(t, fake)
	writeHosts(t, fmt.Sprintf(`
fleetd_url: "%s"
cells:
  front:    { url: "http://127.0.0.1:1", class: always_on }
  gpu-cell: { url: "http://127.0.0.1:1", class: opportunistic, daemon_url: "%s" }
`, leaseFleetd.URL, ts.URL))

	var out bytes.Buffer
	err := drainCell(t.Context(), &out, "gpu-cell", "", "", false, 0)
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("got %v, want the --yes refusal", err)
	}
	if fake.drains != 0 {
		t.Error("drain RPC fired despite the refused prompt")
	}
	if !strings.Contains(out.String(), "batch") {
		t.Errorf("lease not shown before the refusal: %q", out.String())
	}

	// --yes proceeds.
	out.Reset()
	if err := drainCell(t.Context(), &out, "gpu-cell", "", "", true, 0); err != nil {
		t.Fatal(err)
	}
	if fake.drains != 1 {
		t.Error("--yes drain did not fire")
	}
}

func TestCellResumeLocal(t *testing.T) {
	fake := &cellVerbFake{}
	serveCellVerbsOnUnix(t, fake)
	client, name, err := resolveCellClient("")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.CellResume(t.Context()); err != nil {
		t.Fatal(err)
	}
	if fake.resumes != 1 || name != "" {
		t.Errorf("resumes=%d name=%q", fake.resumes, name)
	}
}

func TestDrainUntilExitResumesOnSuccessAndFailure(t *testing.T) {
	for _, succeed := range []bool{true, false} {
		fake := &cellVerbFake{}
		serveCellVerbsOnUnix(t, fake)
		var out bytes.Buffer
		command := []string{"/bin/true"}
		if !succeed {
			command = []string{"/bin/false"}
		}
		err := drainUntilExit(t.Context(), &out, "", "gaming", "", true, 0, command)
		if succeed && err != nil {
			t.Errorf("success case: %v", err)
		}
		if !succeed && err == nil {
			t.Error("failure case: want non-nil error from the wrapped command")
		}
		if fake.drains != 1 || fake.resumes != 1 {
			t.Errorf("succeed=%v: drains=%d resumes=%d — resume must fire on ANY exit", succeed, fake.drains, fake.resumes)
		}
		if !strings.Contains(out.String(), "resumed") {
			t.Errorf("out = %q", out.String())
		}
	}
}

func TestWakeCellViaFleetd(t *testing.T) {
	// A real fleetapi server with a wake-configured cell, exercised
	// through wakeCell's fleetd path.
	dir := t.TempDir()
	ln, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	fleet := fleetapi.New([]fleetapi.Cell{{
		Name: "gpu-cell", URL: "http://127.0.0.1:1",
		Wake: &fleetapi.WakeSpec{MAC: "aa:bb:cc:dd:ee:ff", Broadcast: ln.LocalAddr().String()},
	}}, filepath.Join(dir, "h.json"), func() fleetapi.DaemonInfo { return fleetapi.DaemonInfo{} },
		fleetapi.Options{IntentPath: filepath.Join(dir, "intent.json")})
	t.Cleanup(fleet.Close)
	mux := http.NewServeMux()
	fleet.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	var out bytes.Buffer
	if err := wakeCell(t.Context(), &out, ts.URL, "gpu-cell"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "packet") {
		t.Errorf("out = %q", out.String())
	}
	_ = ln.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1024)
	n, _, err := ln.ReadFromUDP(buf)
	if err != nil || n != 102 {
		t.Errorf("packet: n=%d err=%v", n, err)
	}
}

// TestPrintDrainReport_WaitStatus pins MIN-N's human-facing half: the
// operator who asked for quiescence sees whether it happened.
func TestPrintDrainReport_WaitStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status *string
		want   string
		absent string
	}{
		{name: "skipped", status: strPtr(fleetapi.DrainWaitSkippedNoInflight), want: "--wait was SKIPPED"},
		// The status has TWO producers — never reported, and reported then
		// stopped mid-wait (C20) — so the sentence may not assert either
		// one. "never reported in-flight counts" sends the operator to
		// debug an events stream when what happened is that they cancelled
		// a running generation.
		{name: "skipped says no evidence, not `never`", status: strPtr(fleetapi.DrainWaitSkippedNoInflight), want: "no in-flight evidence", absent: "never reported in-flight counts"},
		{name: "waited", status: strPtr(fleetapi.DrainWaitWaited), want: "waited for in-flight requests"},
		{name: "not requested", status: strPtr(fleetapi.DrainWaitNotRequested), absent: "wait"},
		{name: "absent field (old daemon)", status: nil, absent: "wait"},
		// A newer daemon's vocabulary must not render as silence, which an
		// operator reads as "the wait happened".
		{name: "unknown status from a newer daemon", status: strPtr("skipped_evidence_lost"), want: "unrecognised status"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			printDrainReport(&out, "gpu-cell", &vibev1.CellDrainResponse{WaitStatus: tc.status})
			s := out.String()
			if tc.want != "" && !strings.Contains(s, tc.want) {
				t.Errorf("output %q missing %q", s, tc.want)
			}
			if tc.absent != "" && strings.Contains(s, tc.absent) {
				t.Errorf("output %q mentions %q when it should say nothing", s, tc.absent)
			}
		})
	}
}

func strPtr(s string) *string { return &s }

// TestResolveCellClientTokenPrecedence pins MIN-P and the deliberate
// divergence from fleetd (NIT-E): for a human typing one command an
// explicit $VIBE_TOKEN wins, and an unreadable token_file is a hard
// error — the old `err == nil` swallow turned a typo'd path into an
// opaque 401 from the remote cell.
func TestResolveCellClientTokenPrecedence(t *testing.T) {
	dir := t.TempDir()
	tok := filepath.Join(dir, "cell-token")
	if err := os.WriteFile(tok, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	authCh := make(chan string, 4)
	cellDaemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authCh <- r.Header.Get("Authorization")
		http.Error(w, "no", http.StatusNotFound)
	}))
	t.Cleanup(cellDaemon.Close)
	writeHosts(t, fmt.Sprintf(`
cells:
  front:    { url: "http://127.0.0.1:1", class: always_on }
  gpu-cell: { url: "http://127.0.0.1:1", class: opportunistic,
              daemon_url: "%s", token_file: "%s" }
  bad-cell: { url: "http://127.0.0.1:1", class: opportunistic,
              daemon_url: "%s", token_file: "%s" }
`, cellDaemon.URL, tok, cellDaemon.URL, filepath.Join(dir, "missing")))

	// The CLI's order: an explicit override wins over the file.
	t.Setenv("VIBE_TOKEN", "from-env")
	c, _, err := resolveCellClient("gpu-cell")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.Status(t.Context())
	select {
	case got := <-authCh:
		if got != "Bearer from-env" {
			t.Errorf("Authorization = %q, want $VIBE_TOKEN to win for a human command", got)
		}
	default:
		t.Fatal("the cell daemon saw no request")
	}

	// Without the override the file is used, and an unreadable one is a
	// named error rather than a silent fall-through to the local token.
	t.Setenv("VIBE_TOKEN", "")
	c, _, err = resolveCellClient("gpu-cell")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.Status(t.Context())
	select {
	case got := <-authCh:
		if got != "Bearer from-file" {
			t.Errorf("Authorization = %q, want the per-cell token_file", got)
		}
	default:
		t.Fatal("the cell daemon saw no request")
	}
	if _, _, err := resolveCellClient("bad-cell"); err == nil || !strings.Contains(err.Error(), "token_file") {
		t.Errorf("unreadable token_file: got %v, want a typed error naming it", err)
	}
}
