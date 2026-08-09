package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	vibev1 "github.com/gallowaysoftware/vibe/proto/vibe/v1"
)

func runPS(t *testing.T, args ...string) string {
	t.Helper()
	cmd := psCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("vibe ps %v: %v", args, err)
	}
	return out.String()
}

// TestPSJSON covers the --json half of the read-only status commands: the
// same facts the table prints, in the one shape a script can read.
func TestPSJSON(t *testing.T) {
	started := timestamppb.Now()
	fake := newControlFake()
	fake.active = &vibev1.Status{
		Running: true, Ready: true, Profile: "pi",
		StartedAt: started, BackendAddr: "127.0.0.1:8080", ProxyAddr: "127.0.0.1:9000", Pid: 4242,
	}
	fake.services = []*vibev1.Status{
		{Running: true, Ready: false, Profile: "searxng", BackendAddr: "127.0.0.1:8888", Pid: 77},
	}
	serveControlOnUnix(t, fake)

	var got psReport
	if err := json.Unmarshal([]byte(runPS(t, "--json")), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.DaemonRunning {
		t.Error("daemon_running is false while the daemon answered")
	}
	if got.Active == nil || got.Active.Profile != "pi" || !got.Active.Ready ||
		got.Active.BackendAddr != "127.0.0.1:8080" || got.Active.ProxyAddr != "127.0.0.1:9000" || got.Active.Pid != 4242 {
		t.Errorf("active = %+v", got.Active)
	}
	if got.Active.StartedAt == nil || !got.Active.StartedAt.Equal(started.AsTime()) {
		t.Errorf("started_at = %v, want %v — the table prints an uptime, and a script needs the instant it is derived from",
			got.Active.StartedAt, started.AsTime())
	}
	if len(got.Services) != 1 || got.Services[0].Profile != "searxng" || got.Services[0].Ready {
		t.Errorf("services = %+v", got.Services)
	}

	// And the human table is untouched by the flag.
	if plain := runPS(t); !strings.Contains(plain, "active:   pi (ready)") || !strings.Contains(plain, "searxng") {
		t.Errorf("plain output = %q", plain)
	}
}

// TestPSJSONSaysWhenNobodyWasAsked: `vibe ps` must not spawn a daemon to
// answer "is anything running?", so a stopped daemon is a normal, exit-0
// answer. In JSON that has to be a FACT rather than an empty document —
// "nothing is running" and "nobody was asked" are different things, and a
// consumer seeing only a null active would read a dead daemon as an idle box.
func TestPSJSONSaysWhenNobodyWasAsked(t *testing.T) {
	// A runtime dir with no socket in it: nothing to ping.
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	var got psReport
	if err := json.Unmarshal([]byte(runPS(t, "--json")), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.DaemonRunning {
		t.Error("daemon_running is true with no daemon to answer")
	}
	if got.Active != nil {
		t.Errorf("active = %+v, want null", got.Active)
	}
	if got.Services == nil || len(got.Services) != 0 {
		t.Errorf("services = %v, want an empty array (a JSON consumer iterates it)", got.Services)
	}
}

// TestPSRefusesToCallASlowDaemonAStoppedOne: a ping that ran out of time is
// not evidence that nothing is running, and `--json` is where that matters —
// the human reads a sentence and re-runs, a script reads `daemon_running`
// and acts on it.
//
// The rig is the real thing end to end: a real ControlService on a real unix
// socket in a scratch XDG_RUNTIME_DIR, holding a real active profile, whose
// Status simply answers slower than psPingBudget. Before the fix this
// printed {"daemon_running": false, "active": null, "services": []} and
// exited 0 — a live daemon with a resident model, reported as a stopped one.
//
// Both surfaces are asserted because the claim was made on both, and the
// refusal has to NAME the ambiguity rather than say "not running" more
// quietly.
func TestPSRefusesToCallASlowDaemonAStoppedOne(t *testing.T) {
	fake := newControlFake()
	fake.active = &vibev1.Status{Running: true, Ready: true, Profile: "pi"}
	fake.statusDelay = psPingBudget * 2
	serveControlOnUnix(t, fake)

	for _, args := range [][]string{{"--json"}, nil} {
		label := "table"
		if len(args) > 0 {
			label = args[0]
		}
		t.Run(label, func(t *testing.T) {
			cmd := psCmd()
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs(args)
			err := cmd.ExecuteContext(t.Context())
			if err == nil {
				t.Fatalf("vibe ps %v exited 0 with %q — a ping timeout was reported as a fact about the fleet", args, out.String())
			}
			if !strings.Contains(err.Error(), "did not answer") {
				t.Errorf("err = %v, want it to name the unanswered ping rather than assert a state", err)
			}
			// The specific lie, in the specific shape a consumer parses.
			if strings.Contains(out.String(), `"daemon_running": false`) {
				t.Errorf("output = %q, want no daemon_running claim at all", out.String())
			}
			if strings.Contains(out.String(), "daemon not running") {
				t.Errorf("output = %q, want no 'daemon not running' claim", out.String())
			}
		})
	}
}

// TestDaemonAbsentSeparatesNoDaemonFromNoAnswer pins the discrimination the
// command above depends on, at the level of the error itself. Measured
// shapes, not invented ones: an absent or refused socket arrives as
// connect.CodeUnavailable, a spent budget as CodeDeadlineExceeded. The
// stopped-daemon half is named too, because a guard that refused everything
// would satisfy the test above and break the ordinary answer.
func TestDaemonAbsentSeparatesNoDaemonFromNoAnswer(t *testing.T) {
	t.Run("no socket is absence", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
		err := pingDaemon(psPingBudget)
		if err == nil {
			t.Fatal("ping succeeded with no daemon")
		}
		if !daemonAbsent(err) {
			t.Errorf("daemonAbsent(%v) = false — `vibe ps` would refuse instead of answering "+
				"the ordinary stopped-daemon question, which must stay exit 0", err)
		}
	})
	t.Run("a spent budget is not absence", func(t *testing.T) {
		fake := newControlFake()
		fake.statusDelay = psPingBudget * 2
		serveControlOnUnix(t, fake)
		err := pingDaemon(psPingBudget)
		if err == nil {
			t.Fatal("ping succeeded inside the budget; the fake is not slow enough to prove anything")
		}
		if daemonAbsent(err) {
			t.Errorf("daemonAbsent(%v) = true — a timeout counted as evidence that no daemon exists", err)
		}
	})
	t.Run("nil is not absence", func(t *testing.T) {
		if daemonAbsent(nil) {
			t.Error("daemonAbsent(nil) = true — a successful ping read as a missing daemon")
		}
	})
}
