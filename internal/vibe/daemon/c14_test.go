package daemon

// C14 coverage, cell side: the CellSuspend contract (including the idle
// proof, where unknown is not zero) and the sleep_schedule wiring's
// refusals — each of which is a permanent configuration answer that must
// be loud at startup rather than at 23:30.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetannounce"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	vibev1 "github.com/gallowaysoftware/vibe/proto/vibe/v1"
)

func TestCellSuspend_UnconfiguredIsFailedPrecondition(t *testing.T) {
	d := drainDaemon(t, Config{CellCmds: CellCmds{Drain: "true"}})
	_, err := d.CellSuspend(context.Background(), connect.NewRequest(&vibev1.CellSuspendRequest{}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("got %v, want FailedPrecondition (cell_cmds.suspend unset)", err)
	}
}

func TestCellSuspend_FailingCommandIsUnavailableWithStderr(t *testing.T) {
	d := drainDaemon(t, Config{CellCmds: CellCmds{Suspend: "false"}})
	d.SetCellCmdRunner(func(context.Context, string) (string, error) {
		return "Failed to suspend: Interactive authentication required", fmt.Errorf("exit 1")
	})
	_, err := d.CellSuspend(context.Background(), connect.NewRequest(&vibev1.CellSuspendRequest{}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("got %v, want Unavailable", err)
	}
	if !strings.Contains(err.Error(), "Interactive authentication required") {
		t.Fatalf("error = %v, want the command's stderr carried out", err)
	}
}

// TestCellSuspend_IdleProofRefusesUnknownAndBusy is the receiving half of
// the guard. The UNREPORTED case is the one that matters: a cell that
// cannot measure itself must not be suspended on the strength of a zero
// it never observed.
func TestCellSuspend_IdleProofRefusesUnknownAndBusy(t *testing.T) {
	t.Run("unreported is not zero", func(t *testing.T) {
		d := drainDaemon(t, Config{CellCmds: CellCmds{Suspend: "true"}})
		ran := false
		d.SetCellCmdRunner(func(context.Context, string) (string, error) { ran = true; return "", nil })
		_, err := d.CellSuspend(context.Background(), connect.NewRequest(&vibev1.CellSuspendRequest{RequireIdle: true}))
		if connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Fatalf("got %v, want FailedPrecondition", err)
		}
		if !strings.Contains(err.Error(), "unknown is not zero") {
			t.Fatalf("error = %v, want it to name the missing evidence", err)
		}
		if ran {
			t.Fatal("the suspend command ran despite the refusal")
		}
	})

	t.Run("in-flight work refuses", func(t *testing.T) {
		d := drainDaemon(t, Config{CellCmds: CellCmds{Suspend: "true"}})
		d.fleet = fleetWithInFlight(t, `{"requests":[{"model":"qwen"}]}`)
		d.SetCellCmdRunner(func(context.Context, string) (string, error) {
			t.Fatal("the suspend command ran while a request was in flight")
			return "", nil
		})
		_, err := d.CellSuspend(context.Background(), connect.NewRequest(&vibev1.CellSuspendRequest{RequireIdle: true}))
		if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "in flight") {
			t.Fatalf("got %v, want a FailedPrecondition naming the in-flight work", err)
		}
	})

	t.Run("a reported zero passes and reports the proof", func(t *testing.T) {
		d := drainDaemon(t, Config{CellCmds: CellCmds{Suspend: "true"}})
		d.fleet = fleetWithInFlight(t, `{"requests":[]}`)
		d.SetCellCmdRunner(func(context.Context, string) (string, error) { return "", nil })
		resp, err := d.CellSuspend(context.Background(), connect.NewRequest(&vibev1.CellSuspendRequest{RequireIdle: true}))
		if err != nil {
			t.Fatal(err)
		}
		if resp.Msg.IdleStatus != fleetapi.SuspendIdleVerified {
			t.Fatalf("idle_status = %q, want %q", resp.Msg.IdleStatus, fleetapi.SuspendIdleVerified)
		}
		if resp.Msg.InFlightRequests == nil || *resp.Msg.InFlightRequests != 0 {
			t.Fatalf("in_flight = %v, want a reported 0", resp.Msg.InFlightRequests)
		}
	})

	t.Run("without the proof it runs (an operator asking is not fleetd guessing)", func(t *testing.T) {
		d := drainDaemon(t, Config{CellCmds: CellCmds{Suspend: "true"}})
		ran := false
		d.SetCellCmdRunner(func(context.Context, string) (string, error) { ran = true; return "", nil })
		resp, err := d.CellSuspend(context.Background(), connect.NewRequest(&vibev1.CellSuspendRequest{}))
		if err != nil || !ran {
			t.Fatalf("err = %v, ran = %v", err, ran)
		}
		if resp.Msg.IdleStatus != fleetapi.SuspendIdleNotRequired {
			t.Fatalf("idle_status = %q, want %q", resp.Msg.IdleStatus, fleetapi.SuspendIdleNotRequired)
		}
	})
}

// fleetWithInFlight builds a fleetapi server whose one cell streams a
// single inflight frame, so the daemon's own in-flight read has real
// data behind it rather than a hand-set map.
func fleetWithInFlight(t *testing.T, frame string) *fleetapi.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/events" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		payload, err := json.Marshal(frame)
		if err != nil {
			t.Error(err)
			return
		}
		fmt.Fprintf(w, "event:message\ndata:{\"type\":\"inflight\",\"data\":%s}\n\n", payload)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	f := fleetapi.New([]fleetapi.Cell{{Name: fleetcfg.FrontCell, URL: srv.URL}},
		t.TempDir()+"/hist.json", func() fleetapi.DaemonInfo { return fleetapi.DaemonInfo{} }, fleetapi.Options{})
	f.Start()
	t.Cleanup(f.Close)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := f.InFlight(fleetcfg.FrontCell); ok {
			return f
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("the inflight frame never arrived")
	return nil
}

// TestCellSuspend_StampsTheCellsOwnIntentOnEveryPath is the ghost-drain
// guard's cell half. Without the stamp the box wakes echoing `serving`
// at an OLDER instant than fleetd's sleep request, C3's conflict rule
// hands that request back, and the cell runs its own drain verb on a box
// that just woke up.
func TestCellSuspend_StampsTheCellsOwnIntentOnEveryPath(t *testing.T) {
	for _, remote := range []bool{false, true} {
		name := "local"
		if remote {
			name = "remote (fleetd-invoked)"
		}
		t.Run(name, func(t *testing.T) {
			fleetd := newFakeFleetd(t)
			d := drainDaemon(t, Config{
				CellCmds: CellCmds{Suspend: "true"},
				Fleet:    FleetConfig{Cell: "gpu-cell", RegistryURL: fleetd.srv.URL},
			})
			d.SetCellCmdRunner(func(context.Context, string) (string, error) { return "", nil })
			intentFile := t.TempDir() + "/local-intent.json"
			ann, err := fleetannounce.New(fleetannounce.Config{
				Cell: "gpu-cell", RegistryURL: fleetd.srv.URL, IntentPath: intentFile,
			})
			if err != nil {
				t.Fatal(err)
			}
			d.announce = ann

			ctx := context.Background()
			if remote {
				ctx = context.WithValue(ctx, remoteInvocationKey{}, true)
			}
			if _, err := d.CellSuspend(ctx, connect.NewRequest(&vibev1.CellSuspendRequest{Reason: "asleep per sleep_schedule"})); err != nil {
				t.Fatal(err)
			}
			if got := ann.LocalIntent().State; got != "drained" {
				t.Fatalf("the cell's own echo = %q, want drained on both invocation paths", got)
			}
			// And the C3 rule still holds on top of it: an ANNOUNCING cell
			// never posts intent, because the echo it just stamped IS the
			// record — a POST would stamp a newer request than the cell's
			// own and leave a ghost "awaiting ack".
			if n := len(fleetd.intentCalls()); n != 0 {
				t.Fatalf("intent posts = %d on an announcing cell (the echo is the record)", n)
			}
		})
	}
}

// TestCellSuspend_NoStampWhenTheVerbFails keeps C2's other half: a
// failed verb records nothing, anywhere.
func TestCellSuspend_NoStampWhenTheVerbFails(t *testing.T) {
	fleetd := newFakeFleetd(t)
	d := drainDaemon(t, Config{
		CellCmds: CellCmds{Suspend: "false"},
		Fleet:    FleetConfig{Cell: "gpu-cell", RegistryURL: fleetd.srv.URL},
	})
	d.SetCellCmdRunner(func(context.Context, string) (string, error) { return "boom", fmt.Errorf("exit 1") })
	ann, err := fleetannounce.New(fleetannounce.Config{
		Cell: "gpu-cell", RegistryURL: fleetd.srv.URL, IntentPath: t.TempDir() + "/local-intent.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	d.announce = ann

	if _, err := d.CellSuspend(context.Background(), connect.NewRequest(&vibev1.CellSuspendRequest{})); err == nil {
		t.Fatal("a failing suspend command returned success")
	}
	if got := ann.LocalIntent().State; got == "drained" {
		t.Fatal("a failed suspend stamped the cell's intent")
	}
	if got := fleetd.intentCalls(); len(got) != 0 {
		t.Fatalf("a failed suspend recorded intent at fleetd: %v", got)
	}
}

// ─── the wiring's refusals ──────────────────────────────────────────────────

func sleepHosts() *fleetcfg.File {
	return &fleetcfg.File{Cells: map[string]fleetcfg.Cell{
		fleetcfg.FrontCell: {URL: "http://front:9000", Class: fleetcfg.ClassAlwaysOn},
		"gpu-cell": {URL: "http://gpu:9000", Class: fleetcfg.ClassOpportunistic,
			DaemonURL: "http://gpu:9001", Wake: &fleetcfg.Wake{MAC: "aa:bb:cc:dd:ee:ff"}},
		"heavy": {URL: "http://heavy:9000", Class: fleetcfg.ClassAlwaysOn,
			DaemonURL: "http://heavy:9001", Wake: &fleetcfg.Wake{MAC: "aa:bb:cc:dd:ee:ff"}},
		"laptop": {URL: "http://laptop:9000", Class: fleetcfg.ClassRoaming,
			DaemonURL: "http://laptop:9001", Wake: &fleetcfg.Wake{MAC: "aa:bb:cc:dd:ee:ff"}},
		"no-daemon": {URL: "http://nd:9000", Class: fleetcfg.ClassOpportunistic,
			Wake: &fleetcfg.Wake{MAC: "aa:bb:cc:dd:ee:ff"}},
		"no-wake": {URL: "http://nw:9000", Class: fleetcfg.ClassOpportunistic, DaemonURL: "http://nw:9001"},
	}}
}

func TestSleepEntries_RefusesEverythingThatCannotSafelySleep(t *testing.T) {
	cases := []struct {
		name  string
		entry SleepScheduleEntry
	}{
		{"no wake half", SleepScheduleEntry{Cell: "gpu-cell", Suspend: "30 23 * * *"}},
		{"no suspend cron", SleepScheduleEntry{Cell: "gpu-cell", Wake: "15 7 * * *"}},
		{"the front", SleepScheduleEntry{Cell: fleetcfg.FrontCell, Suspend: "30 23 * * *", Wake: "15 7 * * *"}},
		{"unknown cell", SleepScheduleEntry{Cell: "typo", Suspend: "30 23 * * *", Wake: "15 7 * * *"}},
		{"always_on", SleepScheduleEntry{Cell: "heavy", Suspend: "30 23 * * *", Wake: "15 7 * * *"}},
		{"roaming", SleepScheduleEntry{Cell: "laptop", Suspend: "30 23 * * *", Wake: "15 7 * * *"}},
		{"no daemon_url", SleepScheduleEntry{Cell: "no-daemon", Suspend: "30 23 * * *", Wake: "15 7 * * *"}},
		{"no wake path in hosts.yaml", SleepScheduleEntry{Cell: "no-wake", Suspend: "30 23 * * *", Wake: "15 7 * * *"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sleepEntries(Config{SleepSchedule: []SleepScheduleEntry{tc.entry}}, sleepHosts())
			if len(got) != 0 {
				t.Fatalf("entry accepted: %+v", got)
			}
		})
	}
}

func TestSleepEntries_AcceptsAndClamps(t *testing.T) {
	got := sleepEntries(Config{SleepSchedule: []SleepScheduleEntry{{
		Cell: "gpu-cell", Suspend: "30 23 * * *", Wake: "15 7 * * *",
		QuietFor: "30s", MaxDefer: "48h", WakeGrace: "", Warm: []string{"qwen"},
	}}}, sleepHosts())
	if len(got) != 1 {
		t.Fatalf("entries = %+v, want the opportunistic cell accepted", got)
	}
	e := got[0]
	if e.QuietFor != fleetapi.MinSleepQuietFor {
		t.Errorf("quiet_for = %v, want the floor %v (clamped, never skipped)", e.QuietFor, fleetapi.MinSleepQuietFor)
	}
	if e.MaxDefer != fleetapi.MaxSleepMaxDefer {
		t.Errorf("max_defer = %v, want the ceiling %v", e.MaxDefer, fleetapi.MaxSleepMaxDefer)
	}
	if e.WakeGrace != fleetapi.DefaultSleepWakeGrace {
		t.Errorf("wake_grace = %v, want the default %v", e.WakeGrace, fleetapi.DefaultSleepWakeGrace)
	}
	if len(e.Warm) != 1 || e.Warm[0] != "qwen" {
		t.Errorf("warm = %v", e.Warm)
	}
}

// TestSleepEntries_OneNightPerCell (review finding): two entries for one
// box are two loops arming two suspends against one machine, and the
// second RPC lands on something already freezing — a config typo that
// reads as a flaky suspend.
func TestSleepEntries_OneNightPerCell(t *testing.T) {
	got := sleepEntries(Config{SleepSchedule: []SleepScheduleEntry{
		{Cell: "gpu-cell", Suspend: "30 23 * * *", Wake: "15 7 * * *"},
		{Cell: "gpu-cell", Suspend: "0 1 * * *", Wake: "0 8 * * *"},
	}}, sleepHosts())
	if len(got) != 1 || got[0].Suspend != "30 23 * * *" {
		t.Fatalf("entries = %+v, want only the first", got)
	}
}

// TestSuspendCell_NoAnnounceFallback pins the delivery decision: the
// suspend verb is an RPC, and a cell without daemon_url is an error
// rather than a piggybacked command that would be redelivered after the
// box next reboots.
func TestSuspendCell_NoAnnounceFallback(t *testing.T) {
	d := drainDaemon(t, Config{})
	err := d.suspendCell(sleepHosts())(context.Background(), "no-daemon", "asleep per sleep_schedule")
	if err == nil || !strings.Contains(err.Error(), "no announce fallback") {
		t.Fatalf("err = %v, want a refusal naming the missing fallback", err)
	}
}
