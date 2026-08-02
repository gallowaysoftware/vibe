package cli

import (
	"bytes"
	"encoding/json"
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
	target := resolveFleetd(ts.URL)
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
	liveCell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"qwen3.6-27b","object":"model"}]}`))
	}))
	t.Cleanup(liveCell.Close)
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
`, deadURL, liveCell.URL, deadURL)
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
	err := awaitCell(t.Context(), &out, resolveFleetd(ts.URL), "gpu-cell", true, 5*time.Second, 50*time.Millisecond)
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

func TestCellAwaitUnknownCell(t *testing.T) {
	ts := cannedFleetd(t, func() fleetapi.StateSnapshot { return statusState() })
	var out bytes.Buffer
	err := awaitCell(t.Context(), &out, resolveFleetd(ts.URL), "nope", true, 200*time.Millisecond, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Errorf("unknown cell: got %v, want timeout (unknown cells keep waiting, not silently succeeding)", err)
	}
}

func TestCellAwaitDown(t *testing.T) {
	ts := cannedFleetd(t, func() fleetapi.StateSnapshot {
		return statusState(fleetapi.CellSnapshot{
			Name: "gpu-cell", URL: "http://gpu.lan:9000", Reachable: false,
		})
	})
	var out bytes.Buffer
	if err := awaitCell(t.Context(), &out, resolveFleetd(ts.URL), "gpu-cell", false, 2*time.Second, 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "gpu-cell is down") {
		t.Errorf("output = %q", out.String())
	}
}
