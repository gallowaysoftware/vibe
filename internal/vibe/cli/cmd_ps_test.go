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
