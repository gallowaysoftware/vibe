// Package supervisor manages a single local-AI child process: launches it
// with the right argv (driven by a backend-specific LaunchSpec), waits for
// its health URL to report ready, captures stdout/stderr into a ring buffer,
// and shuts it down gracefully.
//
// The supervisor itself is backend-agnostic. Profile-driven argv and
// health-URL derivation lives in the profile package (LlamaServerSpec,
// ComfyUISpec); the supervisor just execs what it's told and polls.
package supervisor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

type State int

const (
	StateStopped State = iota
	StateStarting
	StateReady
	StateExited
)

func (s State) String() string {
	switch s {
	case StateStopped:
		return "stopped"
	case StateStarting:
		return "starting"
	case StateReady:
		return "ready"
	case StateExited:
		return "exited"
	}
	return "unknown"
}

type Status struct {
	State   State
	PID     int
	Addr    string
	Started time.Time
	ExitErr string
}

const (
	healthPollInterval = 250 * time.Millisecond
	gracefulShutdown   = 10 * time.Second
	logRingCapacity    = 1000
)

type Supervisor struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	state   State
	addr    string
	started time.Time
	exitErr error
	logs    *ringBuffer
	stopped chan struct{}
}

// New returns a fresh supervisor with no child running.
func New() *Supervisor {
	return &Supervisor{logs: newRingBuffer(logRingCapacity)}
}

// PickFreePort returns a localhost TCP port currently free, useful for
// callers who need to allocate a port before constructing a LaunchSpec.
func PickFreePort() (int, error) {
	return pickFreePort()
}

// Start launches the child described by spec and blocks until the child's
// HealthURL returns 2xx (then transitions to StateReady) or ctx is canceled.
// `port` is recorded as the public address (the spec already encodes it in
// its Args and HealthURL); the supervisor exposes "http://127.0.0.1:<port>"
// via Status().Addr.
func (s *Supervisor) Start(ctx context.Context, spec LaunchSpec, port int) error {
	if spec.Binary == "" {
		return errors.New("supervisor: LaunchSpec.Binary is required")
	}
	if spec.HealthURL == "" {
		return errors.New("supervisor: LaunchSpec.HealthURL is required")
	}

	s.mu.Lock()
	if s.state == StateStarting || s.state == StateReady {
		s.mu.Unlock()
		return errors.New("supervisor: already running")
	}
	cmd := exec.Command(spec.Binary, spec.Args...)
	if spec.Workdir != "" {
		cmd.Dir = spec.Workdir
	}
	if len(spec.Env) > 0 {
		cmd.Env = append(os.Environ(), spec.Env...)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		s.mu.Unlock()
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if err := cmd.Start(); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("start %s: %w", spec.Binary, err)
	}

	s.cmd = cmd
	s.state = StateStarting
	s.addr = fmt.Sprintf("http://127.0.0.1:%d", port)
	s.started = time.Now()
	s.exitErr = nil
	s.stopped = make(chan struct{})
	s.logs.Reset()
	s.mu.Unlock()

	slog.Info("backend starting",
		"binary", spec.Binary, "pid", cmd.Process.Pid, "addr", s.addr,
		"workdir", spec.Workdir, "health_url", spec.HealthURL)

	go s.pumpLogs(stdout)
	go s.pumpLogs(stderr)
	go s.waitExit()

	if err := s.waitReady(ctx, spec.HealthURL); err != nil {
		_ = s.Stop(context.Background())
		return err
	}
	s.mu.Lock()
	s.state = StateReady
	s.mu.Unlock()
	slog.Info("backend ready",
		"addr", s.addr, "elapsed", time.Since(s.started).Round(time.Millisecond))
	return nil
}

// Stop sends SIGINT, waits up to gracefulShutdown, then SIGKILL.
func (s *Supervisor) Stop(ctx context.Context) error {
	s.mu.Lock()
	cmd := s.cmd
	stopped := s.stopped
	s.mu.Unlock()

	if cmd == nil || cmd.Process == nil || stopped == nil {
		return nil
	}
	select {
	case <-stopped:
		return nil
	default:
	}
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal: %w", err)
	}
	timer := time.NewTimer(gracefulShutdown)
	defer timer.Stop()
	select {
	case <-stopped:
		return nil
	case <-timer.C:
		_ = cmd.Process.Kill()
		<-stopped
		return nil
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-stopped
		return ctx.Err()
	}
}

// Stopped returns a channel that closes when the supervised process
// exits (whether from a Stop call or from the process dying on its own).
// Callers wanting to react to an unexpected crash watch this channel and
// then call Status() to decide whether the exit was a clean Stop or a
// crash mid-life (StateExited after StateReady ⇒ crash).
//
// Returns nil if the supervisor has never been started; callers should
// guard for that.
func (s *Supervisor) Stopped() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

func (s *Supervisor) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	pid := 0
	if s.cmd != nil && s.cmd.Process != nil {
		pid = s.cmd.Process.Pid
	}
	exitMsg := ""
	if s.exitErr != nil {
		exitMsg = s.exitErr.Error()
	}
	return Status{State: s.state, PID: pid, Addr: s.addr, Started: s.started, ExitErr: exitMsg}
}

func (s *Supervisor) Logs() []string {
	return s.logs.Lines()
}

func (s *Supervisor) waitReady(ctx context.Context, healthURL string) error {
	client := &http.Client{Timeout: 2 * time.Second}
	tick := time.NewTicker(healthPollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.stopped:
			s.mu.Lock()
			exitErr := s.exitErr
			s.mu.Unlock()
			return fmt.Errorf("backend exited before becoming ready: %v", exitErr)
		case <-tick.C:
		}
		resp, err := client.Get(healthURL)
		if err != nil {
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
	}
}

func (s *Supervisor) pumpLogs(r io.ReadCloser) {
	defer r.Close()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		s.logs.Add(scanner.Text())
	}
}

func (s *Supervisor) waitExit() {
	err := s.cmd.Wait()
	s.mu.Lock()
	s.exitErr = err
	wasReady := s.state == StateReady
	s.state = StateExited
	close(s.stopped)
	s.mu.Unlock()
	if err != nil && !wasReady {
		slog.Error("backend exited before ready", "err", err)
	} else {
		slog.Info("backend exited", "err", err)
	}
}

func pickFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// ringBuffer is a fixed-capacity FIFO of log lines.
type ringBuffer struct {
	mu    sync.Mutex
	buf   []string
	head  int
	count int
}

func newRingBuffer(n int) *ringBuffer { return &ringBuffer{buf: make([]string, n)} }

func (r *ringBuffer) Add(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.head] = line
	r.head = (r.head + 1) % len(r.buf)
	if r.count < len(r.buf) {
		r.count++
	}
}

func (r *ringBuffer) Lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, r.count)
	start := r.head - r.count
	if start < 0 {
		start += len(r.buf)
	}
	for i := 0; i < r.count; i++ {
		out[i] = r.buf[(start+i)%len(r.buf)]
	}
	return out
}

func (r *ringBuffer) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.head = 0
	r.count = 0
}
