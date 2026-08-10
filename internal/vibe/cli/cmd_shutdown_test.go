package cli

import (
	"bytes"
	"strings"
	"testing"

	vibev1 "github.com/gallowaysoftware/vibe/proto/vibe/v1"
)

// runShutdown executes `vibe shutdown` through the root, which is where
// SilenceUsage and SilenceErrors live — a bare shutdownCmd() would print
// cobra's usage block onto the same stream the command makes its claim on
// and muddy every assertion below.
func runShutdown(t *testing.T) (stdout string, err error) {
	t.Helper()
	cmd := rootCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"shutdown"})
	err = cmd.ExecuteContext(t.Context())
	return out.String(), err
}

// TestShutdownStopsADaemonThatAnswers is the ordinary path, named first
// because the guard below must not buy its refusal by breaking it.
func TestShutdownStopsADaemonThatAnswers(t *testing.T) {
	fake := newControlFake()
	fake.active = &vibev1.Status{Running: true, Ready: true, Profile: "pi"}
	serveControlOnUnix(t, fake)

	out, err := runShutdown(t)
	if err != nil {
		t.Fatalf("vibe shutdown: %v", err)
	}
	if !strings.Contains(out, "daemon shutting down") {
		t.Errorf("stdout = %q, want the shutdown to be reported", out)
	}
	if n := fake.shutdownCount(); n != 1 {
		t.Errorf("Shutdown RPC called %d times, want 1 — the command reported a teardown it never asked for", n)
	}
}

// TestShutdownReportsAnAbsentDaemonAtExitZero pins the idempotence that
// the guard below must leave intact: already-down IS the desired end state
// of a teardown command, so `vibe shutdown` needs no `|| true` in a
// script. A guard that refused every ping failure would satisfy the
// timeout test and break this one.
func TestShutdownReportsAnAbsentDaemonAtExitZero(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	out, err := runShutdown(t)
	if err != nil {
		t.Fatalf("vibe shutdown with no daemon = %v, want exit 0", err)
	}
	if !strings.Contains(out, "daemon not running") {
		t.Errorf("stdout = %q, want the already-down report", out)
	}
}

// TestShutdownRefusesToCallASlowDaemonAStoppedOne is the defect.
//
// Every ping failure printed "daemon not running" and returned nil, on a
// 200ms budget — the shortest in the CLI, on the one command whose entire
// job is to talk to a daemon that is by construction busy: it is holding a
// model and it is about to be asked to tear the whole stack down. So
// `vibe shutdown && <next step>` said the daemon was down, exited 0, and
// ran the next step against a daemon that was still up with the GPU still
// occupied — the next step being, in this repo's own scripts, another
// profile's start.
//
// The rig drives a TIMEOUT, not a refusal: a real ControlService on a real
// unix socket, up and holding an active profile, answering Status slower
// than pingBudget. A refused socket proves nothing here — it is the case
// that was always handled, and it is asserted separately above.
func TestShutdownRefusesToCallASlowDaemonAStoppedOne(t *testing.T) {
	fake := newControlFake()
	fake.active = &vibev1.Status{Running: true, Ready: true, Profile: "pi"}
	fake.statusDelay = pingBudget * 2
	serveControlOnUnix(t, fake)

	out, err := runShutdown(t)
	if err == nil {
		t.Fatalf("vibe shutdown exited 0 with %q — a ping timeout was reported as a fact about the daemon, "+
			"and `vibe shutdown && next` would run `next` against a live daemon", out)
	}
	if strings.Contains(out, "daemon not running") {
		t.Errorf("stdout = %q, want no claim that the daemon is stopped", out)
	}
	if !strings.Contains(err.Error(), "did not answer") {
		t.Errorf("err = %v, want it to name the unanswered ping rather than assert a state", err)
	}
	if n := fake.shutdownCount(); n != 0 {
		t.Errorf("Shutdown RPC called %d times, want 0 — nothing was torn down, and the message must say so", n)
	}
}
