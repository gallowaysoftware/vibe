package frontend

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
	"github.com/gallowaysoftware/vibe/internal/vibe/supervisor"
)

// fakeProc is an in-memory managedProc. Tests drive its lifecycle
// manually (signal, kill, wait) without spawning a real OS process.
type fakeProc struct {
	pid int

	mu             sync.Mutex
	exited         bool
	signals        []os.Signal
	killed         bool
	ignoreSIGINT   bool // when true, SIGINT no longer triggers exit
	signalErr      error
	waitErr        error
	signalCausesEx bool // when true, the first signal triggers exit

	done chan struct{}
}

func newFakeProc(pid int) *fakeProc {
	return &fakeProc{pid: pid, done: make(chan struct{}), signalCausesEx: true}
}

func (f *fakeProc) PID() int { return f.pid }

func (f *fakeProc) Signal(sig os.Signal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signals = append(f.signals, sig)
	if f.signalErr != nil {
		return f.signalErr
	}
	if !f.ignoreSIGINT && f.signalCausesEx && !f.exited {
		f.exited = true
		close(f.done)
	}
	return nil
}

func (f *fakeProc) Kill() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = true
	if !f.exited {
		f.exited = true
		close(f.done)
	}
	return nil
}

func (f *fakeProc) Wait() error {
	<-f.done
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.waitErr
}

// recordedStart captures the parameters a startProcess invocation
// received so the test body can assert without spawning a real binary.
type recordedStart struct {
	name    string
	args    []string
	env     []string
	workdir string
}

// fakeStarter builds a startProcess stub that records every invocation
// and returns the supplied fakeProc (or error) for each call.
func fakeStarter(proc *fakeProc, startErr error) (*[]recordedStart, func(ctx context.Context, name string, args []string, env []string, workdir string) (managedProc, error)) {
	var (
		mu    sync.Mutex
		calls []recordedStart
	)
	return &calls, func(_ context.Context, name string, args []string, env []string, workdir string) (managedProc, error) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls,
			recordedStart{
				name:    name,
				args:    append([]string(nil), args...),
				env:     append([]string(nil), env...),
				workdir: workdir,
			})
		if startErr != nil {
			return nil, startErr
		}
		return proc, nil
	}
}

// writeManagedBinary drops an executable shell script so
// p.Frontend.Binary points at something tests can stat. (No tests
// actually exec it via the fake starter; the real-binary tests below
// build testdata/fake-managed instead.)
func writeManagedBinary(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "run-server.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestManaged_Activate_BuildsArgvAndEnv(t *testing.T) {
	bin := writeManagedBinary(t)
	proc := newFakeProc(1234)
	calls, starter := fakeStarter(proc, nil)

	d := &managedDriver{
		startProcess: starter,
		waitPoller: testPoller(func(_ context.Context, _ string) (int, error) {
			return 200, nil
		}),
		gracefulShutdown: 100 * time.Millisecond,
	}

	p := &profile.Profile{
		Name: "openwebui",
		Frontend: profile.Frontend{
			Kind:   profile.FrontendManaged,
			Binary: bin,
			Args: []string{
				"--listen", "127.0.0.1",
				"--port", "8080",
			},
			Workdir: "/opt/open-webui",
			Env: map[string]string{
				"OPEN_WEBUI_DATA": "/var/lib/open-webui",
				"OLLAMA_API_URL":  "${VIBE_API}",
			},
			WaitFor: []profile.WaitForURL{
				{URL: "http://127.0.0.1:8080/", Timeout: time.Second},
			},
		},
	}
	r, err := d.Activate(context.Background(), p, profile.ExpandContext{
		VibeAPI: "http://127.0.0.1:9000/v1",
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if r.Kind != profile.FrontendManaged {
		t.Errorf("Kind = %q, want %q", r.Kind, profile.FrontendManaged)
	}
	if r.WroteFile != bin {
		t.Errorf("WroteFile = %q, want %q", r.WroteFile, bin)
	}
	if r.Env["OLLAMA_API_URL"] != "http://127.0.0.1:9000/v1" {
		t.Errorf("expanded env OLLAMA_API_URL = %q", r.Env["OLLAMA_API_URL"])
	}

	if len(*calls) != 1 {
		t.Fatalf("got %d start calls, want 1: %+v", len(*calls), *calls)
	}
	got := (*calls)[0]
	if got.name != bin {
		t.Errorf("cmd = %q, want %q", got.name, bin)
	}
	wantArgs := []string{"--listen", "127.0.0.1", "--port", "8080"}
	if !reflect.DeepEqual(got.args, wantArgs) {
		t.Errorf("argv =\n  %v\nwant\n  %v", got.args, wantArgs)
	}
	if got.workdir != "/opt/open-webui" {
		t.Errorf("workdir = %q, want %q", got.workdir, "/opt/open-webui")
	}
	wantEnv := []string{
		"OLLAMA_API_URL=http://127.0.0.1:9000/v1",
		"OPEN_WEBUI_DATA=/var/lib/open-webui",
	}
	if !reflect.DeepEqual(got.env, wantEnv) {
		t.Errorf("env =\n  %v\nwant\n  %v", got.env, wantEnv)
	}
}

func TestManaged_Activate_ExpandsArgs(t *testing.T) {
	bin := writeManagedBinary(t)
	proc := newFakeProc(1)
	calls, starter := fakeStarter(proc, nil)

	d := &managedDriver{
		startProcess:     starter,
		waitPoller:       testPoller(func(context.Context, string) (int, error) { return 200, nil }),
		gracefulShutdown: 100 * time.Millisecond,
	}
	p := &profile.Profile{
		Name: "omp",
		Frontend: profile.Frontend{
			Kind:   profile.FrontendManaged,
			Binary: bin,
			Args: []string{
				"--model", "vibe-local/${MODEL_ALIAS}",
				"--api", "${VIBE_API}",
			},
		},
	}
	if _, err := d.Activate(context.Background(), p, profile.ExpandContext{
		ModelAlias: "qwen",
		VibeAPI:    "http://127.0.0.1:9000/v1",
	}); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	got := (*calls)[0].args
	want := []string{
		"--model", "vibe-local/qwen",
		"--api", "http://127.0.0.1:9000/v1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv =\n  %v\nwant\n  %v", got, want)
	}
}

func TestManaged_Activate_NoArgsNoEnvNoWorkdir(t *testing.T) {
	bin := writeManagedBinary(t)
	proc := newFakeProc(7)
	calls, starter := fakeStarter(proc, nil)

	d := &managedDriver{
		startProcess: starter,
		waitPoller: testPoller(func(_ context.Context, _ string) (int, error) {
			return 200, nil
		}),
		gracefulShutdown: 100 * time.Millisecond,
	}
	p := &profile.Profile{
		Name: "minimal",
		Frontend: profile.Frontend{
			Kind:   profile.FrontendManaged,
			Binary: bin,
		},
	}
	if _, err := d.Activate(context.Background(), p, profile.ExpandContext{}); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(*calls))
	}
	got := (*calls)[0]
	if len(got.args) != 0 {
		t.Errorf("args = %v, want empty", got.args)
	}
	if len(got.env) != 0 {
		t.Errorf("env = %v, want empty", got.env)
	}
	if got.workdir != "" {
		t.Errorf("workdir = %q, want empty", got.workdir)
	}
}

func TestManaged_Activate_MissingBinary(t *testing.T) {
	d := &managedDriver{
		startProcess: func(context.Context, string, []string, []string, string) (managedProc, error) {
			t.Fatal("startProcess should not be called when binary is empty")
			return nil, nil
		},
		waitPoller:       testPoller(func(context.Context, string) (int, error) { return 200, nil }),
		gracefulShutdown: time.Millisecond,
	}
	p := &profile.Profile{
		Name:     "x",
		Frontend: profile.Frontend{Kind: profile.FrontendManaged},
	}
	_, err := d.Activate(context.Background(), p, profile.ExpandContext{})
	if err == nil {
		t.Fatal("expected error for empty binary")
	}
	if !strings.Contains(err.Error(), "binary") {
		t.Errorf("err = %v, want mention of binary", err)
	}
}

func TestManaged_Activate_StartErrorReturned(t *testing.T) {
	bin := writeManagedBinary(t)
	startErr := errors.New("permission denied")
	_, starter := fakeStarter(nil, startErr)
	d := &managedDriver{
		startProcess:     starter,
		waitPoller:       testPoller(func(context.Context, string) (int, error) { return 200, nil }),
		gracefulShutdown: time.Millisecond,
	}
	p := &profile.Profile{
		Name: "x",
		Frontend: profile.Frontend{
			Kind:   profile.FrontendManaged,
			Binary: bin,
		},
	}
	_, err := d.Activate(context.Background(), p, profile.ExpandContext{})
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("err = %v, want permission denied", err)
	}
}

func TestManaged_Activate_WaitForPollsUntilReady(t *testing.T) {
	bin := writeManagedBinary(t)
	proc := newFakeProc(1)
	_, starter := fakeStarter(proc, nil)

	probeCalls := 0
	d := &managedDriver{
		startProcess: starter,
		waitPoller: testPoller(func(_ context.Context, _ string) (int, error) {
			probeCalls++
			if probeCalls < 3 {
				return 503, nil
			}
			return 200, nil
		}),
		gracefulShutdown: 100 * time.Millisecond,
	}
	p := &profile.Profile{
		Name: "x",
		Frontend: profile.Frontend{
			Kind:   profile.FrontendManaged,
			Binary: bin,
			WaitFor: []profile.WaitForURL{
				{URL: "http://127.0.0.1:8080/", Timeout: time.Second},
			},
		},
	}
	if _, err := d.Activate(context.Background(), p, profile.ExpandContext{}); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if probeCalls < 3 {
		t.Errorf("probeCalls = %d, want >= 3", probeCalls)
	}
}

func TestManaged_Activate_WaitForTimeoutTearsDown(t *testing.T) {
	bin := writeManagedBinary(t)
	proc := newFakeProc(42)
	_, starter := fakeStarter(proc, nil)

	d := &managedDriver{
		startProcess: starter,
		waitPoller: waitPoller{
			probeURL: func(context.Context, string) (int, error) {
				return 503, nil
			},
			now:            time.Now,
			defaultTimeout: 10 * time.Millisecond,
			pollInterval:   1 * time.Millisecond,
		},
		gracefulShutdown: 100 * time.Millisecond,
	}
	p := &profile.Profile{
		Name: "x",
		Frontend: profile.Frontend{
			Kind:   profile.FrontendManaged,
			Binary: bin,
			WaitFor: []profile.WaitForURL{
				{URL: "http://127.0.0.1:8080/", Timeout: 10 * time.Millisecond},
			},
		},
	}
	_, err := d.Activate(context.Background(), p, profile.ExpandContext{})
	if err == nil {
		t.Fatal("expected wait_for timeout error")
	}
	if !strings.Contains(err.Error(), "wait_for") {
		t.Errorf("err = %v, want mention of wait_for", err)
	}
	// Teardown should have been invoked — the process is expected to have
	// received SIGINT and exited.
	proc.mu.Lock()
	defer proc.mu.Unlock()
	if !proc.exited {
		t.Errorf("process should have exited after wait_for timeout")
	}
	if len(proc.signals) == 0 || proc.signals[0] != syscall.SIGINT {
		t.Errorf("signals = %v, want first signal SIGINT", proc.signals)
	}
}

func TestManaged_Activate_WaitFor503Then200(t *testing.T) {
	bin := writeManagedBinary(t)
	proc := newFakeProc(1)
	_, starter := fakeStarter(proc, nil)

	results := []probeResult{
		{status: 503},
		{status: 503},
		{status: 200},
	}
	idx := 0
	d := &managedDriver{
		startProcess: starter,
		waitPoller: testPoller(func(context.Context, string) (int, error) {
			r := results[idx]
			if idx < len(results)-1 {
				idx++
			}
			return r.status, r.err
		}),
		gracefulShutdown: 100 * time.Millisecond,
	}
	p := &profile.Profile{
		Name: "x",
		Frontend: profile.Frontend{
			Kind:   profile.FrontendManaged,
			Binary: bin,
			WaitFor: []profile.WaitForURL{
				{URL: "http://127.0.0.1:8080/", Timeout: time.Second},
			},
		},
	}
	if _, err := d.Activate(context.Background(), p, profile.ExpandContext{}); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if idx < 2 {
		t.Errorf("idx = %d, want at least 2 503s before 200", idx)
	}
}

func TestManaged_Deactivate_GracefulStop(t *testing.T) {
	bin := writeManagedBinary(t)
	proc := newFakeProc(99)
	_, starter := fakeStarter(proc, nil)

	d := &managedDriver{
		startProcess:     starter,
		waitPoller:       testPoller(func(context.Context, string) (int, error) { return 200, nil }),
		gracefulShutdown: time.Second,
	}
	p := &profile.Profile{
		Name: "x",
		Frontend: profile.Frontend{
			Kind:   profile.FrontendManaged,
			Binary: bin,
		},
	}
	r, err := d.Activate(context.Background(), p, profile.ExpandContext{})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}

	if err := Deactivate(context.Background(), r); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	proc.mu.Lock()
	defer proc.mu.Unlock()
	if !proc.exited {
		t.Errorf("process did not exit after graceful Deactivate")
	}
	if proc.killed {
		t.Errorf("SIGKILL should not have been needed when child exits on SIGINT")
	}
	if len(proc.signals) != 1 || proc.signals[0] != syscall.SIGINT {
		t.Errorf("signals = %v, want exactly [SIGINT]", proc.signals)
	}
}

func TestManaged_Deactivate_KillFallback(t *testing.T) {
	bin := writeManagedBinary(t)
	proc := newFakeProc(99)
	proc.ignoreSIGINT = true
	_, starter := fakeStarter(proc, nil)

	d := &managedDriver{
		startProcess:     starter,
		waitPoller:       testPoller(func(context.Context, string) (int, error) { return 200, nil }),
		gracefulShutdown: 50 * time.Millisecond,
	}
	p := &profile.Profile{
		Name: "x",
		Frontend: profile.Frontend{
			Kind:   profile.FrontendManaged,
			Binary: bin,
		},
	}
	r, err := d.Activate(context.Background(), p, profile.ExpandContext{})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	start := time.Now()
	if err := Deactivate(context.Background(), r); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	elapsed := time.Since(start)
	// We should have waited the full graceful budget before SIGKILL.
	if elapsed < 40*time.Millisecond {
		t.Errorf("Deactivate returned in %v; expected >= ~50ms graceful timeout", elapsed)
	}
	proc.mu.Lock()
	defer proc.mu.Unlock()
	if !proc.killed {
		t.Errorf("expected SIGKILL after SIGINT timeout")
	}
}

func TestManaged_Deactivate_NilSafe(t *testing.T) {
	if err := Deactivate(context.Background(), nil); err != nil {
		t.Errorf("Deactivate(nil): %v", err)
	}
	if err := Deactivate(context.Background(), &Result{Kind: profile.FrontendManaged}); err != nil {
		t.Errorf("Deactivate(Result without teardown): %v", err)
	}
}

// --------------------------------------------------------------------------
// Integration test against the real testdata/fake-managed binary. Built
// once in TestMain and reused. Asserts the SIGKILL-fallback path: a child
// that ignores SIGINT gets SIGKILLed after gracefulShutdown.
// --------------------------------------------------------------------------

var fakeManagedBinary string

func TestMain(m *testing.M) {
	bin := filepath.Join(os.TempDir(), fmt.Sprintf("vibe-fake-managed-%d", os.Getpid()))
	cmd := exec.Command("go", "build", "-o", bin, "../../../testdata/fake-managed")
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintln(os.Stderr, "build fake-managed:", err, string(out))
		os.Exit(1)
	}
	fakeManagedBinary = bin
	code := m.Run()
	_ = os.Remove(bin)
	os.Exit(code)
}

func TestManaged_RealBinary_GracefulStop(t *testing.T) {
	port, err := supervisor.PickFreePort()
	if err != nil {
		t.Fatalf("pickFreePort: %v", err)
	}
	d := defaultManaged()
	d.pollInterval = 10 * time.Millisecond
	d.gracefulShutdown = 2 * time.Second

	p := &profile.Profile{
		Name: "real",
		Frontend: profile.Frontend{
			Kind:   profile.FrontendManaged,
			Binary: fakeManagedBinary,
			Args:   []string{"--listen", "127.0.0.1", "--port", strconv.Itoa(port)},
			WaitFor: []profile.WaitForURL{
				{URL: fmt.Sprintf("http://127.0.0.1:%d/", port), Timeout: 5 * time.Second},
			},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	r, err := d.Activate(ctx, p, profile.ExpandContext{})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	t.Cleanup(func() { _ = Deactivate(context.Background(), r) })

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err := Deactivate(stopCtx, r); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
}

func TestManaged_RealBinary_KillFallback(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: waits for gracefulShutdown before SIGKILL")
	}
	port, err := supervisor.PickFreePort()
	if err != nil {
		t.Fatalf("pickFreePort: %v", err)
	}
	d := defaultManaged()
	d.pollInterval = 10 * time.Millisecond
	d.gracefulShutdown = 1 * time.Second // short, to keep the test snappy

	p := &profile.Profile{
		Name: "real",
		Frontend: profile.Frontend{
			Kind:   profile.FrontendManaged,
			Binary: fakeManagedBinary,
			Args: []string{
				"--listen", "127.0.0.1",
				"--port", strconv.Itoa(port),
				"--ignore-sigint",
			},
			WaitFor: []profile.WaitForURL{
				{URL: fmt.Sprintf("http://127.0.0.1:%d/", port), Timeout: 5 * time.Second},
			},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	r, err := d.Activate(ctx, p, profile.ExpandContext{})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	t.Cleanup(func() { _ = Deactivate(context.Background(), r) })

	start := time.Now()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), d.gracefulShutdown+3*time.Second)
	defer stopCancel()
	if err := Deactivate(stopCtx, r); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < d.gracefulShutdown-200*time.Millisecond {
		t.Errorf("Deactivate returned in %v; expected ~%v (full graceful budget)", elapsed, d.gracefulShutdown)
	}
	if elapsed > d.gracefulShutdown+2*time.Second {
		t.Errorf("Deactivate took %v; expected close to %v", elapsed, d.gracefulShutdown)
	}
}

func TestManaged_RealBinary_EnvAndWorkdirPropagate(t *testing.T) {
	port, err := supervisor.PickFreePort()
	if err != nil {
		t.Fatalf("pickFreePort: %v", err)
	}
	workdir := t.TempDir()
	d := defaultManaged()
	d.pollInterval = 10 * time.Millisecond
	d.gracefulShutdown = 1 * time.Second

	p := &profile.Profile{
		Name: "real",
		Frontend: profile.Frontend{
			Kind:    profile.FrontendManaged,
			Binary:  fakeManagedBinary,
			Workdir: workdir,
			Env: map[string]string{
				"OPEN_WEBUI_DATA": "/var/lib/open-webui",
			},
			Args: []string{
				"--listen", "127.0.0.1",
				"--port", strconv.Itoa(port),
			},
			WaitFor: []profile.WaitForURL{
				{URL: fmt.Sprintf("http://127.0.0.1:%d/", port), Timeout: 5 * time.Second},
			},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := d.Activate(ctx, p, profile.ExpandContext{})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	t.Cleanup(func() { _ = Deactivate(context.Background(), r) })
	if r.Env["OPEN_WEBUI_DATA"] != "/var/lib/open-webui" {
		t.Errorf("Env OPEN_WEBUI_DATA = %q", r.Env["OPEN_WEBUI_DATA"])
	}
}
