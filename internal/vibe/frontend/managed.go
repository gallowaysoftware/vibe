package frontend

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
)

// managedGracefulShutdown is how long the managed driver waits for a child
// to exit after SIGINT before falling back to SIGKILL. Matches the value
// used by the backend supervisor for llama-server so users see a single
// consistent shutdown budget across both surfaces.
const managedGracefulShutdown = 10 * time.Second

// managedProc is the slice of `*exec.Cmd` the managed driver actually
// relies on. Pulling it behind an interface lets tests inject a fake
// process — same trick composeDriver's runCommand uses — so we can
// exercise the SIGINT-then-SIGKILL path without spawning a real binary.
type managedProc interface {
	// PID returns the child PID (0 if the process never started).
	PID() int
	// Signal forwards sig to the child.
	Signal(sig os.Signal) error
	// Kill sends SIGKILL.
	Kill() error
	// Wait blocks until the child exits and returns the wait error (if
	// any). It must be safe to call exactly once; subsequent calls return
	// the cached result.
	Wait() error
}

// managedDriver supervises a native binary as the frontend for a profile.
// Shape mirrors composeDriver — startProcess and probeURL are injectable
// so unit tests can assert argv / env / URL polling without spawning real
// binaries or making real network calls.
type managedDriver struct {
	// startProcess execs name with args, env, and workdir, and returns a
	// handle the driver can signal/wait on. In production this wraps
	// *exec.Cmd; tests inject a fake.
	startProcess func(ctx context.Context, name string, args []string, env []string, workdir string) (managedProc, error)

	// probeURL issues a single GET against url and returns the response
	// status code. The default uses net/http (httpProbe, shared with
	// composeDriver).
	probeURL func(ctx context.Context, url string) (int, error)

	// now is wired so tests can advance time deterministically; defaults
	// to time.Now.
	now func() time.Time

	// defaultTimeout applies when a wait_for entry has no explicit
	// timeout. Matches composeDriver's default for consistency.
	defaultTimeout time.Duration

	// pollInterval is the gap between wait_for polls. Tests override this
	// to spin faster.
	pollInterval time.Duration

	// gracefulShutdown is how long teardown waits between SIGINT and
	// SIGKILL. Exposed for tests; production uses managedGracefulShutdown.
	gracefulShutdown time.Duration
}

// defaultManaged returns a managedDriver configured for production:
// real exec.Command starts, real net/http probes, real time.
func defaultManaged() *managedDriver {
	return &managedDriver{
		startProcess:     execStartProcess,
		probeURL:         httpProbe,
		now:              time.Now,
		defaultTimeout:   60 * time.Second,
		pollInterval:     500 * time.Millisecond,
		gracefulShutdown: managedGracefulShutdown,
	}
}

// execStartProcess is the production startProcess. It builds an exec.Cmd,
// streams stdout/stderr to the daemon's standard handles (matching the
// supervisor for consistency), and starts the child.
func execStartProcess(ctx context.Context, name string, args []string, env []string, workdir string) (managedProc, error) {
	cmd := exec.Command(name, args...)
	if workdir != "" {
		cmd.Dir = workdir
	}
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return newExecProc(cmd), nil
}

// execProc wraps *exec.Cmd so the cached Wait() result can be returned to
// multiple callers (the wait_for poll, the teardown). exec.Cmd.Wait is
// only safe to call once.
type execProc struct {
	cmd      *exec.Cmd
	waitOnce sync.Once
	waitErr  error
	done     chan struct{}
}

func newExecProc(cmd *exec.Cmd) *execProc {
	return &execProc{cmd: cmd, done: make(chan struct{})}
}

func (e *execProc) PID() int {
	if e.cmd == nil || e.cmd.Process == nil {
		return 0
	}
	return e.cmd.Process.Pid
}

func (e *execProc) Signal(sig os.Signal) error {
	if e.cmd == nil || e.cmd.Process == nil {
		return errors.New("managed: no process")
	}
	return e.cmd.Process.Signal(sig)
}

func (e *execProc) Kill() error {
	if e.cmd == nil || e.cmd.Process == nil {
		return errors.New("managed: no process")
	}
	return e.cmd.Process.Kill()
}

func (e *execProc) Wait() error {
	e.waitOnce.Do(func() {
		e.waitErr = e.cmd.Wait()
		close(e.done)
	})
	<-e.done
	return e.waitErr
}

// Activate execs the managed binary with the configured args/env/workdir,
// then polls wait_for URLs. On success the returned Result's teardown
// stops the child gracefully (SIGINT, then SIGKILL after
// gracefulShutdown).
func (m *managedDriver) Activate(reqCtx context.Context, p *profile.Profile, ctx profile.ExpandContext) (*Result, error) {
	if p.Frontend.Binary == "" {
		return nil, errors.New("managed: frontend.binary is required")
	}

	env, err := p.ExpandEnv(ctx)
	if err != nil {
		return nil, fmt.Errorf("expand env: %w", err)
	}
	envSlice := envMapToSlice(env)

	args := append([]string(nil), p.Frontend.Args...)

	proc, err := m.startProcess(reqCtx, p.Frontend.Binary, args, envSlice, p.Frontend.Workdir)
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", p.Frontend.Binary, err)
	}

	slog.Info("frontend managed: starting",
		"binary", p.Frontend.Binary,
		"pid", proc.PID(),
		"workdir", p.Frontend.Workdir,
	)

	if err := m.waitForReady(reqCtx, p.Frontend.WaitFor); err != nil {
		// Best-effort teardown: don't leave a half-started child behind
		// when wait_for never goes ready.
		tdCtx, cancel := context.WithTimeout(context.Background(), m.gracefulShutdown+5*time.Second)
		_ = m.stopProcess(tdCtx, proc)
		cancel()
		return nil, err
	}

	teardown := func(tdCtx context.Context) error {
		return m.stopProcess(tdCtx, proc)
	}

	return &Result{
		// No file is written for managed; surface the binary path so
		// `vibe status` has something useful to display.
		WroteFile:       p.Frontend.Binary,
		RestartRequired: p.Frontend.RestartRequired,
		Env:             env,
		Kind:            profile.FrontendManaged,
		teardown:        teardown,
	}, nil
}

// stopProcess sends SIGINT, waits up to gracefulShutdown for the child to
// exit, and falls back to SIGKILL if it doesn't. Modeled on
// supervisor.Stop so the user-facing shutdown behavior is consistent
// between the backend supervisor and the managed-frontend driver.
func (m *managedDriver) stopProcess(ctx context.Context, proc managedProc) error {
	if proc == nil {
		return nil
	}
	if err := proc.Signal(syscall.SIGINT); err != nil && !errors.Is(err, syscall.ESRCH) {
		// Process already gone? Treat that as a clean stop.
		return nil
	}

	// Run Wait in a goroutine so we can race it against the graceful
	// timeout / ctx cancellation.
	done := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = proc.Wait()
		close(done)
	}()

	timer := time.NewTimer(m.gracefulShutdown)
	defer timer.Stop()

	select {
	case <-done:
		_ = waitErr
		return nil
	case <-timer.C:
		_ = proc.Kill()
		<-done
		return nil
	case <-ctx.Done():
		_ = proc.Kill()
		<-done
		return ctx.Err()
	}
}

// waitForReady polls each wait_for URL until it returns 2xx or its
// timeout (or reqCtx) expires. Sequential, same as composeDriver, so the
// error message can name the URL that failed.
func (m *managedDriver) waitForReady(reqCtx context.Context, urls []profile.WaitForURL) error {
	for _, w := range urls {
		timeout := w.Timeout
		if timeout <= 0 {
			timeout = m.defaultTimeout
		}
		if err := m.pollURL(reqCtx, w.URL, timeout); err != nil {
			return err
		}
	}
	return nil
}

// pollURL hits url at m.pollInterval cadence until it 2xxs, or the
// deadline / reqCtx fires. Same shape as composeDriver.pollURL — kept
// separate to avoid coupling the two drivers' lifecycles.
func (m *managedDriver) pollURL(reqCtx context.Context, url string, timeout time.Duration) error {
	deadline := m.now().Add(timeout)
	var lastStatus int
	var lastErr error
	for {
		if err := reqCtx.Err(); err != nil {
			return fmt.Errorf("wait_for %s: %w", url, err)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return managedWaitErr(url, lastStatus, lastErr)
		}
		probeCtx, cancel := context.WithTimeout(reqCtx, minDuration(remaining, 5*time.Second))
		status, err := m.probeURL(probeCtx, url)
		cancel()
		if err == nil && status >= 200 && status < 300 {
			return nil
		}
		lastStatus = status
		lastErr = err
		if m.now().After(deadline) {
			return managedWaitErr(url, lastStatus, lastErr)
		}
		select {
		case <-reqCtx.Done():
			return fmt.Errorf("wait_for %s: %w", url, reqCtx.Err())
		case <-time.After(m.pollInterval):
		}
	}
}

func managedWaitErr(url string, status int, lastErr error) error {
	if lastErr != nil {
		return fmt.Errorf("wait_for %s: timeout (last err: %v)", url, lastErr)
	}
	if status != 0 {
		return fmt.Errorf("wait_for %s: timeout (last status: %d)", url, status)
	}
	return fmt.Errorf("wait_for %s: timeout", url)
}
