package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	vibev1 "github.com/gallowaysoftware/vibe/proto/vibe/v1"
)

// controlFake is a ControlService in the DAEMON's role, for the commands
// that talk to the local daemon rather than to fleetd. It embeds
// cellVerbFake for the cell verbs and the unimplemented remainder, and
// answers the four RPCs `vibe run` makes: Status (which is also what
// ensureDaemon pings — an unanswered ping makes the CLI spawn a real
// daemon, which a test must never do), Pull, Start and Stop.
type controlFake struct {
	*cellVerbFake

	mu       sync.Mutex
	active   *vibev1.Status
	services []*vibev1.Status
	frontend *vibev1.FrontendInfo
	starts   int
	stops    int
	// statusDelay stalls Status before it answers, so a test can be a
	// daemon that is UP and merely slow — the case a ping budget cannot
	// tell from a daemon that is gone unless somebody looks at the error.
	statusDelay time.Duration
}

func newControlFake() *controlFake {
	return &controlFake{cellVerbFake: &cellVerbFake{}}
}

func (f *controlFake) Status(context.Context, *connect.Request[vibev1.StatusRequest]) (*connect.Response[vibev1.StatusResponse], error) {
	f.mu.Lock()
	delay := f.statusDelay
	f.mu.Unlock()
	// Outside the lock: the stall is meant to be a slow ANSWER, not a
	// serialisation point every other RPC in the test queues behind.
	time.Sleep(delay)
	f.mu.Lock()
	defer f.mu.Unlock()
	return connect.NewResponse(&vibev1.StatusResponse{Status: f.active, Services: f.services}), nil
}

func (f *controlFake) Start(_ context.Context, req *connect.Request[vibev1.StartRequest]) (*connect.Response[vibev1.StartResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	return connect.NewResponse(&vibev1.StartResponse{
		Status:   &vibev1.Status{Running: true, Ready: true, Profile: req.Msg.Profile, BackendAddr: "127.0.0.1:1"},
		Frontend: f.frontend,
	}), nil
}

func (f *controlFake) Stop(context.Context, *connect.Request[vibev1.StopRequest]) (*connect.Response[vibev1.StopResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	return connect.NewResponse(&vibev1.StopResponse{}), nil
}

func (f *controlFake) Pull(context.Context, *connect.Request[vibev1.PullRequest], *connect.ServerStream[vibev1.PullProgress]) error {
	return nil
}

func (f *controlFake) counts() (starts, stops int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts, f.stops
}

// runFixture writes a kind=managed profile whose frontend is a script
// exiting with the given code, and points every XDG location at temp
// dirs so nothing here can reach the live daemon, the live config or the
// live state.
func runFixture(t *testing.T, exitCode int) {
	t.Helper()
	cfg, state := t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("XDG_STATE_HOME", state)

	dir := t.TempDir()
	frontend := filepath.Join(dir, "frontend.sh")
	if err := os.WriteFile(frontend, []byte("#!/bin/sh\nexit "+strconv.Itoa(exitCode)+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(model, []byte("gguf"), 0o600); err != nil {
		t.Fatal(err)
	}
	profiles := filepath.Join(cfg, "vibe", "profiles")
	if err := os.MkdirAll(profiles, 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`name: fixture
backend:
  llama_server:
    path: %s
    alias: fixture-model
    context: 4096
frontend:
  kind: managed
  binary: %s
`, model, frontend)
	if err := os.WriteFile(filepath.Join(profiles, "fixture.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRunPropagatesTheFrontendsExitStatus is root.go's rule applied to
// the other wrapper. `vibe run` returned nil for ANY *exec.ExitError, so
// a frontend that died on a bad config, an OOM or a panic was reported to
// the shell exactly as a clean quit was — and `vibe run omp && next` ran
// `next`. The C24 sibling flattened 3 to 1; this flattened it to 0.
//
// The teardown half is asserted in the same run because it is the reason
// the swallow was written: returning an error does NOT skip the deferred
// stop.
func TestRunPropagatesTheFrontendsExitStatus(t *testing.T) {
	for _, code := range []int{7, 3, 0} {
		t.Run("exit "+strconv.Itoa(code), func(t *testing.T) {
			runFixture(t, code)
			fake := newControlFake()
			fake.frontend = &vibev1.FrontendInfo{Kind: "managed"}
			serveControlOnUnix(t, fake)

			cmd := runCmd()
			cmd.SetOut(&nopWriter{})
			cmd.SetErr(&nopWriter{})
			cmd.SetArgs([]string{"fixture"})
			err := cmd.ExecuteContext(t.Context())

			if got := ExitCode(err); got != code {
				t.Errorf("ExitCode(%v) = %d, want the frontend's own %d — a launcher, a .desktop Exec= and every `&&` in a shell read this number", err, got, code)
			}
			if code == 0 && err != nil {
				t.Errorf("a clean frontend exit produced %v", err)
			}
			starts, stops := fake.counts()
			if starts != 1 || stops != 1 {
				t.Errorf("starts=%d stops=%d — the profile must still be torn down on the failure path", starts, stops)
			}
		})
	}
}

// TestFrontendExitError_NonExitErrors: a Wait failure that is NOT the
// child's own status (the binary vanished mid-run, a ptrace error) has no
// exit code to pass on and must stay an ordinary named error rather than
// becoming a fabricated status.
func TestFrontendExitError_NonExitErrors(t *testing.T) {
	if err := frontendExitError("pi", nil); err != nil {
		t.Errorf("a clean exit produced %v", err)
	}
	err := frontendExitError("pi", errors.New("waitid: no child processes"))
	if err == nil || ExitCode(err) != 0 {
		t.Errorf("err = %v, ExitCode = %d — a non-ExitError must fall through to the ordinary `vibe: …` line and 1", err, ExitCode(err))
	}
	var ec exitCodeError
	if errors.As(err, &ec) {
		t.Errorf("a non-ExitError became an exit status (%d)", ec.code)
	}
}

// TestFrontendExitError_CarriesTheChildsCode runs a REAL child so the
// mapping is exercised against a genuine *exec.ExitError rather than a
// hand-built one.
func TestFrontendExitError_CarriesTheChildsCode(t *testing.T) {
	c := exec.Command("sh", "-c", "exit 7")
	err := frontendExitError("sh", c.Run())
	if got := ExitCode(err); got != 7 {
		t.Fatalf("ExitCode(%v) = %d, want 7", err, got)
	}
}

// nopWriter swallows the command's own output; the assertions are on the
// exit status and the RPCs, not on the banner.
type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestValidateRunArgs(t *testing.T) {
	cases := []struct {
		name    string
		dashAt  int
		args    []string
		wantErr bool
	}{
		{"plain profile name", -1, []string{"omp"}, false},
		{"no args", -1, []string{}, true},
		{"two args, no dash", -1, []string{"omp", "extra"}, true},
		{"profile then dash-args", 1, []string{"omp", "-c"}, false},
		{"profile then multiple dash-args", 1, []string{"omp", "-r", "abc123"}, false},
		{"dash with zero profile args", 0, []string{"-c"}, true},
		{"dash with two profile args", 2, []string{"omp", "extra", "-c"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRunArgs(tc.dashAt, tc.args)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateRunArgs(%d, %v) error = %v, wantErr %v", tc.dashAt, tc.args, err, tc.wantErr)
			}
		})
	}
}

func TestSplitRunArgs(t *testing.T) {
	cases := []struct {
		name         string
		dashAt       int
		args         []string
		wantProfile  string
		wantPassthru []string
	}{
		{"no dash", -1, []string{"omp"}, "omp", nil},
		{"dash with one passthrough", 1, []string{"omp", "-c"}, "omp", []string{"-c"}},
		{"dash with two passthrough", 1, []string{"omp", "-r", "abc123"}, "omp", []string{"-r", "abc123"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			profile, passthru := splitRunArgs(tc.dashAt, tc.args)
			if profile != tc.wantProfile {
				t.Errorf("profile = %q, want %q", profile, tc.wantProfile)
			}
			if !reflect.DeepEqual(passthru, tc.wantPassthru) {
				t.Errorf("passthru = %v, want %v", passthru, tc.wantPassthru)
			}
		})
	}
}
