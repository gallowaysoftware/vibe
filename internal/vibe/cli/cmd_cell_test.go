package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/swaptest"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
)

// cannedFleetd serves a scripted /api/fleet/state.
func cannedFleetd(t *testing.T, stateFn func() fleetapi.StateSnapshot) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/fleet/state", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(stateFn())
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func statusState(cells ...fleetapi.CellSnapshot) fleetapi.StateSnapshot {
	return fleetapi.StateSnapshot{
		GeneratedAt: time.Now(),
		Cells:       cells,
		Daemon:      fleetapi.DaemonInfo{AuthRejected: 2},
	}
}

func TestCellStatusRendersDerivedTable(t *testing.T) {
	now := time.Now()
	ts := cannedFleetd(t, func() fleetapi.StateSnapshot {
		return statusState(
			fleetapi.CellSnapshot{
				Name: "front", URL: "http://front.lan:9000", Class: "always_on",
				Reachable: true, Display: fleetapi.DisplayServing,
				Models: []fleetapi.ModelState{{ID: "kimi-k3", State: "stopped"}},
			},
			fleetapi.CellSnapshot{
				Name: "gpu-cell", URL: "http://gpu.lan:9000", Class: "opportunistic",
				Reachable: false, Display: fleetapi.DisplayDrained,
				Intent: &fleetapi.Intent{State: "drained", Reason: "gaming", ETA: "23:00", Since: now},
			},
			fleetapi.CellSnapshot{
				Name: "laptop", URL: "http://laptop.lan:9000", Class: "roaming",
				Reachable: false, Display: fleetapi.DisplayOffAway,
				LastSeen: &[]time.Time{now.Add(-2 * time.Hour)}[0],
			},
		)
	})

	var out bytes.Buffer
	target, err := resolveFleetd(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := target.fetchState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	renderStatus(&out, target.base, snap)
	s := out.String()
	for _, want := range []string{
		"front", "SERVING", "always_on",
		"gpu-cell", "DRAINED", "gaming, eta 23:00",
		"laptop", "OFF/AWAY", "last seen",
		"auth rejections since start: 2",
		"ui: http://front.lan:9000/ui",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("status output missing %q:\n%s", want, s)
		}
	}
}

func TestCellStatusDegradedFallback(t *testing.T) {
	// fleetd is dead; hosts.yaml has cells. One cell answers a direct
	// /v1/models probe, the other doesn't.
	//
	// The live cell is internal/swaptest's double rather than a handler
	// that answers the same canned catalog to every path. Two differences
	// are load-bearing here: its rows carry `created` and `owned_by` (the
	// real catalog's full field set, so a decoder that grew a dependency on
	// either fails here rather than in production), and it answers only the
	// routes llama-swap actually serves — a probe that drifted onto the
	// wrong path used to get a 200 and a catalog back regardless.
	liveCell := swaptest.NewCell(t, swaptest.WithModels(
		swaptest.Model{ID: "qwen3.6-27b", State: "ready", TTL: 1800}))
	deadLn, _ := net.Listen("tcp", "127.0.0.1:0")
	deadURL := "http://" + deadLn.Addr().String()
	deadLn.Close()

	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if err := os.MkdirAll(filepath.Join(xdg, "vibe"), 0o755); err != nil {
		t.Fatal(err)
	}
	hostsYAML := fmt.Sprintf(`
fleetd_url: "%s"
cells:
  front:  { url: "%s", class: always_on }
  gpu-cell: { url: "%s", class: opportunistic }
`, deadURL, liveCell.URL(), deadURL)
	if err := os.WriteFile(filepath.Join(xdg, "vibe", "hosts.yaml"), []byte(hostsYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	// --api pointing at the dead fleetd forces the degraded path.
	var out bytes.Buffer
	err := renderDegraded(&out, fmt.Errorf("connection refused"))
	if err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"DEGRADED", "front", "up", "qwen3.6-27b", "gpu-cell", "down"} {
		if !strings.Contains(s, want) {
			t.Errorf("degraded output missing %q:\n%s", want, s)
		}
	}
}

func TestCellAwaitUnblocksOnTransition(t *testing.T) {
	var up atomic.Bool
	ts := cannedFleetd(t, func() fleetapi.StateSnapshot {
		return statusState(fleetapi.CellSnapshot{
			Name: "gpu-cell", URL: "http://gpu.lan:9000", Reachable: up.Load(),
			Display: fleetapi.DisplayServing,
		})
	})
	// Cell comes up 300ms in.
	go func() {
		time.Sleep(300 * time.Millisecond)
		up.Store(true)
	}()

	var out bytes.Buffer
	start := time.Now()
	target, terr := resolveFleetd(ts.URL)
	if terr != nil {
		t.Fatal(terr)
	}
	_, err := awaitCell(t.Context(), &out, target, "gpu-cell", awaitConds{wantUp: true}, 5*time.Second, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("await took %v, want unblock within ~one interval of the transition", elapsed)
	}
	if !strings.Contains(out.String(), "gpu-cell is up") {
		t.Errorf("output = %q", out.String())
	}
}

func TestCellAwaitViaEventsStream(t *testing.T) {
	// fleetd with an SSE stream: the transition event must unblock the
	// await before the poll interval matters. The cell becomes reachable
	// at the same moment the event is emitted — the event is what makes
	// await LOOK, and the snapshot is what decides (C10 review: a
	// fixture whose state contradicted its own event was asserting that
	// the event alone could return, which is how `--down` unblocked on
	// `fleet.cellStale` against a cell C6 deliberately keeps reachable).
	var reachable atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/fleet/state", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(fleetapi.StateSnapshot{
			GeneratedAt: time.Now(),
			Cells: []fleetapi.CellSnapshot{{
				Name: "gpu-cell", URL: "http://gpu.lan:9000", Reachable: reachable.Load(),
			}},
		})
	})
	mux.HandleFunc("GET /api/fleet/events", func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		time.Sleep(200 * time.Millisecond)
		reachable.Store(true)
		fmt.Fprintf(w, "event:message\ndata:{\"cell\":\"gpu-cell\",\"type\":\"fleet.cellReturned\"}\n\n")
		flusher.Flush()
		<-r.Context().Done()
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	var out bytes.Buffer
	target, terr := resolveFleetd(ts.URL)
	if terr != nil {
		t.Fatal(terr)
	}
	start := time.Now()
	_, err := awaitCell(t.Context(), &out, target, "gpu-cell", awaitConds{wantUp: true}, 5*time.Second, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v — the events stream didn't drive the unblock (poll fallback did)", elapsed)
	}
	if !strings.Contains(out.String(), "gpu-cell is up") {
		t.Errorf("out = %q", out.String())
	}
}

// TestCellAwaitUnknownCellFailsFast: fleetd answering "no such cell" can
// only be a typo, and `--timeout 0` is the documented overnight-batch
// idiom — so it errors immediately instead of parking until reboot. The
// 3s context is the regression detector: the old behaviour waits it out.
func TestCellAwaitUnknownCellFailsFast(t *testing.T) {
	ts := cannedFleetd(t, func() fleetapi.StateSnapshot { return statusState() })
	var out bytes.Buffer
	target, terr := resolveFleetd(ts.URL)
	if terr != nil {
		t.Fatal(terr)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_, err := awaitCell(ctx, &out, target, "nope", awaitConds{wantUp: true}, 0, 50*time.Millisecond)
	if !errors.Is(err, errUnknownCell) {
		t.Errorf("unknown cell: got %v, want errUnknownCell", err)
	}
}

func TestCellAwaitDown(t *testing.T) {
	ts := cannedFleetd(t, func() fleetapi.StateSnapshot {
		return statusState(fleetapi.CellSnapshot{
			Name: "gpu-cell", URL: "http://gpu.lan:9000", Reachable: false,
		})
	})
	var out bytes.Buffer
	target, terr := resolveFleetd(ts.URL)
	if terr != nil {
		t.Fatal(terr)
	}
	if _, err := awaitCell(t.Context(), &out, target, "gpu-cell", awaitConds{}, 2*time.Second, 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "gpu-cell is down") {
		t.Errorf("output = %q", out.String())
	}
}
