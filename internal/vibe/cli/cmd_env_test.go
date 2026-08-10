package cli

import (
	"bytes"
	"strings"
	"testing"

	vibev1 "github.com/gallowaysoftware/vibe/proto/vibe/v1"
)

// runEnv executes `vibe env` with stdout and stderr in SEPARATE buffers.
// Separate on purpose: this command's stdout is fed to `eval`, so "did it
// print anything" and "did it say anything" are different questions and a
// shared buffer cannot tell them apart.
//
// Through rootCmd rather than envCmd, because the root is where
// SilenceUsage and SilenceErrors are set and this test's whole subject is
// which stream things land on. A bare envCmd() prints cobra's usage block
// to stdout on any error — which the shell would then EXECUTE — and no
// production invocation does that.
func runEnv(t *testing.T) (stdout, stderr string, err error) {
	t.Helper()
	cmd := rootCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"env"})
	err = cmd.ExecuteContext(t.Context())
	return out.String(), errOut.String(), err
}

// TestEnvExportsTheActiveProfilesFrontendEnv is the ordinary answer, named
// first because the guard below must not buy its refusal by breaking it.
func TestEnvExportsTheActiveProfilesFrontendEnv(t *testing.T) {
	fake := newControlFake()
	fake.active = &vibev1.Status{
		Running: true, Ready: true, Profile: "pi",
		FrontendEnv: map[string]string{
			"OPENAI_BASE_URL": "http://127.0.0.1:9000/v1",
			"OPENAI_API_KEY":  "local",
		},
	}
	serveControlOnUnix(t, fake)

	out, _, err := runEnv(t)
	if err != nil {
		t.Fatalf("vibe env: %v", err)
	}
	// Sorted, and quoted so a value with a space survives `eval`.
	want := "export OPENAI_API_KEY=\"local\"\nexport OPENAI_BASE_URL=\"http://127.0.0.1:9000/v1\"\n"
	if out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
}

// TestEnvPrintsNothingWithNoDaemon pins the DOCUMENTED silence: `vibe env`
// is read-only, must not spawn a daemon to answer, and with no daemon
// there is no active profile — so nothing to print, at exit 0. This is the
// half the refusal below must leave alone, and a guard that refused
// everything would satisfy that test and break this one.
func TestEnvPrintsNothingWithNoDaemon(t *testing.T) {
	// A runtime dir with no socket in it: nothing to ping.
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	out, _, err := runEnv(t)
	if err != nil {
		t.Fatalf("vibe env with no daemon = %v, want exit 0 and silence", err)
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty", out)
	}
}

// TestEnvRefusesToCallASlowDaemonAnAbsentOne is the defect.
//
// `eval "$(vibe env)"` is the documented way to point a frontend at the
// local front. Every ping failure used to `return nil`, so a daemon that
// was UP with a model resident and merely slow to answer produced exactly
// what "no profile is active" produces: nothing on stdout, exit 0, no
// message anywhere. The frontend then falls back to its own built-in
// endpoint — a paid vendor API — and the operator is billed for tokens the
// local front was sitting there ready to serve. The failure is silent by
// construction, because silence is also what success looks like.
//
// The rig drives a TIMEOUT rather than a refusal: a real ControlService on
// a real unix socket in a scratch XDG_RUNTIME_DIR, holding a real active
// profile with real env vars, whose Status answers slower than pingBudget.
// A refused socket would prove nothing here — it is the case that was
// always handled.
//
// Three assertions, because the contract has three parts: stdout stays
// EMPTY (it is executed by the user's shell, so a diagnostic there would
// be a command), the failure reaches the exit status, and the message
// names the unanswered ping instead of asserting a state.
func TestEnvRefusesToCallASlowDaemonAnAbsentOne(t *testing.T) {
	fake := newControlFake()
	fake.active = &vibev1.Status{
		Running: true, Ready: true, Profile: "pi",
		FrontendEnv: map[string]string{"OPENAI_BASE_URL": "http://127.0.0.1:9000/v1"},
	}
	fake.statusDelay = pingBudget * 2
	serveControlOnUnix(t, fake)

	out, errOut, err := runEnv(t)
	if err == nil {
		t.Fatalf("vibe env exited 0 with stdout %q — a ping timeout was reported as 'no profile is active', "+
			"which is what makes the frontend fall back to its billable default endpoint", out)
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty: this stream is eval'd, so anything here is EXECUTED", out)
	}
	if !strings.Contains(err.Error(), "did not answer") {
		t.Errorf("err = %v, want it to name the unanswered ping rather than assert a state", err)
	}
	// Nothing on stderr either, at THIS layer: the root sets
	// SilenceErrors, and cmd/vibe/main.go is what renders the returned
	// error to os.Stderr as `vibe: …`. Asserted rather than assumed,
	// because "the diagnostic goes to stderr" is only true if exactly one
	// place prints it.
	if errOut != "" {
		t.Errorf("stderr = %q, want empty at this layer — main() prints the returned error", errOut)
	}
}
