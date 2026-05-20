package frontend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
)

// recordedCall captures the parameters of one runCommand invocation so the
// test body can assert against them without spawning docker.
type recordedCall struct {
	name string
	args []string
	env  []string
}

// fakeRunner builds a runCommand stub that records every invocation and
// returns the provided errors in order (one per call; nil keeps the call
// successful).
func fakeRunner(errs ...error) (*[]recordedCall, func(ctx context.Context, name string, args []string, env []string) error) {
	var (
		mu    sync.Mutex
		calls []recordedCall
		i     int
	)
	return &calls, func(_ context.Context, name string, args []string, env []string) error {
		mu.Lock()
		defer mu.Unlock()
		// Defensive copies — exec.Cmd-style argv slices are sometimes
		// reused by callers.
		argsCopy := append([]string(nil), args...)
		envCopy := append([]string(nil), env...)
		calls = append(calls, recordedCall{name: name, args: argsCopy, env: envCopy})
		if i < len(errs) {
			err := errs[i]
			i++
			return err
		}
		return nil
	}
}

// fakeProbe returns a probeURL stub that yields statuses (and errors) in
// the order supplied. After the list is exhausted it returns the final
// entry indefinitely.
func fakeProbe(results ...probeResult) func(ctx context.Context, url string) (int, error) {
	var (
		mu sync.Mutex
		i  int
	)
	return func(_ context.Context, _ string) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		r := results[i]
		if i < len(results)-1 {
			i++
		}
		return r.status, r.err
	}
}

type probeResult struct {
	status int
	err    error
}

// writeComposeFile drops a placeholder compose file the driver will stat()
// before invoking docker.
func writeComposeFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "docker-compose.yaml")
	if err := os.WriteFile(p, []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCompose_Activate_BuildsArgv_WithServicesAndProjectName(t *testing.T) {
	composeFile := writeComposeFile(t)
	calls, runner := fakeRunner()

	d := &composeDriver{
		runCommand:     runner,
		probeURL:       fakeProbe(probeResult{status: 200}),
		now:            time.Now,
		defaultTimeout: time.Second,
		pollInterval:   time.Millisecond,
	}

	p := &profile.Profile{
		Name: "perplexica",
		Frontend: profile.Frontend{
			Kind:        profile.FrontendDockerCompose,
			ComposeFile: composeFile,
			ProjectName: "vibe-perplexica",
			Services:    []string{"searxng", "perplexica"},
		},
	}
	r, err := d.Activate(context.Background(), p, profile.ExpandContext{})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if r.WroteFile != composeFile {
		t.Errorf("WroteFile = %q, want %q", r.WroteFile, composeFile)
	}
	if r.Kind != profile.FrontendDockerCompose {
		t.Errorf("Kind = %q, want %q", r.Kind, profile.FrontendDockerCompose)
	}
	if len(*calls) != 1 {
		t.Fatalf("got %d runner calls, want 1: %+v", len(*calls), *calls)
	}
	got := (*calls)[0]
	if got.name != "docker" {
		t.Errorf("cmd = %q, want docker", got.name)
	}
	wantArgs := []string{
		"compose", "-f", composeFile, "-p", "vibe-perplexica",
		"up", "-d", "searxng", "perplexica",
	}
	if !reflect.DeepEqual(got.args, wantArgs) {
		t.Errorf("argv =\n  %v\nwant\n  %v", got.args, wantArgs)
	}
}

func TestCompose_Activate_DefaultsProjectName(t *testing.T) {
	composeFile := writeComposeFile(t)
	calls, runner := fakeRunner()

	d := &composeDriver{
		runCommand:     runner,
		probeURL:       fakeProbe(probeResult{status: 200}),
		now:            time.Now,
		defaultTimeout: time.Second,
		pollInterval:   time.Millisecond,
	}

	p := &profile.Profile{
		Name: "research",
		Frontend: profile.Frontend{
			Kind:        profile.FrontendDockerCompose,
			ComposeFile: composeFile,
		},
	}
	if _, err := d.Activate(context.Background(), p, profile.ExpandContext{}); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(*calls))
	}
	// `-p` should be present and follow it with "vibe-<profile-name>".
	args := (*calls)[0].args
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-p" {
			if args[i+1] != "vibe-research" {
				t.Errorf("project name = %q, want vibe-research", args[i+1])
			}
			return
		}
	}
	t.Errorf("argv missing -p flag: %v", args)
}

func TestCompose_Activate_NoWaitFor_NoServices(t *testing.T) {
	composeFile := writeComposeFile(t)
	calls, runner := fakeRunner()

	d := &composeDriver{
		runCommand:     runner,
		probeURL:       fakeProbe(probeResult{status: 200}),
		now:            time.Now,
		defaultTimeout: time.Second,
		pollInterval:   time.Millisecond,
	}

	p := &profile.Profile{
		Name: "p",
		Frontend: profile.Frontend{
			Kind:        profile.FrontendDockerCompose,
			ComposeFile: composeFile,
			ProjectName: "vibe-p",
		},
	}
	if _, err := d.Activate(context.Background(), p, profile.ExpandContext{}); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	wantArgs := []string{"compose", "-f", composeFile, "-p", "vibe-p", "up", "-d"}
	if !reflect.DeepEqual((*calls)[0].args, wantArgs) {
		t.Errorf("argv = %v\nwant %v", (*calls)[0].args, wantArgs)
	}
}

func TestCompose_Activate_PassesEnvSorted(t *testing.T) {
	composeFile := writeComposeFile(t)
	calls, runner := fakeRunner()

	d := &composeDriver{
		runCommand:     runner,
		probeURL:       fakeProbe(probeResult{status: 200}),
		now:            time.Now,
		defaultTimeout: time.Second,
		pollInterval:   time.Millisecond,
	}

	p := &profile.Profile{
		Name: "p",
		Frontend: profile.Frontend{
			Kind:        profile.FrontendDockerCompose,
			ComposeFile: composeFile,
			ProjectName: "vibe-p",
			Env: map[string]string{
				"OLLAMA_API_URL": "${VIBE_API}",
				"PORT":           "3001",
			},
		},
	}
	r, err := d.Activate(context.Background(), p, profile.ExpandContext{
		VibeAPI: "http://127.0.0.1:9000/v1",
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if got, want := r.Env["OLLAMA_API_URL"], "http://127.0.0.1:9000/v1"; got != want {
		t.Errorf("expanded env OLLAMA_API_URL = %q, want %q", got, want)
	}
	gotEnv := (*calls)[0].env
	wantEnv := []string{
		"OLLAMA_API_URL=http://127.0.0.1:9000/v1",
		"PORT=3001",
	}
	// envMapToSlice sorts; verify equality.
	if !reflect.DeepEqual(gotEnv, wantEnv) {
		// double-check by sorting both sides — failure here is a real bug,
		// but we want a clear diff in either ordering.
		sort.Strings(gotEnv)
		sort.Strings(wantEnv)
		if !reflect.DeepEqual(gotEnv, wantEnv) {
			t.Errorf("env = %v\nwant %v", (*calls)[0].env, wantEnv)
		}
	}
}

func TestCompose_Activate_WaitForPollsUntilReady(t *testing.T) {
	composeFile := writeComposeFile(t)
	_, runner := fakeRunner()

	probeCalls := 0
	d := &composeDriver{
		runCommand: runner,
		probeURL: func(_ context.Context, _ string) (int, error) {
			probeCalls++
			if probeCalls < 3 {
				return 503, nil
			}
			return 200, nil
		},
		now:            time.Now,
		defaultTimeout: time.Second,
		pollInterval:   time.Millisecond,
	}
	p := &profile.Profile{
		Name: "p",
		Frontend: profile.Frontend{
			Kind:        profile.FrontendDockerCompose,
			ComposeFile: composeFile,
			ProjectName: "vibe-p",
			WaitFor: []profile.WaitForURL{
				{URL: "http://127.0.0.1:3001/", Timeout: time.Second},
			},
		},
	}
	if _, err := d.Activate(context.Background(), p, profile.ExpandContext{}); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if probeCalls < 3 {
		t.Errorf("probeCalls = %d, want at least 3", probeCalls)
	}
}

func TestCompose_Activate_WaitForTimeoutTearsDown(t *testing.T) {
	composeFile := writeComposeFile(t)
	calls, runner := fakeRunner()

	d := &composeDriver{
		runCommand:     runner,
		probeURL:       fakeProbe(probeResult{status: 503}),
		now:            time.Now,
		defaultTimeout: 10 * time.Millisecond,
		pollInterval:   1 * time.Millisecond,
	}
	p := &profile.Profile{
		Name: "p",
		Frontend: profile.Frontend{
			Kind:        profile.FrontendDockerCompose,
			ComposeFile: composeFile,
			ProjectName: "vibe-p",
			WaitFor: []profile.WaitForURL{
				{URL: "http://127.0.0.1:3001/", Timeout: 10 * time.Millisecond},
			},
		},
	}
	_, err := d.Activate(context.Background(), p, profile.ExpandContext{})
	if err == nil {
		t.Fatal("Activate: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "wait_for") {
		t.Errorf("err = %v, want mention of wait_for", err)
	}
	// First call is `up`, second should be `down`.
	if len(*calls) != 2 {
		t.Fatalf("got %d runner calls, want 2 (up + teardown down): %+v", len(*calls), *calls)
	}
	down := (*calls)[1]
	wantDown := []string{"compose", "-f", composeFile, "-p", "vibe-p", "down"}
	if !reflect.DeepEqual(down.args, wantDown) {
		t.Errorf("teardown argv = %v, want %v", down.args, wantDown)
	}
}

func TestCompose_Activate_UpFailureReturnsError(t *testing.T) {
	composeFile := writeComposeFile(t)
	upErr := errors.New("boom")
	calls, runner := fakeRunner(upErr)

	d := &composeDriver{
		runCommand:     runner,
		probeURL:       fakeProbe(probeResult{status: 200}),
		now:            time.Now,
		defaultTimeout: time.Second,
		pollInterval:   time.Millisecond,
	}
	p := &profile.Profile{
		Name: "p",
		Frontend: profile.Frontend{
			Kind:        profile.FrontendDockerCompose,
			ComposeFile: composeFile,
			ProjectName: "vibe-p",
		},
	}
	_, err := d.Activate(context.Background(), p, profile.ExpandContext{})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Activate: err = %v, want wrapping of boom", err)
	}
	// Only one call (the failing `up`); we do not run `down` if `up`
	// itself failed.
	if len(*calls) != 1 {
		t.Errorf("got %d calls, want 1 (no teardown on up failure)", len(*calls))
	}
}

func TestCompose_Activate_MissingComposeFile(t *testing.T) {
	d := &composeDriver{
		runCommand: func(context.Context, string, []string, []string) error {
			t.Fatal("runCommand should not be called when compose_file is missing")
			return nil
		},
		probeURL:       fakeProbe(probeResult{status: 200}),
		now:            time.Now,
		defaultTimeout: time.Second,
		pollInterval:   time.Millisecond,
	}
	p := &profile.Profile{
		Name: "p",
		Frontend: profile.Frontend{
			Kind:        profile.FrontendDockerCompose,
			ComposeFile: "/nonexistent/docker-compose.yaml",
			ProjectName: "vibe-p",
		},
	}
	_, err := d.Activate(context.Background(), p, profile.ExpandContext{})
	if err == nil {
		t.Fatal("expected error for missing compose_file")
	}
	if !strings.Contains(err.Error(), "compose_file") {
		t.Errorf("err = %v, want mention of compose_file", err)
	}
}

func TestCompose_Deactivate_RunsDown(t *testing.T) {
	composeFile := writeComposeFile(t)
	calls, runner := fakeRunner()

	d := &composeDriver{
		runCommand:     runner,
		probeURL:       fakeProbe(probeResult{status: 200}),
		now:            time.Now,
		defaultTimeout: time.Second,
		pollInterval:   time.Millisecond,
	}
	p := &profile.Profile{
		Name: "p",
		Frontend: profile.Frontend{
			Kind:        profile.FrontendDockerCompose,
			ComposeFile: composeFile,
			ProjectName: "vibe-p",
			Services:    []string{"web"},
		},
	}
	r, err := d.Activate(context.Background(), p, profile.ExpandContext{})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if err := Deactivate(context.Background(), r); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	if len(*calls) != 2 {
		t.Fatalf("got %d calls, want 2: %+v", len(*calls), *calls)
	}
	wantDown := []string{"compose", "-f", composeFile, "-p", "vibe-p", "down"}
	if !reflect.DeepEqual((*calls)[1].args, wantDown) {
		t.Errorf("down argv = %v\nwant %v", (*calls)[1].args, wantDown)
	}
}

func TestCompose_ExpandsComposeFilePath(t *testing.T) {
	stateDir := t.TempDir()
	subDir := filepath.Join(stateDir, "frontend", "perplexica")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	composeFile := filepath.Join(subDir, "docker-compose.yaml")
	if err := os.WriteFile(composeFile, []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	calls, runner := fakeRunner()
	d := &composeDriver{
		runCommand:     runner,
		probeURL:       fakeProbe(probeResult{status: 200}),
		now:            time.Now,
		defaultTimeout: time.Second,
		pollInterval:   time.Millisecond,
	}
	p := &profile.Profile{
		Name: "research",
		Frontend: profile.Frontend{
			Kind:        profile.FrontendDockerCompose,
			ComposeFile: "${VIBE_STATE_DIR}/frontend/perplexica/docker-compose.yaml",
		},
	}
	r, err := d.Activate(context.Background(), p, profile.ExpandContext{
		VibeStateDir: stateDir,
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if r.WroteFile != composeFile {
		t.Errorf("WroteFile = %q, want %q", r.WroteFile, composeFile)
	}
	// Make sure the expanded path made it into the argv.
	if !strings.Contains(strings.Join((*calls)[0].args, " "), composeFile) {
		t.Errorf("argv missing expanded compose path: %v", (*calls)[0].args)
	}
}

func TestDeactivate_NilSafe(t *testing.T) {
	if err := Deactivate(context.Background(), nil); err != nil {
		t.Errorf("Deactivate(nil): %v", err)
	}
	if err := Deactivate(context.Background(), &Result{Kind: profile.FrontendExternal}); err != nil {
		t.Errorf("Deactivate(external Result): %v", err)
	}
}
