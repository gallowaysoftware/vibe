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
// Shape mirrors composeDriver — startProcess and the embedded waitPoller
// are injectable so unit tests can assert argv / env / URL polling without
// spawning real binaries or making real network calls.
type managedDriver struct {
	// startProcess execs name with args, env, and workdir, and returns a
	// handle the driver can signal/wait on. In production this wraps
	// *exec.Cmd; tests inject a fake.
	startProcess func(ctx context.Context, name string, args []string, env []string, workdir string) (managedProc, error)

	waitPoller

	// gracefulShutdown is how long teardown waits between SIGINT and
	// SIGKILL. Exposed for tests; production uses managedGracefulShutdown.
	gracefulShutdown time.Duration
}

// defaultManaged returns a managedDriver configured for production:
// real exec.Command starts, real net/http probes, real time.
func defaultManaged() *managedDriver {
	return &managedDriver{
		startProcess: execStartProcess,
		waitPoller: waitPoller{
			driver:         "managed",
			probeURL:       httpProbe,
			now:            time.Now,
			defaultTimeout: 60 * time.Second,
			pollInterval:   500 * time.Millisecond,
		},
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

	// Render write_file/template if the profile supplied them (opencode and
	// similar tools need both a config file and a launched process). Empty
	// write_file is fine — writeFrontendConfig is a no-op then and we still
	// get the expanded env back.
	wroteFile, env, err := writeFrontendConfig(p, &ctx)
	if err != nil {
		return nil, err
	}
	envSlice := envMapToSlice(env)

	// Args feed exec, so they get the same ${VAR} expansion env values and
	// write_file templates receive — otherwise "vibe-local/${MODEL_ALIAS}"
	// would reach the binary as a literal and fail only at first use.
	// Expanded after writeFrontendConfig so ${WRITE_FILE} resolves too.
	args := make([]string, len(p.Frontend.Args))
	for i, a := range p.Frontend.Args {
		ea, err := profile.ExpandPathString(a, ctx)
		if err != nil {
			return nil, fmt.Errorf("expand args[%d] %q: %w", i, a, err)
		}
		args[i] = ea
	}

	proc, err := m.startProcess(reqCtx, p.Frontend.Binary, args, envSlice, p.Frontend.Workdir)
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", p.Frontend.Binary, err)
	}

	slog.Info("frontend managed: starting",
		"binary", p.Frontend.Binary,
		"pid", proc.PID(),
		"workdir", p.Frontend.Workdir,
		"wrote_file", wroteFile,
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

	// Surface the config file when one was written; otherwise fall back to
	// the binary path so `vibe status` still has something to display.
	wrote := wroteFile
	if wrote == "" {
		wrote = p.Frontend.Binary
	}
	return &Result{
		WroteFile:       wrote,
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
