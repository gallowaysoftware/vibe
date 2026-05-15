package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/protobuf/types/known/timestamppb"

	vibev1 "github.com/gallowaysoftware/vibe/proto/vibe/v1"
)

// stubAPI is a fully synchronous in-memory implementation of vibeAPI for the
// model unit tests. It records the most recent action so tests can assert
// that key presses dispatched the right RPC.
type stubAPI struct {
	status       *vibev1.Status
	profiles     []*vibev1.Profile
	logs         []string
	statusErr    error
	startCalls   []string
	stopCalls    int
	startErr     error
	stopErr      error
}

func (s *stubAPI) Status(ctx context.Context) (*vibev1.Status, error) {
	if s.statusErr != nil {
		return nil, s.statusErr
	}
	return s.status, nil
}

func (s *stubAPI) ListProfiles(ctx context.Context) ([]*vibev1.Profile, error) {
	return s.profiles, nil
}

func (s *stubAPI) Logs(ctx context.Context) ([]string, error) { return s.logs, nil }

func (s *stubAPI) Start(ctx context.Context, profile string) error {
	s.startCalls = append(s.startCalls, profile)
	return s.startErr
}

func (s *stubAPI) Stop(ctx context.Context) error {
	s.stopCalls++
	return s.stopErr
}

func newTestModel(api *stubAPI) Model {
	// honorNoColor=true so View() output is plain ASCII regardless of env.
	return NewModel(api, "http://test", true)
}

// key returns a tea.KeyMsg for a single rune; for special keys, callers pass
// tea.KeyMsg{Type: ...} directly.
func key(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}

func TestUpdate_CursorNavigation(t *testing.T) {
	m := newTestModel(&stubAPI{})
	m.profiles = []*vibev1.Profile{
		{Name: "code"}, {Name: "fast"}, {Name: "comfyui"},
	}

	// down twice
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	nm, _ = nm.(Model).Update(tea.KeyMsg{Type: tea.KeyDown})
	if got := nm.(Model).cursor; got != 2 {
		t.Fatalf("cursor after 2 downs = %d, want 2", got)
	}

	// past the end stays clamped
	nm, _ = nm.(Model).Update(tea.KeyMsg{Type: tea.KeyDown})
	if got := nm.(Model).cursor; got != 2 {
		t.Fatalf("cursor clamped at end = %d, want 2", got)
	}

	// up once
	nm, _ = nm.(Model).Update(tea.KeyMsg{Type: tea.KeyUp})
	if got := nm.(Model).cursor; got != 1 {
		t.Fatalf("cursor after up = %d, want 1", got)
	}

	// vim keys also work
	nm, _ = nm.(Model).Update(key('k'))
	if got := nm.(Model).cursor; got != 0 {
		t.Fatalf("cursor after k = %d, want 0", got)
	}
	nm, _ = nm.(Model).Update(key('j'))
	if got := nm.(Model).cursor; got != 1 {
		t.Fatalf("cursor after j = %d, want 1", got)
	}

	// up at top stays at 0
	nm, _ = nm.(Model).Update(tea.KeyMsg{Type: tea.KeyUp})
	nm, _ = nm.(Model).Update(tea.KeyMsg{Type: tea.KeyUp})
	if got := nm.(Model).cursor; got != 0 {
		t.Fatalf("cursor clamped at start = %d, want 0", got)
	}
}

func TestUpdate_QuitKeys(t *testing.T) {
	cases := []tea.KeyMsg{
		key('q'),
		{Type: tea.KeyCtrlC},
		{Type: tea.KeyEsc},
	}
	for _, k := range cases {
		m := newTestModel(&stubAPI{})
		nm, cmd := m.Update(k)
		if !nm.(Model).quitting {
			t.Fatalf("key %v did not set quitting", k)
		}
		if cmd == nil {
			t.Fatalf("key %v did not return a Quit cmd", k)
		}
	}
}

func TestUpdate_StartKeyDispatches(t *testing.T) {
	api := &stubAPI{}
	m := newTestModel(api)
	m.profiles = []*vibev1.Profile{{Name: "code"}, {Name: "fast"}}
	m.cursor = 1

	nm, cmd := m.Update(key('s'))
	if got := nm.(Model).busy; got != "start" {
		t.Fatalf("busy = %q, want %q", got, "start")
	}
	if cmd == nil {
		t.Fatalf("start key returned nil cmd")
	}
	// Execute the cmd to confirm it calls Start on the right profile.
	cmd()
	if len(api.startCalls) != 1 || api.startCalls[0] != "fast" {
		t.Fatalf("startCalls = %v, want [fast]", api.startCalls)
	}
}

func TestUpdate_EnterIsAliasForStart(t *testing.T) {
	api := &stubAPI{}
	m := newTestModel(api)
	m.profiles = []*vibev1.Profile{{Name: "code"}}

	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := nm.(Model).busy; got != "start" {
		t.Fatalf("enter: busy = %q, want %q", got, "start")
	}
	cmd()
	if len(api.startCalls) != 1 || api.startCalls[0] != "code" {
		t.Fatalf("enter: startCalls = %v, want [code]", api.startCalls)
	}
}

func TestUpdate_StopKeyDispatches(t *testing.T) {
	api := &stubAPI{}
	m := newTestModel(api)
	m.profiles = []*vibev1.Profile{{Name: "code"}}

	nm, cmd := m.Update(key('x'))
	if got := nm.(Model).busy; got != "stop" {
		t.Fatalf("busy = %q, want %q", got, "stop")
	}
	cmd()
	if api.stopCalls != 1 {
		t.Fatalf("stopCalls = %d, want 1", api.stopCalls)
	}
}

func TestUpdate_StartSwallowedWhenBusy(t *testing.T) {
	api := &stubAPI{}
	m := newTestModel(api)
	m.profiles = []*vibev1.Profile{{Name: "code"}}
	m.busy = "start"

	_, cmd := m.Update(key('s'))
	if cmd != nil {
		t.Fatalf("expected nil cmd while busy, got non-nil")
	}
	if len(api.startCalls) != 0 {
		t.Fatalf("Start dispatched while busy: %v", api.startCalls)
	}
}

func TestUpdate_StartNoopWithNoProfiles(t *testing.T) {
	api := &stubAPI{}
	m := newTestModel(api)
	// No profiles loaded.
	_, cmd := m.Update(key('s'))
	if cmd != nil {
		t.Fatalf("expected nil cmd with no profiles")
	}
	if got := m.busy; got != "" {
		t.Fatalf("busy set to %q with no profiles", got)
	}
}

func TestUpdate_RefreshKeyReturnsCmd(t *testing.T) {
	m := newTestModel(&stubAPI{})
	_, cmd := m.Update(key('r'))
	if cmd == nil {
		t.Fatalf("'r' did not return a refresh cmd")
	}
}

func TestUpdate_TickSchedulesNextTickAndRefresh(t *testing.T) {
	m := newTestModel(&stubAPI{})
	_, cmd := m.Update(tickMsg(time.Now()))
	if cmd == nil {
		t.Fatalf("tick did not return a batched cmd")
	}
}

func TestUpdate_RefreshMsgPopulatesState(t *testing.T) {
	m := newTestModel(&stubAPI{})
	st := &vibev1.Status{
		Running: true, Ready: true, Profile: "code",
		BackendAddr: "http://127.0.0.1:34567", ProxyAddr: "http://127.0.0.1:9000",
		Pid: 42, StartedAt: timestamppb.New(time.Now().Add(-time.Minute)),
	}
	profs := []*vibev1.Profile{{Name: "code", Description: "Coding"}}
	logs := []string{"line1", "line2"}

	nm, _ := m.Update(refreshMsg{status: st, profiles: profs, logs: logs})
	got := nm.(Model)
	if !got.connected {
		t.Fatal("connected = false, want true")
	}
	if got.status != st {
		t.Fatal("status not assigned")
	}
	if len(got.profiles) != 1 || got.profiles[0].Name != "code" {
		t.Fatalf("profiles = %v", got.profiles)
	}
	if len(got.logs) != 2 {
		t.Fatalf("logs len = %d, want 2", len(got.logs))
	}
}

func TestUpdate_RefreshMsgErrorMarksDisconnected(t *testing.T) {
	m := newTestModel(&stubAPI{})
	m.connected = true
	nm, _ := m.Update(refreshMsg{err: errors.New("dial unix /run/vibe/vibe.sock: connection refused")})
	got := nm.(Model)
	if got.connected {
		t.Fatal("connected should be false after error")
	}
	if !strings.Contains(got.errMsg, "connection refused") {
		t.Fatalf("errMsg = %q, want it to mention connection refused", got.errMsg)
	}
}

func TestUpdate_RefreshClampsCursor(t *testing.T) {
	m := newTestModel(&stubAPI{})
	m.profiles = []*vibev1.Profile{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	m.cursor = 2
	// Profiles shrink to 1 entry; cursor should clamp.
	nm, _ := m.Update(refreshMsg{profiles: []*vibev1.Profile{{Name: "a"}}})
	if got := nm.(Model).cursor; got != 0 {
		t.Fatalf("cursor clamp = %d, want 0", got)
	}
}

func TestUpdate_ActionDoneClearsBusyAndRefreshes(t *testing.T) {
	m := newTestModel(&stubAPI{})
	m.busy = "start"
	nm, cmd := m.Update(actionDoneMsg{action: "start"})
	if got := nm.(Model).busy; got != "" {
		t.Fatalf("busy = %q, want empty after action done", got)
	}
	if cmd == nil {
		t.Fatal("expected refresh cmd after action done")
	}
}

func TestUpdate_ActionDoneErrorSurfaced(t *testing.T) {
	m := newTestModel(&stubAPI{})
	m.busy = "start"
	nm, _ := m.Update(actionDoneMsg{action: "start", err: errors.New("model not found")})
	got := nm.(Model)
	if got.busy != "" {
		t.Fatalf("busy = %q, want empty", got.busy)
	}
	if !strings.Contains(got.errMsg, "start failed") || !strings.Contains(got.errMsg, "model not found") {
		t.Fatalf("errMsg = %q, missing expected text", got.errMsg)
	}
}

func TestUpdate_WindowSize(t *testing.T) {
	m := newTestModel(&stubAPI{})
	nm, cmd := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if cmd != nil {
		t.Fatal("WindowSizeMsg should not return a cmd")
	}
	if got := nm.(Model); got.width != 120 || got.height != 40 {
		t.Fatalf("dimensions = %dx%d, want 120x40", got.width, got.height)
	}
}

// TestView_DaemonDown asserts that View renders the friendly "not running"
// panel when the model has no status yet.
func TestView_DaemonDown(t *testing.T) {
	m := newTestModel(&stubAPI{})
	out := m.View()
	if !strings.Contains(out, "vibe daemon not running.") {
		t.Fatalf("View did not surface daemon-down message:\n%s", out)
	}
	if !strings.Contains(out, "vibe daemon &") {
		t.Fatalf("View missing hint about `vibe daemon &`:\n%s", out)
	}
}

// TestView_StubStatus is the snapshot the task asks for: with a hand-rolled
// Status, ListProfiles, and Logs, View renders the dashboard. We don't pin
// the exact bytes (uptime is wall-clock dependent), but we assert the
// load-bearing pieces are present.
func TestView_StubStatus(t *testing.T) {
	m := newTestModel(&stubAPI{})
	m.connected = true
	m.status = &vibev1.Status{
		Running: true, Ready: true, Profile: "code",
		BackendAddr: "http://127.0.0.1:34567",
		ProxyAddr:   "http://127.0.0.1:9000",
		Pid:         12345,
		StartedAt:   timestamppb.New(time.Now().Add(-12 * time.Minute)),
	}
	m.profiles = []*vibev1.Profile{
		{Name: "code", Description: "Coding with Qwen3.6-27B Q6_K + opencode"},
		{Name: "fast", Description: "Fast drafting with Qwen2.5-Coder-7B Q4_K_M"},
		{Name: "comfyui", Description: "ComfyUI for image generation (SDXL-Turbo workflow)"},
	}
	m.cursor = 2
	m.logs = []string{
		"20:13:32  llama-server starting binary=llama-server pid=...",
		"20:13:38  llama-server ready elapsed=6s",
		"20:13:38  daemon ready unix_socket=/run/user/1000/vibe/vibe.sock ...",
	}

	out := m.View()

	for _, want := range []string{
		"vibe", "http://test",
		"code", "fast", "comfyui",
		"ready", "pid 12345",
		"http://127.0.0.1:34567", "http://127.0.0.1:9000",
		"llama-server ready elapsed=6s",
		"[s/enter] start selected", "[x] stop", "[r] refresh", "[q] quit",
		"▸ ", // selected marker on the third row
	} {
		if !strings.Contains(out, want) {
			t.Errorf("View missing %q\n--- output ---\n%s", want, out)
		}
	}
}
