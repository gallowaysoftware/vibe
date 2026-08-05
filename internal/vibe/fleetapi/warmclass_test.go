package fleetapi

// The model_classes guard on the AUTOMATED warm paths (live gate,
// 2026-08-05). hosts.yaml pins non-chat ids so the control plane never
// pokes them with a chat completion. fleetmcp's warm_model has honoured
// that since C1; the three producers in this package did not, and the lab
// watched a warm_schedule fire five chat completions at an embed-class id
// (HTTP 500 each, JIT-loading the model for nothing) seconds after the
// same fleetd refused warm_model for that exact id.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
)

// classedHosts is the lab's hosts.yaml shape: one embed-class id, one
// chat-class id (which documents ownership and must stay warmable).
func classedHosts() *fleetcfg.File {
	return &fleetcfg.File{
		Cells: map[string]fleetcfg.Cell{
			"front": {URL: "http://front.test"},
			"heavy": {URL: "http://127.0.0.1:1"},
		},
		ModelClasses: map[string]string{
			"lab-embed-b":  "embed",
			"lab-chat":     fleetcfg.ModelClassChat,
			"lab-vision-b": fleetcfg.ModelClassVision,
		},
	}
}

func newClassedWarmServer(t *testing.T) *Server {
	t.Helper()
	s := New([]Cell{{Name: "heavy", URL: "http://127.0.0.1:1", Class: "always_on"}},
		t.TempDir()+"/hist.json", testDaemonInfo, Options{Hosts: classedHosts()})
	s.baseBackoff = 10 * time.Millisecond
	s.maxBackoff = 50 * time.Millisecond
	t.Cleanup(s.Close)
	return s
}

// A warm TARGET naming an embed-class id is refused at WIRING: the tick
// here is an hour, so nothing in the loop can have run, and the refusal
// is already the state fleet_status renders. A target that is silently
// absent instead is the failure mode C4 wrote its "clamp, never skip"
// rule against.
func TestWarmTarget_EmbedClassIsSkippedAtWiringBeforeAnyTick(t *testing.T) {
	probe := &warmProbe{}
	s := newClassedWarmServer(t)
	s.startWarmLoopWithConfig(warmLoopConfig{
		targets:  []WarmTarget{{Cell: "heavy", Model: "lab-embed-b", RestoreAfterIdle: time.Millisecond}},
		frontURL: "http://front.test",
		warmFn:   probe.warm,
		tick:     time.Hour,
	})

	st := warmTargetStateOf(s, 0)
	if st.State != "skipped" {
		t.Errorf("state = %q before the first tick, want skipped", st.State)
	}
	if st.Detail == "" || st.Model != "lab-embed-b" {
		t.Errorf("the refusal is not visible in fleet_status: %+v", st)
	}
	if st.LastRestore != nil {
		t.Errorf("last_restore set on a warm that never happened: %v", st.LastRestore)
	}
	if got := probe.got(); len(got) != 0 {
		t.Fatalf("fired %v at an embed-class model", got)
	}
}

// And the same refusal holds at the FIRE point, which is what makes it a
// rule rather than a property of one wiring path. restore() is the warm
// target's single fire point; this is the live shape (the cell announced
// nothing resident, so the empty-restore branch decided to warm).
func TestWarmTarget_RestoreRefusesAnEmbedClassModelAtFireTime(t *testing.T) {
	probe := &warmProbe{}
	s := newClassedWarmServer(t)
	target := WarmTarget{Cell: "heavy", Model: "lab-embed-b", RestoreAfterIdle: time.Millisecond}
	st := &warmTargetState{Cell: "heavy", Model: "lab-embed-b", State: "waiting"}
	s.restore(target, st, warmLoopConfig{frontURL: "http://front.test", warmFn: probe.warm}, "nothing resident")

	if got := probe.got(); len(got) != 0 {
		t.Fatalf("fired %v at an embed-class model; hosts.yaml model_classes exists to stop exactly this", got)
	}
	s.mu.Lock()
	state, detail, last := st.State, st.Detail, st.LastRestore
	s.mu.Unlock()
	if state != "skipped" || detail == "" {
		t.Errorf("state = %q detail = %q, want a named skip", state, detail)
	}
	if last != nil {
		t.Errorf("last_restore set on a warm that never happened: %v", last)
	}
}

// A chat-CLASS entry documents ownership and must stay warmable: the
// guard is about the verb being wrong for the model, not about the id
// appearing in model_classes at all.
func TestWarmTarget_ChatClassModelStillWarms(t *testing.T) {
	probe := &warmProbe{}
	s := newClassedWarmServer(t)
	s.startWarmLoopWithConfig(warmLoopConfig{
		targets:    []WarmTarget{{Cell: "heavy", Model: "lab-chat", RestoreAfterIdle: time.Millisecond}},
		frontURL:   "http://front.test",
		warmFn:     probe.warm,
		tick:       10 * time.Millisecond,
		emptyGrace: 10 * time.Millisecond,
	})
	presenceOf(s, "heavy")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(probe.got()) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("a chat-class warm target never warmed; the guard caught the wrong thing")
}

// TestWarmTarget_VisionClassModelStillWarms is the false-positive guard
// that matters most, because a vision entry is the one plausible
// non-`chat` value an operator writes for a model the warm verb WORKS
// on: a multimodal model is llama-server plus `--mmproj`, answering
// `/v1/chat/completions` with the image as a content part. Refusing it
// would cost a declared target its whole life — a permanent `skipped`
// row and a permanent `warm.policy` WARN — for a warm that would have
// succeeded, which is the class of regression a guard added at four
// call sites at once is most likely to introduce.
func TestWarmTarget_VisionClassModelStillWarms(t *testing.T) {
	probe := &warmProbe{}
	s := newClassedWarmServer(t)
	if why := s.warmClassRefusal("lab-vision-b"); why != "" {
		t.Fatalf("vision refused at the source: %q", why)
	}
	s.startWarmLoopWithConfig(warmLoopConfig{
		targets:    []WarmTarget{{Cell: "heavy", Model: "lab-vision-b", RestoreAfterIdle: time.Millisecond}},
		frontURL:   "http://front.test",
		warmFn:     probe.warm,
		tick:       10 * time.Millisecond,
		emptyGrace: 10 * time.Millisecond,
	})
	if st := warmTargetStateOf(s, 0); st.State == "skipped" {
		t.Fatalf("a vision target was refused at wiring: %+v", st)
	}
	presenceOf(s, "heavy")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(probe.got()) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("a vision-class warm target never warmed")
}

// The schedule half. It is refused at wiring, so it carries no next_fire
// for a warm that will never happen, and the goroutine never exists.
func TestWarmSchedule_EmbedClassEntryIsRefusedAtWiringAndNeverFires(t *testing.T) {
	probe := &warmProbe{}
	s := newClassedWarmServer(t)
	s.startScheduleLoopWithConfig([]WarmScheduleEntry{
		{Cron: "* * * * *", Model: "lab-embed-b"},
	}, func(string) (string, error) { return "heavy", nil }, time.UTC, probe.warm, "http://front.test")

	st := schedStateOf(s, 0)
	if st.NextFire != nil {
		t.Errorf("next_fire = %v on a schedule that is refused", st.NextFire)
	}
	if st.LastNote == "" {
		t.Errorf("a refused schedule says nothing in fleet_status: %+v", st)
	}
	if len(probe.got()) != 0 {
		t.Fatalf("fired %v", probe.got())
	}
}

// Fire time is the second half of the same rule: a producer that reaches
// evalScheduleEntry another way is refused ahead of every guard rung and
// of whatever the cell resolve returned, and without touching last_fire.
func TestWarmSchedule_EmbedClassIsRefusedAtFireTimeToo(t *testing.T) {
	probe := &warmProbe{}
	s := newClassedWarmServer(t)
	spec, err := parseCron("* * * * *")
	if err != nil {
		t.Fatal(err)
	}
	// Every OTHER guard rung open: the cell reports zero in-flight and
	// holds no lease, so this entry would fire on this tick. Without the
	// rung under test that is exactly what it does.
	s.trackInFlight("heavy", inflightFrame(t))
	past := time.Now().Add(-time.Minute)
	st := &warmScheduleState{Cron: "* * * * *", Model: "lab-embed-b", NextFire: &past}
	s.evalScheduleEntry(WarmScheduleEntry{Cron: "* * * * *", Model: "lab-embed-b"}, spec, st,
		func(string) (string, error) { return "heavy", nil }, time.UTC, probe.warm, "http://front.test", time.Now())

	if got := probe.got(); len(got) != 0 {
		t.Fatalf("fired %v at an embed-class model", got)
	}
	if st.LastFire != nil {
		t.Errorf("last_fire stamped on a warm that never happened: %v", st.LastFire)
	}
	if st.LastNote == "" {
		t.Errorf("the refusal is not in the status: %+v", st)
	}
}

// C14's post-wake warms are the third producer, and the one that runs at
// 07:15 with nobody watching.
func TestSleepSchedule_WakeWarmRefusesANonChatClassModel(t *testing.T) {
	probe := &warmProbe{}
	s := newClassedWarmServer(t)
	note := s.wakeWarm(context.Background(), "heavy", "lab-embed-b",
		sleepLoopConfig{warmFn: probe.warm, frontURL: "http://front.test"})
	if got := probe.got(); len(got) != 0 {
		t.Fatalf("the wake warm fired %v at an embed-class model", got)
	}
	if note == "" {
		t.Error("a refused wake warm reported nothing")
	}
}

// The piggyback queue is the channel all three producers share, and a
// 500 from the front is a DELIVERY failure — which is how the live gate's
// refused warm reached the cell anyway, one heartbeat later.
func TestQueueWarm_RefusesANonChatClassModelInsteadOfQueueingIt(t *testing.T) {
	s := newClassedWarmServer(t)
	presenceOf(s, "heavy", AnnounceModel{ID: "lab-embed-b", State: "ready"})

	// A 500 is what the lab's front actually answered.
	cause := &warmHTTPError{Status: 500, Body: "the current context does not logits computation. skipping"}
	if _, err := s.queueWarm("heavy", "lab-embed-b", cause); err == nil {
		t.Fatal("queued a warm for a model the warm verb must never be fired at")
	}
	s.mu.Lock()
	queued := len(s.commands["heavy"])
	s.mu.Unlock()
	if queued != 0 {
		t.Fatalf("%d command(s) queued for the cell to execute", queued)
	}
}

// A fleet with no hosts.yaml (Options.Hosts nil — every pre-C7b test
// server, and any single-box daemon) declares no classes and must keep
// warming everything.
func TestWarmClassRefusal_NoHostsFileRefusesNothing(t *testing.T) {
	s := New([]Cell{{Name: "heavy", URL: "http://127.0.0.1:1"}}, t.TempDir()+"/hist.json", testDaemonInfo, Options{})
	t.Cleanup(s.Close)
	if why := s.warmClassRefusal("anything"); why != "" {
		t.Errorf("refused %q with no hosts.yaml at all", why)
	}
}

// TestDoctor_RefusedWarmScheduleReportsTheClassNotTheCron.
//
// The wiring refusal parks the entry with no next_fire, which is
// correct — and lands it in `warm.policy`'s findings, which is also
// correct, because a declared schedule that will never fire IS the warm
// policy not doing what it was declared to do. What the check must not
// do is describe it as a missing fire time and stop: three different
// causes park an entry that way (an invalid cron, a spec with no fire
// inside the scan horizon, and now a model the warm verb cannot be
// fired at), and only one of them is about the cron field the message
// quotes. C13's rule that a check names what it PROVES applies to the
// detail line too.
func TestDoctor_RefusedWarmScheduleReportsTheClassNotTheCron(t *testing.T) {
	dir := t.TempDir()
	s := New([]Cell{
		{Name: fleetcfg.FrontCell, URL: "http://127.0.0.1:1"},
		{Name: "heavy", URL: "http://127.0.0.1:1", Class: "always_on"},
	}, filepath.Join(dir, "hist.json"), testDaemonInfo, Options{
		IntentPath:   filepath.Join(dir, "intent.json"),
		LastSeenPath: filepath.Join(dir, "last-seen.json"),
		Hosts:        classedHosts(),
	})
	t.Cleanup(s.Close)
	s.startScheduleLoopWithConfig([]WarmScheduleEntry{{Cron: "0 6 * * *", Model: "lab-embed-b"}},
		func(string) (string, error) { return "heavy", nil }, time.UTC,
		func(context.Context, string, string) error { return nil }, "http://front.test")

	got := mustCheck(t, s.Doctor(context.Background()), "warm.policy", "")
	if got.Level != LevelWarn {
		t.Fatalf("warm.policy → %s (%s), want warn: a schedule that can never fire is a declared policy that is not running", got.Level, got.Detail)
	}
	if !strings.Contains(got.Detail, "embed-class") {
		t.Errorf("detail = %q, want the refusal's reason carried — an operator reading only "+
			"\"no resolved next fire\" goes and debugs a cron field that is fine", got.Detail)
	}
}
