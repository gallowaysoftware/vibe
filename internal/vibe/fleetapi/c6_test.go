package fleetapi

// C6 substrate repair: presence must not discard positive probe evidence
// (M5), a complied drain must keep its reason and ETA (M6), and every
// intent writer clones → persists → swaps (MIN-C).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/router"
)

func c6Cells() []Cell {
	return []Cell{
		{Name: "front", URL: "http://127.0.0.1:1", Class: "always_on"},
		{Name: "laptop", URL: "http://127.0.0.1:1", Class: "roaming"},
	}
}

// TestDecorate_StalePresenceKeepsProbeEvidence pins M5: a dead announcer
// must not retract a probe that just answered.
func TestDecorate_StalePresenceKeepsProbeEvidence(t *testing.T) {
	for _, tc := range []struct {
		name      string
		stale     bool
		withdrawn bool
	}{
		{name: "stale", stale: true},
		{name: "withdrawn", withdrawn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := newFleetdServer(t, c6Cells())
			s.mu.Lock()
			s.presence["laptop"] = &Presence{
				Cell:       "laptop",
				Announcing: true,
				Stale:      tc.stale,
				Withdrawn:  tc.withdrawn,
				IntentEcho: &AnnounceIntent{State: "serving"},
				Models:     []AnnounceModel{{ID: "qwen", State: "ready"}},
				ReceivedAt: time.Now().UTC().Add(-time.Hour),
			}
			s.mu.Unlock()

			snap := CellSnapshot{Name: "laptop", Reachable: true, Models: []ModelState{{ID: "qwen", State: "ready"}}}
			s.decorate(&snap)
			if !snap.Reachable {
				t.Errorf("probe answered but snapshot says unreachable: %+v", snap)
			}
			if snap.Display != DisplayServing {
				t.Errorf("display = %q, want %q", snap.Display, DisplayServing)
			}
		})
	}
}

// TestDecorate_StalePresenceWithoutProbeIsUnreachable is M5's mirror: with
// no probe evidence the stale announce still reads as absent.
func TestDecorate_StalePresenceWithoutProbeIsUnreachable(t *testing.T) {
	s, _, _ := newFleetdServer(t, c6Cells())
	s.mu.Lock()
	s.presence["laptop"] = &Presence{
		Cell:       "laptop",
		Announcing: true,
		Withdrawn:  true,
		IntentEcho: &AnnounceIntent{State: "withdrawing"},
		ReceivedAt: time.Now().UTC().Add(-time.Hour),
	}
	s.mu.Unlock()

	snap := CellSnapshot{Name: "laptop", Reachable: false}
	s.decorate(&snap)
	if snap.Reachable {
		t.Error("no probe evidence but snapshot says reachable")
	}
	if snap.HostReachable == nil || *snap.HostReachable {
		t.Errorf("withdrawn cell must read host-unreachable: %+v", snap.HostReachable)
	}
}

// TestRecordAnnounce_CompliedDrainKeepsReasonAndETA pins M6: the reason
// and ETA survive the cell's ack, and the resolved entry is not pending.
func TestRecordAnnounce_CompliedDrainKeepsReasonAndETA(t *testing.T) {
	s, _, _ := newFleetdServer(t, c6Cells())
	if _, err := s.SetIntent("laptop", "drained", "gaming", "23:00"); err != nil {
		t.Fatal(err)
	}
	echoAt := time.Now().UTC().Add(time.Second)
	resp := s.recordAnnounce(&AnnounceRequest{
		V: 1, Cell: "laptop", Seq: 1,
		Intent: &AnnounceIntent{State: "drained", Since: echoAt},
	})
	if resp.DesiredIntent != nil {
		t.Errorf("desired = %+v, want nil (request satisfied by the echo)", resp.DesiredIntent)
	}

	snap := CellSnapshot{Name: "laptop"}
	s.decorate(&snap)
	if snap.Intent == nil {
		t.Fatal("intent gone after the cell acked the drain")
	}
	if snap.Intent.Reason != "gaming" || snap.Intent.ETA != "23:00" {
		t.Errorf("intent = %+v, want reason/eta preserved", snap.Intent)
	}
	if snap.IntentPending {
		t.Error("a complied drain must not read as a pending request")
	}

	// The reason also survives a fleetd restart (the entry, not just the
	// in-memory decoration, carries it).
	s.mu.Lock()
	stored, ok := s.intents["laptop"]
	s.mu.Unlock()
	if !ok || stored.Reason != "gaming" || stored.ETA != "23:00" {
		t.Errorf("stored intent = %+v (ok=%v), want the reason persisted", stored, ok)
	}
}

// TestRecordAnnounce_DivergingEchoClearsRequest keeps the conflict rule's
// other half: a newer echo that disagrees drops the request outright.
func TestRecordAnnounce_DivergingEchoClearsRequest(t *testing.T) {
	s, _, _ := newFleetdServer(t, c6Cells())
	if _, err := s.SetIntent("laptop", "drained", "gaming", "23:00"); err != nil {
		t.Fatal(err)
	}
	resp := s.recordAnnounce(&AnnounceRequest{
		V: 1, Cell: "laptop", Seq: 1,
		Intent: &AnnounceIntent{State: "serving", Since: time.Now().UTC().Add(time.Minute)},
	})
	if resp.DesiredIntent != nil {
		t.Errorf("desired = %+v, want nil", resp.DesiredIntent)
	}
	s.mu.Lock()
	_, stillThere := s.intents["laptop"]
	s.mu.Unlock()
	if stillThere {
		t.Error("a diverging newer echo must drop the request")
	}
}

// TestRecordAnnounce_PersistFailureKeepsRequest pins MIN-C: memory never
// resolves ahead of the file.
func TestRecordAnnounce_PersistFailureKeepsRequest(t *testing.T) {
	s, _, opts := newFleetdServer(t, c6Cells())
	if _, err := s.SetIntent("laptop", "drained", "gaming", "23:00"); err != nil {
		t.Fatal(err)
	}

	// A regular file where a directory must be: MkdirAll fails for any
	// user, root included, so the test doesn't depend on who runs it.
	blocker := filepath.Join(filepath.Dir(opts.IntentPath), "blocked")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.intentPath = filepath.Join(blocker, "intent.json")

	s.recordAnnounce(&AnnounceRequest{
		V: 1, Cell: "laptop", Seq: 1,
		Intent: &AnnounceIntent{State: "serving", Since: time.Now().UTC().Add(time.Minute)},
	})
	got := s.Snapshot(context.Background())
	var laptop *CellSnapshot
	for i := range got.Cells {
		if got.Cells[i].Name == "laptop" {
			laptop = &got.Cells[i]
		}
	}
	if laptop == nil {
		t.Fatal("laptop missing from the snapshot")
	}
	if laptop.Intent == nil || laptop.Intent.State != "drained" {
		t.Errorf("intent = %+v, want the drained request retained after a failed persist", laptop.Intent)
	}

	// The next successful persist resolves it.
	s.intentPath = opts.IntentPath
	s.recordAnnounce(&AnnounceRequest{
		V: 1, Cell: "laptop", Seq: 2,
		Intent: &AnnounceIntent{State: "serving", Since: time.Now().UTC().Add(2 * time.Minute)},
	})
	s.mu.Lock()
	_, stillThere := s.intents["laptop"]
	s.mu.Unlock()
	if stillThere {
		t.Error("request survived a successful persist")
	}
	data, err := os.ReadFile(opts.IntentPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(loadIntentsFromBytes(t, data)) != 0 {
		t.Errorf("intent file still carries entries: %s", data)
	}
}

func loadIntentsFromBytes(t *testing.T, data []byte) map[string]Intent {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "intent.json")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return loadIntents(p)
}

// TestRenderLoop_SteadyStateFingerprintDriftEnforced pins M7: a
// flags_sha256 change with an unchanged model-id set on an always_on
// cell (no membership transition of any kind) must still raise the loud
// event and pull a strict def out of the rendered config.
func TestRenderLoop_SteadyStateFingerprintDriftEnforced(t *testing.T) {
	strictDef := llmDef("embed-x", "gpu", "strict")
	cells := []Cell{
		{Name: "front", URL: "http://127.0.0.1:1", Class: "always_on"},
		{Name: "gpu", URL: "http://127.0.0.1:3", Class: "always_on"},
	}
	hosts := &fleetcfg.File{Cells: map[string]fleetcfg.Cell{
		"front": {URL: "http://127.0.0.1:1", Class: fleetcfg.ClassAlwaysOn},
		"gpu":   {URL: "http://127.0.0.1:3", Class: fleetcfg.ClassAlwaysOn},
	}}
	probe := newRenderProbe(strictDef)

	cfg := RenderLoopConfig{FullWaveTimeout: 30 * time.Second, RenderMinInterval: time.Millisecond}
	cfg.Hosts = hosts
	cfg.FrontConfigPath = filepath.Join(t.TempDir(), "front.yaml")
	cfg.LoadDefs = probe.loadDefs
	cfg.Render = probe.render
	cfg.WriteFile = probe.writeFile
	s := New(cells, filepath.Join(t.TempDir(), "hist.json"), testDaemonInfo, Options{})
	t.Cleanup(s.Close)

	mux := http.NewServeMux()
	s.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch := subscribeEvents(t, ctx, ts)

	s.StartRenderLoop(cfg)

	cmd, err := router.ModelCmd(strictDef, router.Options{Hosts: hosts})
	if err != nil {
		t.Fatal(err)
	}
	expected, err := router.FlagsSHA256(cmd)
	if err != nil {
		t.Fatal(err)
	}

	// Steady state: both cells announced, the hash matches, the def is in.
	rlAnnounce(t, s, "front", rlServing(), nil)
	rlAnnounce(t, s, "gpu", rlServing(), []AnnounceModel{{ID: "embed-x", State: "ready", FlagsSHA256: expected}})
	probe.waitPass(t, "render with the matching def", func(n []string) bool {
		return slices.Contains(n, "embed-x")
	})
	probe.waitWrite(t, "initial front config")
	// The cold-start wave leaves one trailing coalesced pass behind the
	// cooldown; let it land before asserting silence.
	settleRenders(probe)

	// A repeat announce with the SAME hash is not a transition: nothing
	// re-renders (otherwise the trigger fires every heartbeat).
	rlAnnounce(t, s, "gpu", rlServing(), []AnnounceModel{{ID: "embed-x", State: "ready", FlagsSHA256: expected}})
	probe.assertSilent(t, 300*time.Millisecond, "unchanged fingerprint")

	// Now the serving flags drift, id set unchanged.
	wrong := strings.Repeat("0", 64)
	if wrong == expected {
		t.Fatal("hash collision with the canary value")
	}
	s.recordAnnounce(&AnnounceRequest{
		V: AnnounceVersion, Cell: "gpu", Seq: 3, Intent: rlServing(),
		Models:   []AnnounceModel{{ID: "embed-x", State: "ready", FlagsSHA256: wrong}},
		Versions: &AnnounceVersions{DefsSHA: "abc123", DefsDirty: true},
	})

	names := probe.waitPass(t, "render excluding the drifted strict def", func(n []string) bool {
		return !slices.Contains(n, "embed-x")
	})
	if len(names) != 0 {
		t.Fatalf("render defs = %v, want the strict def excluded", names)
	}
	ev := waitFor(t, ch, "fingerprint mismatch event", func(ev Event) bool { return ev.Type == EventFingerprint })
	var pl struct {
		Model string `json:"model"`
		Cell  string `json:"cell"`
		Mode  string `json:"mode"`
		Got   string `json:"got"`
	}
	if err := json.Unmarshal(ev.Data, &pl); err != nil {
		t.Fatal(err)
	}
	if pl.Model != "embed-x" || pl.Cell != "gpu" || pl.Mode != "strict" || pl.Got != wrong {
		t.Fatalf("event payload = %+v", pl)
	}
}

// TestModelFingerprintChanged covers the trigger predicate directly,
// including the case the render trigger must NOT fire on (state flips).
func TestModelFingerprintChanged(t *testing.T) {
	a := []AnnounceModel{{ID: "m", State: "ready", FlagsSHA256: "aaa"}}
	for _, tc := range []struct {
		name string
		prev []AnnounceModel
		next []AnnounceModel
		want bool
	}{
		{name: "same hash", prev: a, next: a},
		{name: "state flip only", prev: a, next: []AnnounceModel{{ID: "m", State: "stopped", FlagsSHA256: "aaa"}}},
		{name: "hash drift", prev: a, next: []AnnounceModel{{ID: "m", State: "ready", FlagsSHA256: "bbb"}}, want: true},
		{name: "hash appears", prev: []AnnounceModel{{ID: "m", State: "ready"}}, next: a, want: true},
		{name: "hash disappears", prev: a, next: []AnnounceModel{{ID: "m", State: "ready"}}},
		{name: "new id", prev: a, next: append(slices.Clone(a), AnnounceModel{ID: "n", FlagsSHA256: "ccc"})},
		{name: "first announce", prev: nil, next: a},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := modelFingerprintChanged(tc.prev, tc.next); got != tc.want {
				t.Errorf("modelFingerprintChanged = %v, want %v", got, tc.want)
			}
		})
	}
}

// settleRenders waits until the loop has been quiet for one window, so a
// trailing coalesced pass from an earlier trigger can't be mistaken for
// the pass a later assertion is about.
func settleRenders(p *renderProbe) {
	for {
		select {
		case <-p.passCh:
		case <-p.writeCh:
		case <-time.After(150 * time.Millisecond):
			return
		}
	}
}

// waitStale blocks until the REAL staleness loop has marked the cell.
// Tests must never carry their own copy of the predicate: the copy is
// what let a short-circuited production predicate keep the suite green.
func waitStale(t *testing.T, s *Server, cell string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if p := s.presenceFor(cell); p != nil && p.Stale {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("cell %q never went stale", cell)
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// TestStalenessLoop_PublishesPrunesAndTriggersRender is M8's replacement
// for the test-file copy of the predicate: it drives the production loop
// and asserts its whole tail — the stale event, the render trigger, and
// the unresolvable serving request being dropped.
func TestStalenessLoop_PublishesPrunesAndTriggersRender(t *testing.T) {
	s, ts, _ := newFleetdServer(t, c6Cells())
	s.stalenessTick = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch := subscribeEvents(t, ctx, ts)

	// An announcing cell with a resume REQUEST pending: only an
	// announcing cell can hold a stored serving intent, and only that
	// entry is what pruneStaleServingRequest exists to drop.
	s.recordAnnounce(&AnnounceRequest{
		V: AnnounceVersion, Cell: "laptop", Seq: 1,
		Intent: &AnnounceIntent{State: "serving", Since: time.Now().UTC().Add(-time.Hour)},
	})
	if _, err := s.SetIntent("laptop", "serving", "", ""); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	if _, ok := s.intents["laptop"]; !ok {
		s.mu.Unlock()
		t.Fatal("serving request not stored for an announcing cell")
	}
	s.mu.Unlock()

	// Drain the triggers the announce itself produced, so the trigger
	// asserted below is the staleness loop's.
	for drained := false; !drained; {
		select {
		case <-s.renderTrigger:
		default:
			drained = true
		}
	}

	s.Start()
	s.mu.Lock()
	s.presence["laptop"].ReceivedAt = time.Now().UTC().Add(-time.Hour)
	s.mu.Unlock()

	waitFor(t, ch, "fleet.cellStale", func(ev Event) bool {
		return ev.Type == EventCellStale && ev.Cell == "laptop"
	})

	select {
	case cell := <-s.renderTrigger:
		if cell != "laptop" {
			t.Errorf("render trigger for %q, want laptop", cell)
		}
	case <-time.After(5 * time.Second):
		t.Error("staleness never triggered a render")
	}

	deadline := time.After(5 * time.Second)
	for {
		s.mu.Lock()
		_, stillThere := s.intents["laptop"]
		s.mu.Unlock()
		if !stillThere {
			break
		}
		select {
		case <-deadline:
			t.Fatal("serving request survived the cell going stale")
		case <-time.After(5 * time.Millisecond):
		}
	}

	data, err := os.ReadFile(filepathJoinIntent(t, s))
	if err != nil {
		t.Fatal(err)
	}
	if len(loadIntentsFromBytes(t, data)) != 0 {
		t.Errorf("prune not persisted: %s", data)
	}
}

func filepathJoinIntent(t *testing.T, s *Server) string {
	t.Helper()
	if s.intentPath == "" {
		t.Fatal("intent store disabled")
	}
	return s.intentPath
}

// TestRecordAnnounce_PersistsLastSeen pins MIN-E: an announce-only cell
// (no inbound port — C3's whole destination) must leave a persisted
// sighting, and steady-state heartbeats must not rewrite the file every
// time.
func TestRecordAnnounce_PersistsLastSeen(t *testing.T) {
	s, _, opts := newFleetdServer(t, c6Cells())
	s.recordAnnounce(&AnnounceRequest{V: AnnounceVersion, Cell: "laptop", Seq: 1, Intent: &AnnounceIntent{State: "serving"}})

	first, ok := loadLastSeen(opts.LastSeenPath)["laptop"]
	if !ok || first.IsZero() {
		t.Fatal("first announce left no persisted sighting")
	}

	// A steady-state heartbeat inside the age window must not rewrite:
	// at 15s cadence x N cells that is a full-file write per heartbeat.
	s.recordAnnounce(&AnnounceRequest{V: AnnounceVersion, Cell: "laptop", Seq: 2, Intent: &AnnounceIntent{State: "serving"}})
	if got := loadLastSeen(opts.LastSeenPath)["laptop"]; !got.Equal(first) {
		t.Errorf("steady-state heartbeat rewrote the file (%v then %v)", first, got)
	}

	// A transition (return from stale) forces the write.
	s.mu.Lock()
	s.presence["laptop"].Stale = true
	s.mu.Unlock()
	s.recordAnnounce(&AnnounceRequest{V: AnnounceVersion, Cell: "laptop", Seq: 3, Intent: &AnnounceIntent{State: "serving"}})
	if got := loadLastSeen(opts.LastSeenPath)["laptop"]; !got.After(first) {
		t.Errorf("a return transition did not persist the new sighting: %v", got)
	}
}

// TestDrainCommands_AtLeastOnce pins MIN-H: the batch is retired only by
// an announce with a HIGHER seq (proof the cell read the response), and
// a seq reset (cell reboot) cannot pin the slot forever.
func TestDrainCommands_AtLeastOnce(t *testing.T) {
	s, _, _ := newFleetdServer(t, c6Cells())
	s.recordAnnounce(&AnnounceRequest{V: AnnounceVersion, Cell: "laptop", Seq: 1,
		Models: []AnnounceModel{{ID: "qwen", State: "ready"}}})
	if err := s.QueueCommand("laptop", AnnounceCommand{Verb: "unload", Model: "qwen"}); err != nil {
		t.Fatal(err)
	}

	// Seq 2 collects the batch; a retried POST at the same seq (the
	// response the cell never read) gets it again.
	if got := s.drainCommands("laptop", 2); len(got) != 1 {
		t.Fatalf("first delivery = %+v, want 1 command", got)
	}
	if got := s.drainCommands("laptop", 2); len(got) != 1 {
		t.Errorf("retry at the same seq lost the command: %+v", got)
	}
	// A reboot resets seq; the batch is redelivered, not pinned.
	if got := s.drainCommands("laptop", 0); len(got) != 1 {
		t.Errorf("seq reset lost the command: %+v", got)
	}
	// Seq 1 > 0 now retires it.
	if got := s.drainCommands("laptop", 1); len(got) != 0 {
		t.Errorf("a higher seq must retire the batch: %+v", got)
	}
}

// TestQueueCommand_ValidatesAgainstAnnouncedModels: a verb nothing would
// ever execute is a caller error, not a queue entry.
func TestQueueCommand_ValidatesAgainstAnnouncedModels(t *testing.T) {
	s, _, _ := newFleetdServer(t, c6Cells())
	if err := s.QueueCommand("laptop", AnnounceCommand{Verb: "unload", Model: "qwen"}); err == nil {
		t.Error("queued a command for a cell that has never announced")
	}
	s.recordAnnounce(&AnnounceRequest{V: AnnounceVersion, Cell: "laptop", Seq: 1,
		Models: []AnnounceModel{{ID: "qwen", State: "ready"}}})
	if err := s.QueueCommand("laptop", AnnounceCommand{Verb: "unload", Model: "typo"}); err == nil {
		t.Error("queued a command for a model the cell does not announce")
	}
	if err := s.QueueCommand("laptop", AnnounceCommand{Verb: "explode", Model: "qwen"}); err == nil {
		t.Error("queued an unknown verb")
	}
	if err := s.QueueCommand("laptop", AnnounceCommand{Verb: "warm", Model: "qwen"}); err != nil {
		t.Errorf("valid command rejected: %v", err)
	}
}

// TestWriteAtomic_ModeIsReadable pins MIN-A: the front config exists to
// be read by another process, and an operator's wider mode survives.
func TestWriteAtomic_ModeIsReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "front.yaml")
	if err := writeAtomic(path, []byte("a: 1\n")); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644 (llama-swap reads it as another user)", st.Mode().Perm())
	}
	if err := os.Chmod(path, 0o664); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(path, []byte("a: 2\n")); err != nil {
		t.Fatal(err)
	}
	st, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o664 {
		t.Errorf("mode = %v, want the operator's 0664 preserved", st.Mode().Perm())
	}

	// The upgrade case: every fleetd deployed before this fix left a 0600
	// file behind. Inheriting that mode would keep the bug alive on
	// exactly the boxes that have it, so the read bits come back.
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(path, []byte("a: 3\n")); err != nil {
		t.Fatal(err)
	}
	st, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644 (a pre-fix 0600 config must be repaired, not inherited)", st.Mode().Perm())
	}
}

// TestLeaseIngestValidated pins MIN-J: the lease endpoint sanitises what
// it prints into the same status tables the announce ingest guards, and
// bounds the TTL.
func TestLeaseIngestValidated(t *testing.T) {
	_, ts, _ := newFleetdServer(t, c6Cells())
	post := func(body string) int {
		resp, err := http.Post(ts.URL+"/api/fleet/lease", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	cases := map[string]string{
		"control char in note": `{"cell":"laptop","model":"qwen","holder":"batch","note":"a\u0001b","ttl":"1h"}`,
		"oversize holder":      `{"cell":"laptop","model":"qwen","holder":"` + strings.Repeat("h", 300) + `","ttl":"1h"}`,
		"absurd ttl":           `{"cell":"laptop","model":"qwen","holder":"batch","ttl":"9000h"}`,
	}
	for name, body := range cases {
		if code := post(body); code != http.StatusBadRequest {
			t.Errorf("%s: HTTP %d, want 400", name, code)
		}
	}
	if code := post(`{"cell":"laptop","model":"qwen","holder":"batch","note":"mid-batch","ttl":"2h"}`); code != http.StatusOK {
		t.Errorf("clean lease rejected: HTTP %d", code)
	}
}

// TestLeaseCapGatesGrowthOnly: the cap must never refuse the DELETE that
// drains an over-cap store — its own message tells the caller to delete.
func TestLeaseCapGatesGrowthOnly(t *testing.T) {
	s, ts, _ := newFleetdServer(t, c6Cells())
	over := map[string]Lease{}
	for i := range maxLeases + 5 {
		l := Lease{Cell: "laptop", Model: fmt.Sprintf("m%d", i), Holder: "batch",
			ExpiresAt: time.Now().UTC().Add(time.Hour)}
		over[leaseKey(l.Cell, l.Model, l.Holder)] = l
	}
	s.mu.Lock()
	s.leases = over
	s.mu.Unlock()

	do := func(method, body string) int {
		req, err := http.NewRequest(method, ts.URL+"/api/fleet/lease", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if code := do(http.MethodDelete, `{"cell":"laptop","model":"m0","holder":"batch"}`); code != http.StatusOK {
		t.Errorf("DELETE against a full store: HTTP %d, want 200 (delete is the way out)", code)
	}
	if code := do(http.MethodPost, `{"cell":"laptop","model":"new","holder":"batch","ttl":"1h"}`); code != http.StatusConflict {
		t.Errorf("POST against a full store: HTTP %d, want 409", code)
	}
	s.mu.Lock()
	n := len(s.leases)
	s.mu.Unlock()
	if n != maxLeases+4 {
		t.Errorf("store size = %d, want %d (the delete landed, the add did not)", n, maxLeases+4)
	}
}
