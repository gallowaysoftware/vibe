package shellcmd

// The two mechanisms, each proved to do something on its own.
//
// A test here has to produce the thing the defect needs — a process that
// outlives the kill aimed at the shell — which is not something every
// platform's /bin/sh can be talked into. So the coverage is layered: a
// structural read that runs everywhere, and behavioural pairs that run
// where a process can actually be driven out of its group. Each names
// what its own failure means, because the two mechanisms fail in ways
// that look identical from the wire.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// budget is every test's deadline here: long enough that the command is
// certainly running when it fires, short enough to be free.
const budget = 400 * time.Millisecond

// grace is the wait delay under test. Deliberately not DefaultKillGrace —
// a test that passes only for the default proves nothing about the
// parameter.
const grace = 700 * time.Millisecond

// forkingVerb is a command that FORKS on every shell. %s is a path it
// writes its background child's pid to.
//
// A background job and a `wait`, not the one-word `sleep 10` both of this
// repo's first attempts used, because that version proves nothing on the
// machine most of this code gets written on: bash turns `sh -c 'sleep
// 10'` into an EXEC, so the single process the deadline kills really is
// the whole command and a kill with no reach past the shell looks armed.
// dash does not do that, and NO shell can for a command containing a `;`,
// a `&&`, a subshell or a background job — which is what a real
// wake.cmd or cell_cmds.drain looks like. Both units that met this
// defect shipped green on a workstation and red on CI for that reason.
const forkingVerb = "sleep 10 & echo $! > %s; wait"

// escapedVerb is forkingVerb whose background job leaves the process
// group entirely, putting it out of the group kill's reach and leaving
// WaitDelay as the only thing that can end the call. It is the
// daemonising-helper case — a command that starts something meant to
// outlive it — in one word.
//
// setsid(1) rather than `set -m`. Job control looks like the portable
// answer (POSIX does require a background job under `set -m` to get its
// own process group) and is not one: dash's setjobctl opens /dev/tty and
// declines SILENTLY when there is no controlling terminal, which under
// `go test` is always. bash sets the group anyway — the same
// shell-shaped split that produced the defect, one layer up, and it cost
// a CI round to find.
const escapedVerb = "setsid sleep 10 & echo $! > %s; wait"

// grandchildPID reads the pid the command recorded, polling because "the
// first thing it does" still happens after a fork and an exec.
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
			t.Fatalf("the command never recorded a background pid at %s: the forking command this test depends on did not run, so nothing here measured anything", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// pidAlive probes for existence with signal 0. EPERM counts as alive: a
// process owned by someone else is still a process.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// pidGoneWithin polls rather than looking once: a killed process is a
// zombie until its reaper gets to it, and the shell that would have
// reaped this one was killed alongside it. Measured at ~5ms via init on
// Linux, but it is not zero.
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

// run executes script under a `budget` deadline and reports how long the
// call took and where the background child's pid was written.
//
// CombinedOutput, not a bare Run: with Stdout and Stderr left nil, exec
// wires the command straight to /dev/null and there is no pipe COPY for
// Wait to block on — so the whole defect disappears and every test here
// passes for the wrong reason. (The first draft did exactly that and was
// caught by the floor assertion in the escape test, which is what that
// assertion is for.) Both real callers attach pipe-backed output: the
// wake path via CombinedOutput, the drain verb via a bytes.Buffer on
// Stderr.
func run(t *testing.T, tmpl string, killGrace time.Duration) (elapsed time.Duration, pidFile string) {
	t.Helper()
	pidFile = filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	c := New(ctx, fmt.Sprintf(tmpl, pidFile), killGrace)
	start := time.Now()
	_, err := c.CombinedOutput()
	elapsed = time.Since(start)
	if err == nil {
		t.Fatal("a command killed at its deadline reported success")
	}
	return elapsed, pidFile
}

// TestNew_WiresBothHalvesOfTheDeath reads the three fields back off the
// built command. Structural, and deliberately so: the behavioural tests
// below need a process tree that survives a SIGKILL, which no shell can
// be relied on to produce everywhere. This one runs on every platform and
// is what keeps a deleted line visible where they cannot run.
func TestNew_WiresBothHalvesOfTheDeath(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := New(ctx, "true", grace)

	if c.SysProcAttr == nil || !c.SysProcAttr.Setpgid {
		t.Error("the command does not get its own process group, so the negative-pid kill below has nothing to signal and the work the shell forked survives its own deadline")
	}
	if c.Cancel == nil {
		t.Error("no Cancel, so the deadline falls back to exec's default: SIGKILL to the shell alone, which is the defect this package exists to close")
	}
	if c.WaitDelay != grace {
		t.Errorf("WaitDelay is %v, want the caller's %v: Cmd.Wait blocks until every writer to the inherited pipes closes, so work that escaped the kill holds the call with no bound at all", c.WaitDelay, grace)
	}
}

// TestNew_ANonPositiveGraceIsNotABound. Zero is the one WaitDelay value
// that means "wait forever", so a caller who reached it through an unset
// field would silently reinstate the defect. The fallback is loud and
// belongs here rather than at each call site — having one builder is the
// point.
func TestNew_ANonPositiveGraceIsNotABound(t *testing.T) {
	t.Parallel()
	for _, d := range []time.Duration{0, -time.Second} {
		ctx, cancel := context.WithCancel(context.Background())
		c := New(ctx, "true", d)
		if c.WaitDelay != DefaultKillGrace {
			t.Errorf("a %v grace produced WaitDelay %v, want the %v default: zero is documented to mean Wait blocks INDEFINITELY on the I/O pipes, which is not a tighter bound but the absence of one",
				d, c.WaitDelay, DefaultKillGrace)
		}
		cancel()
	}
}

// TestNew_TheKillReachesWhatTheShellForked is the group-kill half. The
// call returning is only part of it: a shell that dies while the work it
// started runs on is a command that reported failure and left the far
// side half-done, and from the caller's side that is indistinguishable
// from the kill having worked.
func TestNew_TheKillReachesWhatTheShellForked(t *testing.T) {
	t.Parallel()
	elapsed, pidFile := run(t, forkingVerb, grace)
	if ceiling := budget + grace + 2*time.Second; elapsed > ceiling {
		t.Fatalf("the call took %v, past the %v ceiling: nothing ended it but the command finishing on its own", elapsed.Round(time.Millisecond), ceiling)
	}
	pid := grandchildPID(t, pidFile)
	if !pidGoneWithin(pid, 5*time.Second) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("the background child (pid %d) outlived the command that started it: the kill landed on the shell in front of the work and nothing else, so an `ssh … poweron` or a `systemctl stop` cut off at its deadline goes on running with nobody waiting on it", pid)
	}
}

// TestNew_TheWaitIsBoundedEvenWhenTheKillCannotLand is the other half,
// and the reason WaitDelay is not a belt on top of the group kill.
//
// A command that puts its work in a process group of its own is out of
// reach of a kill aimed at the shell's group. The work therefore
// SURVIVES, which this test asserts rather than tolerates: with the child
// alive and still holding the pipes it inherited, the only thing that can
// have ended the call is exec closing them at the grace.
func TestNew_TheWaitIsBoundedEvenWhenTheKillCannotLand(t *testing.T) {
	t.Parallel()
	// setsid(1) is util-linux, so it is on every box this fleet runs on
	// and absent on a macOS workstation. Skipping there is safe in a way
	// a skip usually is not here: internal/mutation's registry names this
	// test for the WaitDelay guard and counts a test that does not RUN as
	// a failure, so the Linux CI job goes red if it ever stops happening.
	// On Linux it is not skippable at all — a missing setsid there means
	// the box is wrong, not the test.
	if _, err := exec.LookPath("setsid"); err != nil {
		if runtime.GOOS == "linux" {
			t.Fatalf("setsid(1) is missing (%v): on Linux that is a broken environment, and skipping would retire the only behavioural proof that the wait delay does anything", err)
		}
		t.Skipf("setsid(1) is unavailable on %s (%v); TestNew_WiresBothHalvesOfTheDeath still holds the line structurally, and the Linux CI job runs this one", runtime.GOOS, err)
	}

	elapsed, pidFile := run(t, escapedVerb, grace)
	// Ceiling first: once the command has ended on its own, every
	// diagnostic below reads as something else's fault.
	if ceiling := budget + grace + 2*time.Second; elapsed > ceiling {
		t.Fatalf("the call took %v, past the %v ceiling: nothing bounded the WAIT, so it ended when the command finished on its own — the deadline fired into the void and the error still said `signal: killed`",
			elapsed.Round(time.Millisecond), ceiling)
	}
	pid := grandchildPID(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	pgid, gerr := syscall.Getpgid(pid)
	if !pidAlive(pid) || gerr != nil || pgid != pid {
		t.Fatalf("the background child (pid %d, pgid %d, err %v) did not survive in a group of its own, so this test exercised the group kill and NOT the wait delay: setsid did not detach the job here, and the escape needs a different spelling",
			pid, pgid, gerr)
	}
	if floor := budget + grace; elapsed < floor {
		t.Fatalf("the call returned in %v, sooner than the %v it must spend when a writer to its pipes outlives the kill — whatever ended it, it was not the wait delay",
			elapsed.Round(time.Millisecond), floor)
	}
}
