// Package supervisor manages a single llama-server child process: launches it
// with the right flags, waits for its /health endpoint to report ready,
// captures stdout/stderr into a ring buffer, and shuts it down gracefully.
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
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
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
	binary string

	mu      sync.Mutex
	cmd     *exec.Cmd
	state   State
	addr    string
	started time.Time
	exitErr error
	logs    *ringBuffer
	stopped chan struct{}
}

// New returns a Supervisor that will exec the given binary (defaults to
// "llama-server" if empty).
func New(binary string) *Supervisor {
	if binary == "" {
		binary = "llama-server"
	}
	return &Supervisor{binary: binary, logs: newRingBuffer(logRingCapacity)}
}

// Start launches llama-server. Returns when /health reports 200 or ctx is
// canceled. The supervisor picks a free localhost port for the child.
func (s *Supervisor) Start(ctx context.Context, p *profile.Profile) error {
	s.mu.Lock()
	if s.state == StateStarting || s.state == StateReady {
		s.mu.Unlock()
		return errors.New("supervisor: already running")
	}
	port, err := pickFreePort()
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("pick port: %w", err)
	}
	args := BuildArgs(p, port)
	cmd := exec.Command(s.binary, args...)
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
		return fmt.Errorf("start %s: %w", s.binary, err)
	}

	s.cmd = cmd
	s.state = StateStarting
	s.addr = fmt.Sprintf("http://127.0.0.1:%d", port)
	s.started = time.Now()
	s.exitErr = nil
	s.stopped = make(chan struct{})
	s.logs.Reset()
	s.mu.Unlock()

	slog.Info("llama-server starting",
		"binary", s.binary, "pid", cmd.Process.Pid, "addr", s.addr, "model", p.Model.Path)

	go s.pumpLogs(stdout)
	go s.pumpLogs(stderr)
	go s.waitExit()

	if err := s.waitReady(ctx, port); err != nil {
		_ = s.Stop(context.Background())
		return err
	}
	s.mu.Lock()
	s.state = StateReady
	s.mu.Unlock()
	slog.Info("llama-server ready",
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

func (s *Supervisor) waitReady(ctx context.Context, port int) error {
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
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
			return fmt.Errorf("llama-server exited before becoming ready: %v", exitErr)
		case <-tick.C:
		}
		resp, err := client.Get(url)
		if err != nil {
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
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
		slog.Error("llama-server exited before ready", "err", err)
	} else {
		slog.Info("llama-server exited", "err", err)
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

// BuildArgs converts a profile's Model spec into llama-server CLI arguments.
// The proxy is the public face; the child always listens on 127.0.0.1.
func BuildArgs(p *profile.Profile, port int) []string {
	args := []string{
		"--model", p.Model.Path,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(port),
		"--alias", p.Model.Alias,
		"--ctx-size", strconv.Itoa(p.Model.Context),
		"--parallel", strconv.Itoa(p.Model.Parallel),
	}
	if p.Model.GPULayers > 0 {
		args = append(args, "--n-gpu-layers", strconv.Itoa(p.Model.GPULayers))
	}
	if p.Model.FlashAttn {
		args = append(args, "--flash-attn", "on")
	}
	if p.Model.CacheTypeK != "" {
		args = append(args, "--cache-type-k", p.Model.CacheTypeK)
	}
	if p.Model.CacheTypeV != "" {
		args = append(args, "--cache-type-v", p.Model.CacheTypeV)
	}
	if p.Model.Jinja {
		args = append(args, "--jinja")
	}
	args = append(args, p.Model.ExtraArgs...)
	return args
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
