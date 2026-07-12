package vamp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// writeJobDir builds a synthetic job-style run dir layout under root.
// pid: when > 0 we write that value to vamp.pid; -1 leaves the file
// absent; 0 writes an empty file (exercising the malformed-pid path).
// rec: when non-nil, marshaled to pipeline.json.
// log: when non-empty, written as vamp.log.
func writeJobDir(t *testing.T, root, base string, pid int, rec *RunRecord, log string) string {
	t.Helper()
	dir := filepath.Join(root, base)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	switch {
	case pid > 0:
		if err := WritePidFile(dir, pid); err != nil {
			t.Fatalf("write pid file: %v", err)
		}
	case pid == 0:
		if err := os.WriteFile(filepath.Join(dir, PidFileName), nil, 0o644); err != nil {
			t.Fatalf("write empty pid file: %v", err)
		}
	}
	if rec != nil {
		data, err := json.MarshalIndent(rec, "", "  ")
		if err != nil {
			t.Fatalf("marshal rec: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "pipeline.json"), data, 0o644); err != nil {
			t.Fatalf("write pipeline.json: %v", err)
		}
	}
	if log != "" {
		if err := os.WriteFile(filepath.Join(dir, LogFileName), []byte(log), 0o644); err != nil {
			t.Fatalf("write log: %v", err)
		}
	}
	return dir
}

// TestListJobs_Empty exercises the "no runs dir" early-return so we don't
// regress on the friendliness of `vamp jobs ls` on a fresh machine.
func TestListJobs_Empty(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	got, err := ListJobs(filepath.Join(tmp, "nope"))
	if err != nil {
		t.Fatalf("ListJobs(missing) err = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Fatalf("ListJobs(missing) returned %d, want 0", len(got))
	}
	// Existing dir, no job-shaped subdirs (just a non-daemon-mode dir
	// with neither pid/log/json). Should also return empty.
	if err := os.MkdirAll(filepath.Join(tmp, "exists", "foreground-only"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err = ListJobs(filepath.Join(tmp, "exists"))
	if err != nil {
		t.Fatalf("ListJobs(no-markers) err = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Fatalf("ListJobs(no-markers) returned %d, want 0 (dirs without pid/log/json should be filtered)", len(got))
	}
}

// TestListJobs_StateDerivation covers the JobState matrix: a Running job
// (live pid), a Finished job (no pid, terminal pipeline.json), and a
// Crashed job (stale pid, no pipeline.json). Each row also asserts the
// embedded RunSummary fields propagate so we don't drift from `runs ls`.
func TestListJobs_StateDerivation(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	startB := time.Date(2026, 5, 16, 10, 0, 0, 0, time.Local)

	// Running: pid alive (use our own pid), no terminal record yet.
	writeJobDir(t, tmp, "2026-05-16T09-00-00_a", os.Getpid(), nil, "starting up\n")
	// Finished: pipeline.json present with status="ok", no pid file.
	writeJobDir(t, tmp, "2026-05-16T10-00-00_b", -1, &RunRecord{
		Name:      "b",
		StartTime: startB,
		EndTime:   startB.Add(30 * time.Second),
		Status:    "ok",
		Stages:    []StageRecord{{ID: "x", DurationMS: 100, Status: "ok"}},
	}, "ok\n")
	// Crashed: a recorded pid that we just reaped. Pid recycling is
	// theoretically possible but extremely unlikely on the short test
	// runtime, and even then we'd just falsely report "running" — the
	// state machine itself is what we're validating here.
	deadPid := deadlyPid(t)
	writeJobDir(t, tmp, "2026-05-16T11-00-00_c", deadPid, nil, "")

	got, err := ListJobs(tmp)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ListJobs returned %d jobs, want 3", len(got))
	}
	// Sorted newest-first.
	byID := make(map[string]JobSummary, len(got))
	for _, j := range got {
		byID[j.ID] = j
	}
	if j := byID["2026-05-16T09-00-00_a"]; j.State != JobStateRunning {
		t.Errorf("job a state = %q, want %q", j.State, JobStateRunning)
	} else if j.PID != os.Getpid() {
		t.Errorf("job a pid = %d, want %d", j.PID, os.Getpid())
	}
	if j := byID["2026-05-16T10-00-00_b"]; j.State != JobStateFinished {
		t.Errorf("job b state = %q, want %q", j.State, JobStateFinished)
	} else if j.Status != "ok" {
		t.Errorf("job b status = %q, want ok", j.Status)
	}
	if j := byID["2026-05-16T11-00-00_c"]; j.State != JobStateCrashed {
		t.Errorf("job c state = %q, want %q", j.State, JobStateCrashed)
	}
	// Newest-first ordering: c (11:00) > b (10:00) > a (09:00).
	if got[0].ID != "2026-05-16T11-00-00_c" {
		t.Errorf("got[0].ID = %q, want crashed-c first (newest)", got[0].ID)
	}
}

// TestListJobs_PidAliveButPipelineJSONTerminal exercises the
// "stale pid file after a clean run" edge case: writePipelineJSON
// landed but RemovePidFile failed for some reason. We treat such a
// dir as Finished (the pipeline record is authoritative).
func TestListJobs_StalePidWithTerminalRecord(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	deadPid := deadlyPid(t)
	writeJobDir(t, tmp, "2026-05-16T12-00-00_d", deadPid, &RunRecord{
		Name:   "d",
		Status: "canceled",
	}, "canceled run\n")
	got, err := ListJobs(tmp)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if got[0].State != JobStateFinished {
		t.Errorf("State = %q, want %q (stale pid + terminal record)", got[0].State, JobStateFinished)
	}
	if got[0].Status != "canceled" {
		t.Errorf("Status = %q, want canceled", got[0].Status)
	}
}

// TestFindJobByPrefix_Cases covers the prefix-match grammar: exact,
// ambiguous, unknown, and path-shaped inputs.
func TestFindJobByPrefix_Cases(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	writeJobDir(t, tmp, "2026-05-16T09-00-00_alpha", os.Getpid(), nil, "")
	writeJobDir(t, tmp, "2026-05-16T09-00-01_beta", os.Getpid(), nil, "")

	// Unique prefix → exactly that one.
	got, err := FindJobByPrefix(tmp, "2026-05-16T09-00-00")
	if err != nil {
		t.Fatalf("unique prefix err = %v", err)
	}
	if got.ID != "2026-05-16T09-00-00_alpha" {
		t.Errorf("unique prefix got ID %q", got.ID)
	}
	// Ambiguous prefix → AmbiguousError with both candidates.
	_, err = FindJobByPrefix(tmp, "2026-05-16T09")
	if err == nil {
		t.Fatal("ambiguous prefix err = nil, want AmbiguousError")
	}
	var amb *AmbiguousError
	if !errors.As(err, &amb) {
		t.Fatalf("ambiguous prefix err type %T, want *AmbiguousError", err)
	}
	if len(amb.Candidates) != 2 {
		t.Errorf("AmbiguousError.Candidates = %v, want 2", amb.Candidates)
	}
	// Unknown prefix → NotFoundError.
	_, err = FindJobByPrefix(tmp, "9999")
	if err == nil {
		t.Fatal("unknown prefix err = nil, want NotFoundError")
	}
	var nf *NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("unknown prefix err type %T, want *NotFoundError", err)
	}
	// Path-shaped → taken verbatim and summarized.
	path := filepath.Join(tmp, "2026-05-16T09-00-00_alpha")
	gotByPath, err := FindJobByPrefix(tmp, path)
	if err != nil {
		t.Fatalf("path lookup err = %v", err)
	}
	if gotByPath.ID != "2026-05-16T09-00-00_alpha" {
		t.Errorf("path lookup ID = %q", gotByPath.ID)
	}
	// Empty id → explicit error so the CLI can render a usage hint.
	if _, err := FindJobByPrefix(tmp, ""); err == nil {
		t.Errorf("empty prefix err = nil, want non-nil")
	}
}

// TestIsAlive_OwnPidAndDeadPid pins the two interesting cases of IsAlive
// against real syscalls: our own pid (always alive) and a pid we just
// reaped (essentially always dead).
func TestIsAlive_OwnPidAndDeadPid(t *testing.T) {
	t.Parallel()
	if !IsAlive(os.Getpid()) {
		t.Errorf("IsAlive(self) = false, want true")
	}
	if IsAlive(0) {
		t.Errorf("IsAlive(0) = true, want false")
	}
	if IsAlive(-1) {
		t.Errorf("IsAlive(-1) = true, want false")
	}
	// A freshly-reaped child is overwhelmingly likely to be dead.
	deadPid := deadlyPid(t)
	if IsAlive(deadPid) {
		t.Errorf("IsAlive(reaped pid %d) = true, want false (pid recycled?)", deadPid)
	}
}

// TestCancelJob_RefusesNonRunning makes sure CancelJob doesn't blunder
// into signalling a stranger's process when the job has finished or has
// never recorded a pid.
func TestCancelJob_RefusesNonRunning(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	writeJobDir(t, tmp, "2026-05-16T13-00-00_done", -1, &RunRecord{
		Status: "ok",
	}, "")
	j, err := FindJobByPrefix(tmp, "2026-05-16T13-00-00")
	if err != nil {
		t.Fatalf("FindJobByPrefix: %v", err)
	}
	if j.State == JobStateRunning {
		t.Fatalf("precondition: state = %q, want non-running", j.State)
	}
	if err := CancelJob(j); !errors.Is(err, ErrJobNotRunning) {
		t.Errorf("CancelJob(finished) = %v, want ErrJobNotRunning", err)
	}
}

// TestCancelJob_SignalsLiveWorker spawns a child process, points a fake
// job's pid file at it, and verifies CancelJob actually delivers
// SIGTERM. We can't use `kill -0` here — that's exactly what IsAlive
// uses, so we'd be testing tautology. Instead we use a sleep child and
// confirm it exits with the signal-induced exit status within a tight
// deadline.
func TestCancelJob_SignalsLiveWorker(t *testing.T) {
	t.Parallel()
	// `sleep 60` in a shell is the simplest portable always-running
	// child that responds to SIGTERM by exiting with status 143 (or
	// the equivalent on platforms where the shell propagates the
	// signal). We use a long sleep so the child doesn't race us
	// to a natural exit.
	cmd := newSleepCmd(t)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() {
		// In case the test fails before SIGTERM lands, make sure
		// we don't leave a stray process around the test machine.
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	tmp := t.TempDir()
	dir := writeJobDir(t, tmp, "2026-05-16T14-00-00_sleep", cmd.Process.Pid, nil, "")
	j, err := summarizeJob(dir)
	if err != nil {
		t.Fatalf("summarizeJob: %v", err)
	}
	if j.State != JobStateRunning {
		t.Fatalf("precondition: state = %q, want %q", j.State, JobStateRunning)
	}
	if err := CancelJob(j); err != nil {
		t.Fatalf("CancelJob: %v", err)
	}
	// The child should exit within a short window. We give it a
	// generous deadline (5s) so a busy CI doesn't flake.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		// fine; the signal landed
	case <-time.After(5 * time.Second):
		t.Fatalf("worker did not exit within 5s after SIGTERM")
	}
}

// TestTailLogs_NonFollow exercises the plain `cat` mode: the helper
// streams existing content and returns nil.
func TestTailLogs_NonFollow(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	path := filepath.Join(tmp, "vamp.log")
	want := "hello\nworld\n"
	if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := TailLogs(context.Background(), &buf, path, false, 0, nil); err != nil {
		t.Fatalf("TailLogs: %v", err)
	}
	if got := buf.String(); got != want {
		t.Errorf("TailLogs got %q, want %q", got, want)
	}
}

// TestTailLogs_FollowDrainsThenStops drives the follow-mode flow:
// existing content is streamed, then a goroutine appends more, then
// stopWhen flips true and TailLogs returns with all bytes captured.
func TestTailLogs_FollowDrainsThenStops(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	path := filepath.Join(tmp, "vamp.log")
	if err := os.WriteFile(path, []byte("line1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Append more after a short delay, then flip stop.
	var stop sync.WaitGroup
	stop.Add(1)
	stopFlag := make(chan struct{})
	stopWhen := func() bool {
		select {
		case <-stopFlag:
			return true
		default:
			return false
		}
	}
	go func() {
		defer stop.Done()
		time.Sleep(50 * time.Millisecond)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = f.WriteString("line2\n")
		time.Sleep(50 * time.Millisecond)
		_, _ = f.WriteString("line3\n")
		time.Sleep(50 * time.Millisecond)
		close(stopFlag)
	}()
	var sink syncBuf
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := TailLogs(ctx, &sink, path, true, 20*time.Millisecond, stopWhen); err != nil {
		t.Fatalf("TailLogs: %v", err)
	}
	stop.Wait()
	got := sink.String()
	for _, want := range []string{"line1\n", "line2\n", "line3\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q (got %q)", want, got)
		}
	}
}

// TestTailLogs_FollowWaitsForFile makes sure -f works even when the log
// file doesn't exist yet at invocation time (a likely race with the
// child worker just after `--detach` returns).
func TestTailLogs_FollowWaitsForFile(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	path := filepath.Join(tmp, "vamp.log")
	stopFlag := make(chan struct{})
	stopWhen := func() bool {
		select {
		case <-stopFlag:
			return true
		default:
			return false
		}
	}
	go func() {
		time.Sleep(80 * time.Millisecond)
		_ = os.WriteFile(path, []byte("late\n"), 0o644)
		time.Sleep(80 * time.Millisecond)
		close(stopFlag)
	}()
	var sink syncBuf
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := TailLogs(ctx, &sink, path, true, 20*time.Millisecond, stopWhen); err != nil {
		t.Fatalf("TailLogs: %v", err)
	}
	if got := sink.String(); !strings.Contains(got, "late\n") {
		t.Errorf("got %q, want to contain late", got)
	}
}

// TestWriteAndReadPidFile exercises the round-trip + the malformed-content
// branch readPidFile guards against.
func TestWriteAndReadPidFile(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	if err := WritePidFile(tmp, 12345); err != nil {
		t.Fatalf("WritePidFile: %v", err)
	}
	pid, err := readPidFile(filepath.Join(tmp, PidFileName))
	if err != nil {
		t.Fatalf("readPidFile: %v", err)
	}
	if pid != 12345 {
		t.Errorf("pid = %d, want 12345", pid)
	}
	if err := RemovePidFile(tmp); err != nil {
		t.Fatalf("RemovePidFile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, PidFileName)); !os.IsNotExist(err) {
		t.Errorf("pid file still exists after Remove: %v", err)
	}
	// Malformed content path.
	if err := os.WriteFile(filepath.Join(tmp, PidFileName), []byte("not-a-pid"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readPidFile(filepath.Join(tmp, PidFileName)); err == nil {
		t.Errorf("readPidFile(garbage) err = nil, want non-nil")
	}
	// Non-positive pid.
	if err := os.WriteFile(filepath.Join(tmp, PidFileName), []byte("0"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readPidFile(filepath.Join(tmp, PidFileName)); err == nil {
		t.Errorf("readPidFile(0) err = nil, want non-nil")
	}
}

// --- helpers ---------------------------------------------------------

// deadlyPid forks a tiny child (a no-op `true`), reaps it, and returns
// its pid. The pid is overwhelmingly likely to refer to a dead process
// at the moment the caller checks IsAlive(). A pid recycle is possible
// but extremely unlikely for the test's short runtime.
func deadlyPid(t *testing.T) int {
	t.Helper()
	cmd := newTrueCmd(t)
	if err := cmd.Run(); err != nil {
		t.Fatalf("run /bin/true: %v", err)
	}
	return cmd.Process.Pid
}

// syncBuf is a goroutine-safe bytes.Buffer; needed because the test's
// log-appending goroutine and TailLogs's writer goroutine both write
// concurrently to the same sink in the follow tests.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

var _ io.Writer = (*syncBuf)(nil)

// newSleepCmd builds a long-running child whose SIGTERM exit we use to
// validate CancelJob. We prefer /bin/sleep when present; the
// trivial-but-explicit path lets the test stay portable across distros
// that ship sleep at slightly different locations.
func newSleepCmd(t *testing.T) *exec.Cmd {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("cancel test is Unix-only; runtime=%s", runtime.GOOS)
	}
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("sleep not found on PATH: %v", err)
	}
	return exec.Command(sleep, "60")
}

// newTrueCmd returns a child that exits immediately, used by deadlyPid
// to mint a freshly-reaped pid for IsAlive's dead-pid assertion.
func newTrueCmd(t *testing.T) *exec.Cmd {
	t.Helper()
	tru, err := exec.LookPath("true")
	if err != nil {
		t.Skipf("true not found on PATH: %v", err)
	}
	return exec.Command(tru)
}
