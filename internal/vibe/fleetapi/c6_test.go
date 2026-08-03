package fleetapi

// C6 substrate repair: presence must not discard positive probe evidence
// (M5), a complied drain must keep its reason and ETA (M6), and every
// intent writer clones → persists → swaps (MIN-C).

import (
	"context"
	"encoding/json"
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
