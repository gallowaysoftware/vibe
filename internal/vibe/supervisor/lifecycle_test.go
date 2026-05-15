package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

var fakeBinary string

func TestMain(m *testing.M) {
	bin := filepath.Join(os.TempDir(), fmt.Sprintf("vibe-fake-llama-supervisor-%d", os.Getpid()))
	cmd := exec.Command("go", "build", "-o", bin, "../../../testdata/fake-llama-server")
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintln(os.Stderr, "build fake:", err, string(out))
		os.Exit(1)
	}
	fakeBinary = bin
	code := m.Run()
	_ = os.Remove(bin)
	os.Exit(code)
}

// llamaSpec builds a minimal LaunchSpec that points at the fake llama-server
// binary built in TestMain. Extra args (e.g. --ready-after, --never-ready) can
// be appended to drive specific behavior in individual tests.
func llamaSpec(t *testing.T, port int, extra ...string) LaunchSpec {
	t.Helper()
	stub := filepath.Join(t.TempDir(), "model.gguf")
	if err := os.WriteFile(stub, []byte("x"), 0o644); err != nil {
		t.Fatalf("write stub model: %v", err)
	}
	args := []string{
		"--model", stub,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(port),
		"--alias", "fake",
		"--ctx-size", "1024",
		"--parallel", "1",
	}
	args = append(args, extra...)
	return LaunchSpec{
		Binary:    fakeBinary,
		Args:      args,
		HealthURL: fmt.Sprintf("http://127.0.0.1:%d/health", port),
	}
}

// freshPort allocates a localhost port for a single test. Caller passes it
// into llamaSpec so the spec's Args and HealthURL agree on the port.
func freshPort(t *testing.T) int {
	t.Helper()
	p, err := pickFreePort()
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	return p
}

func TestSupervisor_StartReady(t *testing.T) {
	s := New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	port := freshPort(t)
	if err := s.Start(ctx, llamaSpec(t, port), port); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })

	st := s.Status()
	if st.State != StateReady {
		t.Errorf("state = %v, want ready", st.State)
	}
	if st.PID == 0 {
		t.Errorf("PID == 0; want non-zero")
	}
	if st.Addr == "" {
		t.Errorf("addr is empty")
	}

	if err := s.Stop(context.Background()); err != nil {
		t.Errorf("Stop: %v", err)
	}
}

func TestSupervisor_StartWaitsForReady(t *testing.T) {
	s := New()
	port := freshPort(t)
	spec := llamaSpec(t, port, "--ready-after", "1s")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	if err := s.Start(ctx, spec, port); err != nil {
		t.Fatalf("Start: %v", err)
	}
	elapsed := time.Since(start)
	t.Cleanup(func() { _ = s.Stop(context.Background()) })

	if elapsed < 900*time.Millisecond {
		t.Errorf("Start returned in %v; expected ~1s wait", elapsed)
	}
	if s.Status().State != StateReady {
		t.Errorf("state = %v, want ready", s.Status().State)
	}
}

func TestSupervisor_StartContextTimeout(t *testing.T) {
	s := New()
	port := freshPort(t)
	spec := llamaSpec(t, port, "--never-ready")

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := s.Start(ctx, spec, port)
	t.Cleanup(func() { _ = s.Stop(context.Background()) })

	if err == nil {
		t.Fatalf("Start returned nil; expected deadline error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Start err = %v; want context.DeadlineExceeded", err)
	}
}

func TestSupervisor_PrematureExit(t *testing.T) {
	s := New()
	port := freshPort(t)
	// exit fast and never become ready
	spec := llamaSpec(t, port, "--exit-after", "100ms", "--never-ready")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := s.Start(ctx, spec, port)
	if err == nil {
		t.Fatalf("Start returned nil; expected premature-exit error")
	}
	if !strings.Contains(err.Error(), "exited before becoming ready") {
		t.Errorf("Start err = %q; want \"exited before becoming ready\"", err.Error())
	}
}

func TestSupervisor_GracefulStop(t *testing.T) {
	s := New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	port := freshPort(t)
	if err := s.Start(ctx, llamaSpec(t, port), port); err != nil {
		t.Fatalf("Start: %v", err)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err := s.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	st := s.Status()
	if st.State != StateExited {
		t.Errorf("state = %v, want exited", st.State)
	}
	// Either nil (clean shutdown via ListenAndServe error path returning) or a
	// signal-driven non-zero exit; both are acceptable. We just want the
	// supervisor to have surfaced *some* result and transitioned to Exited.
	_ = st.ExitErr
}

func TestSupervisor_KillFallback(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: waits for gracefulShutdown before SIGKILL")
	}
	s := New()
	port := freshPort(t)
	spec := llamaSpec(t, port, "--ignore-sigint")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Start(ctx, spec, port); err != nil {
		t.Fatalf("Start: %v", err)
	}

	start := time.Now()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), gracefulShutdown+5*time.Second)
	defer stopCancel()
	if err := s.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed < gracefulShutdown-500*time.Millisecond {
		t.Errorf("Stop returned in %v; expected ~%v (graceful timeout before SIGKILL)", elapsed, gracefulShutdown)
	}
	if elapsed > gracefulShutdown+3*time.Second {
		t.Errorf("Stop took %v; expected close to %v", elapsed, gracefulShutdown)
	}
	if s.Status().State != StateExited {
		t.Errorf("state = %v, want exited", s.Status().State)
	}
}

func TestSupervisor_LogsBuffered(t *testing.T) {
	s := New()
	port := freshPort(t)
	spec := llamaSpec(t, port, "--log-stuff")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Start(ctx, spec, port); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Give pumpLogs a tick to drain.
	time.Sleep(150 * time.Millisecond)
	_ = s.Stop(context.Background())

	lines := s.Logs()
	if len(lines) == 0 {
		t.Fatalf("Logs() returned 0 lines; expected fake stub output")
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "fake llama-server: starting on") {
		t.Errorf("logs missing stdout marker; got:\n%s", joined)
	}
}

// TestSupervisor_ParameterizedHealthURL verifies the supervisor can drive a
// non-default health URL (the ComfyUI path, /system_stats). We piggy-back on
// the fake llama-server by serving the same handler at /system_stats via the
// supervisor's HealthURL hook — but the fake only knows /health, so we use a
// tiny inline stub instead.
func TestSupervisor_NonHealthPathPolling(t *testing.T) {
	// Sanity-check the supervisor honors LaunchSpec.HealthURL by pointing at
	// a path the fake llama-server *does not* implement, then confirming the
	// supervisor times out instead of returning ready. (If the supervisor
	// were hard-coded to /health it would falsely report ready here.)
	s := New()
	port := freshPort(t)
	spec := llamaSpec(t, port)
	spec.HealthURL = fmt.Sprintf("http://127.0.0.1:%d/system_stats", port)

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	err := s.Start(ctx, spec, port)
	t.Cleanup(func() { _ = s.Stop(context.Background()) })

	if err == nil {
		t.Fatal("Start succeeded; expected timeout because /system_stats is a 404 on the fake server")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v; want DeadlineExceeded", err)
	}
}
