package vibeclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	vibev1 "github.com/gallowaysoftware/vibe/proto/vibe/v1"
	"github.com/gallowaysoftware/vibe/proto/vibe/v1/vibev1connect"
)

// fakeControl is an in-memory ControlService used by the tests. It records
// every call and supports per-test customization via the *Fn fields.
type fakeControl struct {
	mu       sync.Mutex
	calls    []string
	status   *vibev1.Status
	profiles []*vibev1.Profile

	startErr error
	stopErr  error
}

func (f *fakeControl) record(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
}
func (f *fakeControl) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *fakeControl) Status(_ context.Context, _ *connect.Request[vibev1.StatusRequest]) (*connect.Response[vibev1.StatusResponse], error) {
	f.record("Status")
	return connect.NewResponse(&vibev1.StatusResponse{Status: f.status}), nil
}
func (f *fakeControl) ListProfiles(_ context.Context, _ *connect.Request[vibev1.ListProfilesRequest]) (*connect.Response[vibev1.ListProfilesResponse], error) {
	f.record("ListProfiles")
	return connect.NewResponse(&vibev1.ListProfilesResponse{Profiles: f.profiles}), nil
}
func (f *fakeControl) Start(_ context.Context, req *connect.Request[vibev1.StartRequest]) (*connect.Response[vibev1.StartResponse], error) {
	f.record("Start")
	if f.startErr != nil {
		return nil, f.startErr
	}
	f.mu.Lock()
	f.status = &vibev1.Status{Running: true, Ready: true, Profile: req.Msg.Profile}
	f.mu.Unlock()
	return connect.NewResponse(&vibev1.StartResponse{Status: f.status}), nil
}
func (f *fakeControl) Stop(_ context.Context, _ *connect.Request[vibev1.StopRequest]) (*connect.Response[vibev1.StopResponse], error) {
	f.record("Stop")
	if f.stopErr != nil {
		return nil, f.stopErr
	}
	f.mu.Lock()
	f.status = &vibev1.Status{}
	f.mu.Unlock()
	return connect.NewResponse(&vibev1.StopResponse{Status: f.status}), nil
}
func (f *fakeControl) Shutdown(_ context.Context, _ *connect.Request[vibev1.ShutdownRequest]) (*connect.Response[vibev1.ShutdownResponse], error) {
	f.record("Shutdown")
	return connect.NewResponse(&vibev1.ShutdownResponse{}), nil
}
func (f *fakeControl) Logs(_ context.Context, _ *connect.Request[vibev1.LogsRequest]) (*connect.Response[vibev1.LogsResponse], error) {
	f.record("Logs")
	return connect.NewResponse(&vibev1.LogsResponse{Lines: []string{"line1", "line2"}}), nil
}
func (f *fakeControl) Pull(_ context.Context, _ *connect.Request[vibev1.PullRequest], stream *connect.ServerStream[vibev1.PullProgress]) error {
	f.record("Pull")
	return stream.Send(&vibev1.PullProgress{Phase: vibev1.PullProgress_PHASE_DONE})
}

func newFakeServer(t *testing.T, svc vibev1connect.ControlServiceHandler) (*httptest.Server, *Client) {
	t.Helper()
	mux := http.NewServeMux()
	path, handler := vibev1connect.NewControlServiceHandler(svc)
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, New(srv.URL)
}

func TestNew_PrefersExplicitArgument(t *testing.T) {
	t.Setenv("VIBE_API", "http://env:9999")
	c := New("http://arg:1234")
	if c.rpc == nil {
		t.Fatal("rpc nil")
	}
}

func TestNew_FallsBackToDefault(t *testing.T) {
	t.Setenv("VIBE_API", "")
	c := New("")
	if c.rpc == nil {
		t.Fatal("rpc nil")
	}
}

func TestStatus(t *testing.T) {
	f := &fakeControl{status: &vibev1.Status{Running: true, Ready: true, Profile: "code"}}
	_, c := newFakeServer(t, f)
	s, err := c.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !s.Running || s.Profile != "code" {
		t.Errorf("got %+v", s)
	}
}

func TestListProfiles(t *testing.T) {
	f := &fakeControl{profiles: []*vibev1.Profile{
		{Name: "chat"},
		{Name: "code", Description: "Coding"},
	}}
	_, c := newFakeServer(t, f)
	profs, err := c.ListProfiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(profs) != 2 || profs[1].Name != "code" || profs[1].Description != "Coding" {
		t.Errorf("got %+v", profs)
	}
}

func TestStart_ErrorBubbles(t *testing.T) {
	f := &fakeControl{startErr: connect.NewError(connect.CodeAlreadyExists, errors.New(`profile "code" is already running`))}
	_, c := newFakeServer(t, f)
	_, err := c.Start(context.Background(), "code")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("err = %v", err)
	}
}

func TestEnsureActive_NoOpWhenAlreadyReady(t *testing.T) {
	f := &fakeControl{status: &vibev1.Status{Running: true, Ready: true, Profile: "code"}}
	_, c := newFakeServer(t, f)
	s, err := c.EnsureActive(context.Background(), "code")
	if err != nil {
		t.Fatal(err)
	}
	if s.Profile != "code" {
		t.Errorf("profile = %q", s.Profile)
	}
	if calls := f.callList(); len(calls) != 1 || calls[0] != "Status" {
		t.Errorf("calls = %v", calls)
	}
}

func TestEnsureActive_StopsAndStartsWhenSwitching(t *testing.T) {
	f := &fakeControl{status: &vibev1.Status{Running: true, Ready: true, Profile: "chat"}}
	_, c := newFakeServer(t, f)
	s, err := c.EnsureActive(context.Background(), "code")
	if err != nil {
		t.Fatal(err)
	}
	if s.Profile != "code" {
		t.Errorf("profile = %q", s.Profile)
	}
	want := []string{"Status", "Stop", "Start"}
	got := f.callList()
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

func TestEnsureActive_StartsWhenNothingActive(t *testing.T) {
	f := &fakeControl{status: &vibev1.Status{}}
	_, c := newFakeServer(t, f)
	if _, err := c.EnsureActive(context.Background(), "code"); err != nil {
		t.Fatal(err)
	}
	got := f.callList()
	if len(got) != 2 || got[0] != "Status" || got[1] != "Start" {
		t.Errorf("calls = %v", got)
	}
}

func TestLogs(t *testing.T) {
	_, c := newFakeServer(t, &fakeControl{})
	lines, err := c.Logs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || lines[0] != "line1" {
		t.Errorf("lines = %v", lines)
	}
}
