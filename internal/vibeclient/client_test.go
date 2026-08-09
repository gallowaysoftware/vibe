package vibeclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
func (f *fakeControl) CellDrain(_ context.Context, req *connect.Request[vibev1.CellDrainRequest]) (*connect.Response[vibev1.CellDrainResponse], error) {
	f.record("CellDrain")
	return connect.NewResponse(&vibev1.CellDrainResponse{ResidentModels: []string{"qwen"}}), nil
}

func (f *fakeControl) CellResume(_ context.Context, req *connect.Request[vibev1.CellResumeRequest]) (*connect.Response[vibev1.CellResumeResponse], error) {
	f.record("CellResume")
	return connect.NewResponse(&vibev1.CellResumeResponse{}), nil
}

func (f *fakeControl) CellSuspend(_ context.Context, req *connect.Request[vibev1.CellSuspendRequest]) (*connect.Response[vibev1.CellSuspendResponse], error) {
	f.record("CellSuspend")
	return connect.NewResponse(&vibev1.CellSuspendResponse{IdleStatus: "verified_idle"}), nil
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

// TestResolveToken_PrefersEnv asserts the documented precedence: $VIBE_TOKEN
// wins over the on-disk file. This is the path a remote laptop hits — the
// user exports VIBE_TOKEN even though their machine has no token file.
func TestResolveToken_PrefersEnv(t *testing.T) {
	t.Setenv("VIBE_TOKEN", "from-env")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if got := ResolveToken(); got != "from-env" {
		t.Errorf("ResolveToken = %q, want from-env", got)
	}
}

// TestResolveToken_FallsBackToFile covers the same-machine case: $VIBE_TOKEN
// unset, but the daemon's token file is present at $XDG_STATE_HOME/vibe/token.
func TestResolveToken_FallsBackToFile(t *testing.T) {
	t.Setenv("VIBE_TOKEN", "")
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	dir := filepath.Join(state, "vibe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ResolveToken(); got != "from-file" {
		t.Errorf("ResolveToken = %q, want from-file", got)
	}
}

// TestResolveToken_EmptyWhenNothingSet keeps the unix-socket-CLI path
// working: if there's no env and no file, ResolveToken returns "" rather
// than erroring.
func TestResolveToken_EmptyWhenNothingSet(t *testing.T) {
	t.Setenv("VIBE_TOKEN", "")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if got := ResolveToken(); got != "" {
		t.Errorf("ResolveToken = %q, want empty", got)
	}
}

// TestClient_InjectsBearerHeader confirms the client wraps every outgoing
// RPC with `Authorization: Bearer <token>`. We can't observe the header
// through the Connect SDK directly, so spin up an http.Handler that records
// it and verifies the value.
func TestClient_InjectsBearerHeader(t *testing.T) {
	var gotAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		// Reply with an empty body that Connect will parse as a fail; we
		// don't care, we only need to observe the header.
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := NewWithToken(srv.URL, "the-secret")
	_, _ = c.Status(context.Background()) // error expected — server isn't Connect-aware
	if gotAuth != "Bearer the-secret" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer the-secret")
	}
}

// TestClient_NoTokenMeansNoHeader is the unix-socket happy path: when no
// token is supplied, the client must NOT set Authorization (or the daemon's
// TCP middleware would reject a header it didn't expect on the socket — and
// more importantly, we don't want bogus headers on the wire).
func TestClient_NoTokenMeansNoHeader(t *testing.T) {
	var gotAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := NewWithToken(srv.URL, "")
	_, _ = c.Status(context.Background())
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want empty", gotAuth)
	}
}

// rejectionErr builds a Connect error carrying the typed StartRejection
// detail, the way the daemon does. It exists to enumerate cases the
// daemon does not currently produce (an unknown reason, a detail under a
// different code) — the ones it DOES produce are asserted against the
// real daemon in internal/vibe/daemon's TestDaemon_VRAMCheck_* tests,
// which is where the producer→classifier contract actually lives. This
// helper deliberately builds a typed message and never a prose string:
// the previous version of this test minted the daemon's sentence and
// then matched on it, which is why nobody noticed when 4a4c5ea changed
// the sentence and deleted the condition that emitted it.
func rejectionErr(t *testing.T, code connect.Code, msg string, detail *vibev1.StartRejection) error {
	t.Helper()
	ce := connect.NewError(code, errors.New(msg))
	d, err := connect.NewErrorDetail(detail)
	if err != nil {
		t.Fatalf("build error detail: %v", err)
	}
	ce.AddDetail(d)
	return ce
}

// TestIsVRAMRejection is the discrimination table: which errors vamp's
// candidate walk may skip past, and which must abort the pipeline. It
// asserts on the TYPED detail only — see rejectionErr.
//
// The two cases that matter most are the ones that would have caught the
// original defect: a FailedPrecondition whose prose reads exactly like
// the old pre-flight message but carries no detail is NOT a rejection,
// and a rejection with no such prose IS one.
func TestIsVRAMRejection(t *testing.T) {
	insufficient := &vibev1.StartRejection{
		Reason:       vibev1.StartRejection_REASON_VRAM_INSUFFICIENT_FREE,
		Profile:      "code",
		EstimatedGib: 24,
		FreeGib:      8,
	}
	exceeds := &vibev1.StartRejection{
		Reason:       vibev1.StartRejection_REASON_VRAM_EXCEEDS_CAPACITY,
		Profile:      "code",
		EstimatedGib: 128,
		FreeGib:      1,
	}

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil",
			err:  nil,
			want: false,
		},
		{
			name: "insufficient free memory, strict caller",
			err:  rejectionErr(t, connect.CodeFailedPrecondition, "needs more than is free right now", insufficient),
			want: true,
		},
		{
			name: "exceeds the machine's capacity",
			err:  rejectionErr(t, connect.CodeFailedPrecondition, "will never fit on this box", exceeds),
			want: true,
		},
		{
			// The regression case. This is verbatim the message the
			// classifier used to grep for, and it must no longer be
			// enough on its own — a producer that stops attaching the
			// detail has to go red somewhere.
			name: "the old prose with no detail",
			err: connect.NewError(
				connect.CodeFailedPrecondition,
				errors.New(`profile "code" needs ~24.0 GiB free VRAM but only 8.0 GiB is free.`),
			),
			want: false,
		},
		{
			name: "FailedPrecondition with no detail",
			err: connect.NewError(
				connect.CodeFailedPrecondition,
				errors.New("no profile is active"),
			),
			want: false,
		},
		{
			// A reason this build does not recognise is not a memory
			// rejection. Skipping a candidate on an unknown reason would
			// mean guessing that a future SLOT_HELD or NOT_STAGED is
			// safe to retry past, which is the caller's decision to make
			// deliberately when it teaches itself the new value.
			name: "unspecified reason",
			err: rejectionErr(t, connect.CodeFailedPrecondition, "refused",
				&vibev1.StartRejection{Profile: "code"}),
			want: false,
		},
		{
			// The detail is the contract, not the code. A rejection that
			// arrives under a different code is still a rejection; the
			// classifier deliberately does not require both, because two
			// couplings rot independently and that is how we got here.
			name: "detail under a different code",
			err:  rejectionErr(t, connect.CodeResourceExhausted, "refused", insufficient),
			want: true,
		},
		{
			name: "wrapped connect error",
			err: fmt.Errorf("activate %q: %w", "code",
				rejectionErr(t, connect.CodeFailedPrecondition, "refused", insufficient)),
			want: true,
		},
		{
			name: "plain error",
			err:  errors.New("free VRAM"),
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsVRAMRejection(tc.err); got != tc.want {
				t.Errorf("IsVRAMRejection(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestVRAMRejectionSurvivesTheWire proves the detail is readable by a
// real client after a real Connect round trip, not merely by the process
// that built it. A detail that marshalled but did not travel would leave
// the classifier answering false in production while every in-process
// test passed — the same shape of blind spot as the string match, one
// layer down.
//
// The numbers are asserted too: a candidate walk that can classify the
// refusal but cannot report what it cost is only half useful.
func TestVRAMRejectionSurvivesTheWire(t *testing.T) {
	want := &vibev1.StartRejection{
		Reason:       vibev1.StartRejection_REASON_VRAM_INSUFFICIENT_FREE,
		Profile:      "code",
		EstimatedGib: 24,
		FreeGib:      8.25,
	}
	mux := http.NewServeMux()
	path, handler := vibev1connect.NewControlServiceHandler(&rejectingControl{rej: want})
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := NewWithHTTPClient(srv.URL, srv.Client(), "")
	_, err := c.StartWithOptions(context.Background(), "code", StartOptions{StrictVRAM: true})
	if err == nil {
		t.Fatal("Start: expected the handler's refusal, got nil")
	}
	got, ok := VRAMRejection(err)
	if !ok {
		t.Fatalf("VRAMRejection lost the detail across the wire: %v", err)
	}
	if got.GetReason() != want.GetReason() {
		t.Errorf("reason = %v, want %v", got.GetReason(), want.GetReason())
	}
	if got.GetProfile() != want.GetProfile() {
		t.Errorf("profile = %q, want %q", got.GetProfile(), want.GetProfile())
	}
	if got.GetEstimatedGib() != want.GetEstimatedGib() {
		t.Errorf("estimated_gib = %v, want %v", got.GetEstimatedGib(), want.GetEstimatedGib())
	}
	if got.GetFreeGib() != want.GetFreeGib() {
		t.Errorf("free_gib = %v, want %v", got.GetFreeGib(), want.GetFreeGib())
	}
	if got.TotalGib != nil {
		t.Errorf("total_gib = %v, want absent — an unset optional must not arrive as a zero", *got.TotalGib)
	}
}

// rejectingControl answers Start with a typed rejection and everything
// else with Unimplemented. It also records whether the caller asked for
// a strict pre-flight, so the wire test proves the request flag travels
// as well as the error detail.
type rejectingControl struct {
	vibev1connect.UnimplementedControlServiceHandler
	rej *vibev1.StartRejection
}

func (s *rejectingControl) Start(_ context.Context, req *connect.Request[vibev1.StartRequest]) (*connect.Response[vibev1.StartResponse], error) {
	if !req.Msg.GetStrictVram() {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("strict_vram did not reach the handler: the request half of the contract is broken"))
	}
	ce := connect.NewError(connect.CodeFailedPrecondition, errors.New("refused"))
	d, err := connect.NewErrorDetail(s.rej)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	ce.AddDetail(d)
	return nil, ce
}

// TestClient_TCPRejectsMissingTokenAgainstSecuredHandler proves a client
// constructed without a token is actually rejected by a server that requires
// one. End-to-end shape check against a real Connect handler wrapped in the
// bearer-auth middleware.
func TestClient_TCPRejectsMissingTokenAgainstSecuredHandler(t *testing.T) {
	const token = "right-token"
	mux := http.NewServeMux()
	path, handler := vibev1connect.NewControlServiceHandler(&fakeControl{status: &vibev1.Status{}})
	mux.Handle(path, handler)
	// Inline the daemon's middleware here so the test stays in
	// internal/vibeclient (no daemon import cycle).
	authMux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if h != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(authMux)
	t.Cleanup(srv.Close)

	// 1) Wrong token → error.
	wrong := NewWithToken(srv.URL, "wrong")
	if _, err := wrong.Status(context.Background()); err == nil {
		t.Errorf("Status with wrong token: nil err; want unauthenticated")
	}
	// 2) No token → error.
	none := NewWithToken(srv.URL, "")
	if _, err := none.Status(context.Background()); err == nil {
		t.Errorf("Status with no token: nil err; want unauthenticated")
	}
	// 3) Right token → success.
	good := NewWithToken(srv.URL, token)
	if _, err := good.Status(context.Background()); err != nil {
		t.Errorf("Status with right token: %v", err)
	}
}
