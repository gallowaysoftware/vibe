package fleetannounce

// The desired-intent verb's bound, and the seam that hides it.
//
// This was the fourth call site in the repo handing an operator's shell
// string to exec, and the last one still building the command itself. The
// defect it inherited is internal/vibe/shellcmd's package comment in full:
// exec.CommandContext kills the process it STARTED, `sh` forks the real
// work for anything that is not a lone simple command, the fork keeps the
// output pipes, and Cmd.Wait waits for the pipe COPY. The deadline then
// fires exactly on time, the error says `signal: killed`, and the call does
// not return until the operator's systemctl finishes on its own.
//
// It was deferred rather than fixed with the other three because of the
// seam: every test in this package replaces execSh, so moving the
// production path onto shellcmd while leaving the seam alone would have
// produced a bound no test in this package ever executes. The arrangement
// here is the answer — the seam stays replaceable, and these three tests
// run the UNREPLACED default, structurally and then twice behaviourally.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// forkingVerb FORKS on every shell. %s is a path it writes its background
// child's pid to.
//
// A background job and a `wait`, never the one-word `sleep 10` two of this
// repo's earlier attempts at this test used: bash exec-optimises `sh -c
// 'sleep 10'`, so the one process the deadline kills really is the whole
// command and a kill with no reach past the shell looks armed on the
// workstation and fails in CI, where /bin/sh is dash. No shell can
// exec-optimise this one, which is also what every real cell_cmds verb
// looks like.
const forkingVerb = "sleep 10 & echo $! > %s; wait"

// escapedVerb puts the background job in a process group of its own, out
// of the group kill's reach, leaving WaitDelay as the only thing that can
// end the call. setsid(1) rather than `set -m`: dash's setjobctl opens
// /dev/tty and declines SILENTLY with no controlling terminal, which under
// `go test` is always, while bash sets the group anyway — the same
// shell-shaped split that produced the original defect.
const escapedVerb = "setsid sleep 10 & echo $! > %s; wait"

// verbTestBudget is the deadline these tests hand runVerb. It arrives as
// the CALLER's context, so it beats the 60s verbBudget runVerb derives:
// long enough that the command is certainly running when it fires, short
// enough to be free.
const verbTestBudget = 400 * time.Millisecond

func grandchildPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, err := os.ReadFile(path); err == nil {
			if pid, cerr := strconv.Atoi(strings.TrimSpace(string(b))); cerr == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the verb never recorded a background pid at %s: the forking command this test depends on did not run, so nothing here measured a bound", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// pidAlive probes with signal 0. EPERM counts as alive: a process owned by
// someone else is still a process.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// pidGoneWithin polls rather than looking once: a killed process is a
// zombie until its reaper gets to it, and the shell that would have reaped
// this one was killed alongside it.
func pidGoneWithin(pid int, limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return !pidAlive(pid)
}

// verbClient is a Client with no registry to talk to — runVerb never
// announces, so the loop's config is irrelevant to what these tests time.
// The logger is discarded because a failing verb logs its output, and the
// output here is a killed `sleep`.
func verbClient(t *testing.T) *Client {
	t.Helper()
	c, err := New(Config{
		Cell:        "gpu-cell",
		RegistryURL: "http://127.0.0.1:1",
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// runRealVerb drives runVerb through the UNREPLACED seam under a caller
// deadline of verbTestBudget, and reports how long the call took plus
// where the background child's pid landed.
func runRealVerb(t *testing.T, tmpl string) (elapsed time.Duration, pidFile string) {
	t.Helper()
	pidFile = filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithTimeout(context.Background(), verbTestBudget)
	defer cancel()

	c := verbClient(t)
	start := time.Now()
	ok := c.runVerb(ctx, "drain", fmt.Sprintf(tmpl, pidFile))
	elapsed = time.Since(start)
	if ok {
		t.Fatal("a verb killed at its deadline reported success: the cell would stamp `drained` for a reclaim that never finished")
	}
	return elapsed, pidFile
}

// TestVerbSeam_ProductionDefaultIsTheBoundedRunner is the seam's own
// guard, and the reason this fix was not just an edit to execSh.
//
// Every other test in this package assigns over execSh. A default that
// drifted back to a bare exec.CommandContext — or to any builder that is
// not shellcmd's — would leave those tests passing exactly as they do now
// while the cell's verbs quietly went unbounded again. So the identity is
// asserted, and so is the wiring the builder is responsible for.
func TestVerbSeam_ProductionDefaultIsTheBoundedRunner(t *testing.T) {
	t.Parallel()
	got := reflect.ValueOf(execSh).Pointer()
	want := reflect.ValueOf(runShellVerb).Pointer()
	if got != want {
		t.Fatalf("the execSh seam's default is not runShellVerb (%v vs %v): every test in this package replaces this variable, so a default that stops going through shellcmd is invisible to all of them",
			runtime.FuncForPC(got).Name(), runtime.FuncForPC(want).Name())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := verbCmd(ctx, "true")
	if c.SysProcAttr == nil || !c.SysProcAttr.Setpgid {
		t.Error("the verb's command does not get its own process group, so the negative-pid kill has nothing to signal and the work `sh` forked survives its own deadline")
	}
	if c.Cancel == nil {
		t.Error("no Cancel, so the deadline falls back to exec's default: SIGKILL to the shell alone, which is the whole defect")
	}
	if c.WaitDelay != verbKillGrace {
		t.Errorf("WaitDelay is %v, want verbKillGrace (%v): zero is documented to mean Wait blocks INDEFINITELY on the inherited pipes, which is not a tighter bound but the absence of one", c.WaitDelay, verbKillGrace)
	}
}

// TestDesiredIntentVerb_TheBudgetReachesWhatTheShellForked is the group-kill
// half, driven end to end from runVerb.
//
// The call returning on time is only half of it. A verb whose shell died
// while the reclaim it started runs on is a cell that reported failure and
// left a half-run drain on the box — and on the wire that is
// indistinguishable from the bound having worked.
func TestDesiredIntentVerb_TheBudgetReachesWhatTheShellForked(t *testing.T) {
	t.Parallel()
	elapsed, pidFile := runRealVerb(t, forkingVerb)
	if ceiling := verbTestBudget + verbKillGrace + 2*time.Second; elapsed > ceiling {
		t.Fatalf("the verb took %v, past the %v ceiling: nothing ended it but the command finishing on its own, and an announce loop reconciling a desired intent would sit here for as long as the operator's command lives",
			elapsed.Round(time.Millisecond), ceiling)
	}
	pid := grandchildPID(t, pidFile)
	if !pidGoneWithin(pid, 5*time.Second) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("the verb's background child (pid %d) outlived the deadline that killed its shell: a `systemctl stop` cut off at the budget goes on running with nobody waiting on it, and the cell has already told fleetd the drain failed", pid)
	}
}

// TestDesiredIntentVerb_TheWaitIsBoundedEvenWhenTheKillCannotLand is the
// other half, and the reason WaitDelay is not a belt on top of the group
// kill: work that setsid'd out of the group SURVIVES the kill, which this
// asserts rather than tolerates. With the child alive and still holding the
// pipes it inherited, the only thing that can have ended the call is exec
// closing them at the grace — so the floor is the assertion that matters.
func TestDesiredIntentVerb_TheWaitIsBoundedEvenWhenTheKillCannotLand(t *testing.T) {
	t.Parallel()
	// setsid(1) is util-linux: present on every box this fleet runs on,
	// absent on a macOS workstation. internal/mutation names this test for
	// the wait-delay guard and counts a test that does not RUN as a
	// failure, so the Linux CI job goes red if it ever stops happening.
	if _, err := exec.LookPath("setsid"); err != nil {
		if runtime.GOOS == "linux" {
			t.Fatalf("setsid(1) is missing (%v): on Linux that is a broken environment, and skipping would retire the only behavioural proof that this call site's wait delay does anything", err)
		}
		t.Skipf("setsid(1) is unavailable on %s (%v); TestVerbSeam_ProductionDefaultIsTheBoundedRunner still holds the line structurally", runtime.GOOS, err)
	}

	elapsed, pidFile := runRealVerb(t, escapedVerb)
	// Ceiling first: once the command has ended on its own, every
	// diagnostic below reads as something else's fault.
	if ceiling := verbTestBudget + verbKillGrace + 2*time.Second; elapsed > ceiling {
		t.Fatalf("the verb took %v, past the %v ceiling: nothing bounded the WAIT, so it ended when the command finished on its own — the deadline fired into the void and the error still said `signal: killed`",
			elapsed.Round(time.Millisecond), ceiling)
	}
	pid := grandchildPID(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	pgid, gerr := syscall.Getpgid(pid)
	if !pidAlive(pid) || gerr != nil || pgid != pid {
		t.Fatalf("the background child (pid %d, pgid %d, err %v) did not survive in a group of its own, so this test exercised the group kill and NOT the wait delay: setsid did not detach the job here, and the escape needs a different spelling",
			pid, pgid, gerr)
	}
	if floor := verbTestBudget + verbKillGrace; elapsed < floor {
		t.Fatalf("the verb returned in %v, sooner than the %v it must spend when a writer to its pipes outlives the kill — whatever ended it, it was not the wait delay",
			elapsed.Round(time.Millisecond), floor)
	}
}
