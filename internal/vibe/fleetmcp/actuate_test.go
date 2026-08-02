package fleetmcp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	vibev1 "github.com/gallowaysoftware/vibe/proto/vibe/v1"
	"github.com/gallowaysoftware/vibe/proto/vibe/v1/vibev1connect"
)

// fakeCellDaemon serves the real Connect handler surface the actuation
// tools call, backed by a stub implementation. failDrain scripts an
// Unavailable outcome.
type fakeCellDaemon struct {
	srv *httptest.Server

	mu          sync.Mutex
	drainCalls  int
	resumeCalls int
	failDrain   bool
	drainDelay  time.Duration
}

func (f *fakeCellDaemon) CellDrain(ctx context.Context, req *connect.Request[vibev1.CellDrainRequest]) (*connect.Response[vibev1.CellDrainResponse], error) {
	f.mu.Lock()
	f.drainCalls++
	delay := f.drainDelay
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.failDrain {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("drain command failed: exit 1: unit not found"))
	}
	two := int64(2)
	return connect.NewResponse(&vibev1.CellDrainResponse{
		ResidentModels:   []string{"qwen3.6-27b"},
		InFlightRequests: &two,
		ActiveLeases: []*vibev1.LeaseView{
			{Cell: "gpu-cell", Model: "bge-embed", Holder: "batch-ingest", Note: "mid-batch", ExpiresAt: timestamppb.New(time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC))},
		},
	}), nil
}

func (f *fakeCellDaemon) CellResume(_ context.Context, _ *connect.Request[vibev1.CellResumeRequest]) (*connect.Response[vibev1.CellResumeResponse], error) {
	f.mu.Lock()
	f.resumeCalls++
	f.mu.Unlock()
	return connect.NewResponse(&vibev1.CellResumeResponse{}), nil
}

// The remaining RPCs are irrelevant to these tests; return unimplemented.
func (f *fakeCellDaemon) Status(context.Context, *connect.Request[vibev1.StatusRequest]) (*connect.Response[vibev1.StatusResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("nope"))
}
func (f *fakeCellDaemon) ListProfiles(context.Context, *connect.Request[vibev1.ListProfilesRequest]) (*connect.Response[vibev1.ListProfilesResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("nope"))
}
func (f *fakeCellDaemon) Start(context.Context, *connect.Request[vibev1.StartRequest]) (*connect.Response[vibev1.StartResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("nope"))
}
func (f *fakeCellDaemon) Stop(context.Context, *connect.Request[vibev1.StopRequest]) (*connect.Response[vibev1.StopResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("nope"))
}
func (f *fakeCellDaemon) Shutdown(context.Context, *connect.Request[vibev1.ShutdownRequest]) (*connect.Response[vibev1.ShutdownResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("nope"))
}
func (f *fakeCellDaemon) Logs(context.Context, *connect.Request[vibev1.LogsRequest]) (*connect.Response[vibev1.LogsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("nope"))
}
func (f *fakeCellDaemon) Pull(context.Context, *connect.Request[vibev1.PullRequest], *connect.ServerStream[vibev1.PullProgress]) error {
	return connect.NewError(connect.CodeUnimplemented, fmt.Errorf("nope"))
}

func newFakeCellDaemon(t *testing.T) *fakeCellDaemon {
	t.Helper()
	f := &fakeCellDaemon{}
	path, handler := vibev1connect.NewControlServiceHandler(f)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func TestMCPRenderFront(t *testing.T) {
	front := newFakeLlamaSwap(t)
	dir := t.TempDir()
	backends := filepath.Join(dir, "backends")
	if err := os.MkdirAll(backends, 0o755); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(dir, "qwen.gguf")
	if err := os.WriteFile(model, []byte("GGUF"), 0o600); err != nil {
		t.Fatal(err)
	}
	def := fmt.Sprintf(`
name: qwen3.6-27b
backend:
  external: true
  llama_server:
    path: %s
    alias: qwen
    context: 4096
`, model)
	if err := os.WriteFile(filepath.Join(backends, "qwen3.6-27b.yaml"), []byte(def), 0o600); err != nil {
		t.Fatal(err)
	}
	frontCfg := filepath.Join(dir, "front-config.yaml")
	if err := os.WriteFile(frontCfg, []byte("# stale hand-maintained config\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	fleetDir := t.TempDir()
	fleet := fleetapi.New([]fleetapi.Cell{{Name: "front", URL: front.srv.URL, Class: "always_on"}},
		fleetDir+"/history.json",
		func() fleetapi.DaemonInfo { return fleetapi.DaemonInfo{} },
		fleetapi.Options{})
	t.Cleanup(fleet.Close)
	hosts := &fleetcfg.File{Cells: map[string]fleetcfg.Cell{
		"front":    {URL: front.srv.URL, Class: fleetcfg.ClassAlwaysOn},
		"gpu-cell": {URL: "http://gpu.lan:9000", Class: fleetcfg.ClassOpportunistic},
	}}
	s := New(fleet, hosts, Options{FrontConfig: frontCfg, BackendsDir: backends, LlamaBinary: "/usr/bin/llama-server"})
	mux := http.NewServeMux()
	s.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	// dry_run=true: diff against the stale file, not applied.
	resp := rpc(t, ts, `{"jsonrpc":"2.0","id":20,"method":"tools/call","params":{"name":"render_front","arguments":{"dry_run":true}}}`)
	text, isErr := toolText(t, resp)
	if isErr {
		t.Fatalf("render_front error: %s", text)
	}
	if !strings.Contains(text, "DRY RUN") || !strings.Contains(text, "stale hand-maintained") {
		t.Errorf("diff output = %q", text)
	}
	// The file is untouched.
	data, _ := os.ReadFile(frontCfg)
	if string(data) != "# stale hand-maintained config\n" {
		t.Errorf("dry run wrote the config: %q", data)
	}
	// dry_run=false: typed refusal.
	resp = rpc(t, ts, `{"jsonrpc":"2.0","id":21,"method":"tools/call","params":{"name":"render_front","arguments":{"dry_run":false}}}`)
	_, isErr = toolText(t, resp)
	if !isErr {
		t.Error("dry_run=false must fail in C2")
	}
}

func TestMCPDrainCellLongWaitNotCappedByProbeTimeout(t *testing.T) {
	// Regression for the review finding: drain_cell with wait_seconds
	// must not die at the 10s probe budget. The fake daemon takes 12s
	// to answer (quiescence wait semantics); the call must succeed.
	cellDaemon := newFakeCellDaemon(t)
	cellDaemon.drainDelay = 12 * time.Second
	front := newFakeLlamaSwap(t)
	_, ts := newTestFacade(t, map[string]fleetcfg.Cell{
		"front":    {URL: front.srv.URL, Class: fleetcfg.ClassAlwaysOn},
		"gpu-cell": {URL: "http://127.0.0.1:1", Class: fleetcfg.ClassOpportunistic, DaemonURL: cellDaemon.srv.URL},
	}, nil)
	if testing.Short() {
		t.Skip("12s delay")
	}
	start := time.Now()
	resp := rpc(t, ts, `{"jsonrpc":"2.0","id":16,"method":"tools/call","params":{"name":"drain_cell","arguments":{"cell":"gpu-cell","wait_seconds":30}}}`)
	text, isErr := toolText(t, resp)
	if isErr {
		t.Fatalf("drain with wait_seconds must survive past 10s: %s", text)
	}
	if time.Since(start) < 12*time.Second {
		t.Errorf("returned in %v — the 10s probe cap still applies", time.Since(start))
	}
}

func TestMCPDrainCellWritesIntentAfterSuccess(t *testing.T) {
	cellDaemon := newFakeCellDaemon(t)
	front := newFakeLlamaSwap(t)
	s, ts := newTestFacade(t, map[string]fleetcfg.Cell{
		"front":    {URL: front.srv.URL, Class: fleetcfg.ClassAlwaysOn},
		"gpu-cell": {URL: "http://127.0.0.1:1", Class: fleetcfg.ClassOpportunistic, DaemonURL: cellDaemon.srv.URL},
	}, nil)

	resp := rpc(t, ts, `{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"drain_cell","arguments":{"cell":"gpu-cell","reason":"gaming","eta":"23:00"}}}`)
	text, isErr := toolText(t, resp)
	if isErr {
		t.Fatalf("drain error: %s", text)
	}
	for _, want := range []string{"Drained gpu-cell", "gaming", "qwen3.6-27b", "in-flight requests at drain: 2", "batch-ingest"} {
		if !strings.Contains(text, want) {
			t.Errorf("report missing %q:\n%s", want, text)
		}
	}
	// Intent landed at fleetd AFTER the RPC succeeded.
	snap := s.fleet.Snapshot(t.Context())
	for _, c := range snap.Cells {
		if c.Name == "gpu-cell" {
			if c.Intent == nil || c.Intent.Reason != "gaming" || c.Intent.ETA != "23:00" {
				t.Errorf("intent = %+v", c.Intent)
			}
		}
	}
}

func TestMCPDrainCellRPCFailureWritesNoIntent(t *testing.T) {
	cellDaemon := newFakeCellDaemon(t)
	cellDaemon.failDrain = true
	front := newFakeLlamaSwap(t)
	s, ts := newTestFacade(t, map[string]fleetcfg.Cell{
		"front":    {URL: front.srv.URL, Class: fleetcfg.ClassAlwaysOn},
		"gpu-cell": {URL: "http://127.0.0.1:1", Class: fleetcfg.ClassOpportunistic, DaemonURL: cellDaemon.srv.URL},
	}, nil)

	resp := rpc(t, ts, `{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"drain_cell","arguments":{"cell":"gpu-cell","reason":"gaming"}}}`)
	text, isErr := toolText(t, resp)
	if !isErr {
		t.Fatalf("expected drain failure, got %q", text)
	}
	snap := s.fleet.Snapshot(t.Context())
	for _, c := range snap.Cells {
		if c.Name == "gpu-cell" && c.Intent != nil {
			t.Errorf("failed drain recorded intent: %+v — a failed drain never records", c.Intent)
		}
	}
}

func TestMCPResumeCellClearsIntent(t *testing.T) {
	cellDaemon := newFakeCellDaemon(t)
	front := newFakeLlamaSwap(t)
	s, ts := newTestFacade(t, map[string]fleetcfg.Cell{
		"front":    {URL: front.srv.URL, Class: fleetcfg.ClassAlwaysOn},
		"gpu-cell": {URL: "http://127.0.0.1:1", Class: fleetcfg.ClassOpportunistic, DaemonURL: cellDaemon.srv.URL},
	}, nil)
	if _, err := s.fleet.SetIntent("gpu-cell", "drained", "gaming", ""); err != nil {
		t.Fatal(err)
	}

	resp := rpc(t, ts, `{"jsonrpc":"2.0","id":12,"method":"tools/call","params":{"name":"resume_cell","arguments":{"cell":"gpu-cell"}}}`)
	text, isErr := toolText(t, resp)
	if isErr {
		t.Fatalf("resume error: %s", text)
	}
	snap := s.fleet.Snapshot(t.Context())
	for _, c := range snap.Cells {
		if c.Name == "gpu-cell" && c.Intent != nil {
			t.Errorf("resume left intent: %+v", c.Intent)
		}
	}
	if cellDaemon.resumeCalls != 1 {
		t.Errorf("resume RPC calls = %d", cellDaemon.resumeCalls)
	}
}

func TestMCPDrainCellNoDaemonURL(t *testing.T) {
	front := newFakeLlamaSwap(t)
	_, ts := newTestFacade(t, map[string]fleetcfg.Cell{
		"front":  {URL: front.srv.URL, Class: fleetcfg.ClassAlwaysOn},
		"laptop": {URL: "http://127.0.0.1:1", Class: fleetcfg.ClassRoaming},
	}, nil)
	resp := rpc(t, ts, `{"jsonrpc":"2.0","id":13,"method":"tools/call","params":{"name":"drain_cell","arguments":{"cell":"laptop"}}}`)
	text, isErr := toolText(t, resp)
	if !isErr || !strings.Contains(text, "daemon_url") {
		t.Errorf("missing daemon_url must be a typed error: isErr=%v text=%q", isErr, text)
	}
}

func TestMCPWakeCellErrorsWithoutConfig(t *testing.T) {
	front := newFakeLlamaSwap(t)
	_, ts := newTestFacade(t, map[string]fleetcfg.Cell{
		"front":    {URL: front.srv.URL, Class: fleetcfg.ClassAlwaysOn},
		"gpu-cell": {URL: "http://127.0.0.1:1", Class: fleetcfg.ClassOpportunistic},
	}, nil)
	// newTestFacade builds fleet cells without Wake — the tool must
	// surface the no-wake-config error, not panic.
	resp := rpc(t, ts, `{"jsonrpc":"2.0","id":14,"method":"tools/call","params":{"name":"wake_cell","arguments":{"cell":"gpu-cell"}}}`)
	text, isErr := toolText(t, resp)
	if !isErr || !strings.Contains(text, "wake") {
		t.Errorf("no-wake-config must be a tool error: isErr=%v text=%q", isErr, text)
	}
	resp = rpc(t, ts, `{"jsonrpc":"2.0","id":15,"method":"tools/call","params":{"name":"wake_cell","arguments":{"cell":"typo"}}}`)
	text, isErr = toolText(t, resp)
	if !isErr || !strings.Contains(text, "unknown cell") {
		t.Errorf("unknown cell must be a tool error: isErr=%v text=%q", isErr, text)
	}
}
