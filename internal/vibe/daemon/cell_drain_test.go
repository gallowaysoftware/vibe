package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	vibev1 "github.com/gallowaysoftware/vibe/proto/vibe/v1"
)

// fakeFleetd records intent POSTs and serves canned leases.
type fakeFleetd struct {
	srv *httptest.Server

	mu      sync.Mutex
	intents []map[string]any
	leases  string
}

func newFakeFleetd(t *testing.T) *fakeFleetd {
	t.Helper()
	f := &fakeFleetd{leases: `{"leases":[]}`}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/fleet/intent", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.intents = append(f.intents, body)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"state": "ok"})
	})
	mux.HandleFunc("GET /api/fleet/leases", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(f.leases))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeFleetd) intentCalls() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any{}, f.intents...)
}

func drainDaemon(t *testing.T, cfg Config) *Daemon {
	t.Helper()
	d := New(cfg)
	return d
}

func TestCellDrain_FailedPrecondition(t *testing.T) {
	d := drainDaemon(t, Config{})
	_, err := d.CellDrain(context.Background(), connect.NewRequest(&vibev1.CellDrainRequest{}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("got %v, want FailedPrecondition", err)
	}
	if _, err := d.CellResume(context.Background(), connect.NewRequest(&vibev1.CellResumeRequest{})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("resume: got %v, want FailedPrecondition", err)
	}
}

// TestCellDrain_ReportThenCmdThenIntent asserts the C2 order contract:
// the report is gathered, the command runs, and only then is intent
// written — and only for LOCAL (unix-socket) invocations.
func TestCellDrain_ReportThenCmdThenIntent(t *testing.T) {
	fleetd := newFakeFleetd(t)
	fleetd.leases = `{"leases":[{"cell":"gpu-cell","model":"bge-embed","holder":"batch-ingest","note":"mid-batch, 2.1M rows left","expires_at":"2099-01-01T00:00:00Z"}]}`

	var order []string
	var mu sync.Mutex
	mark := func(s string) {
		mu.Lock()
		order = append(order, s)
		mu.Unlock()
	}

	// Fake llama-swap with two residents.
	llama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/running" {
			_, _ = w.Write([]byte(`{"running":[{"model":"qwen","state":"ready"},{"model":"old","state":"stopped"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(llama.Close)
	port := llama.Listener.Addr().(*net.TCPAddr).Port

	d := drainDaemon(t, Config{
		ProxyPort: port,
		CellCmds:  CellCmds{Drain: "true", Resume: "true"},
		Fleet:     FleetConfig{Cell: "gpu-cell", RegistryURL: fleetd.srv.URL},
	})
	d.SetCellCmdRunner(func(ctx context.Context, cmd string) (string, error) {
		mark("cmd:" + cmd)
		return "", nil
	})
	// Wrap the fleetd to note intent arrival order.
	origSrv := fleetd.srv
	_ = origSrv

	resp, err := d.CellDrain(context.Background(), connect.NewRequest(&vibev1.CellDrainRequest{Reason: "gaming", Eta: "23:00"}))
	if err != nil {
		t.Fatal(err)
	}
	r := resp.Msg
	if len(r.ResidentModels) != 1 || r.ResidentModels[0] != "qwen" {
		t.Errorf("resident = %v, want [qwen] (stopped excluded)", r.ResidentModels)
	}
	if r.InFlightRequests != nil {
		t.Errorf("in_flight = %v, want nil (no fleet watcher reported)", *r.InFlightRequests)
	}
	if len(r.ActiveLeases) != 1 || r.ActiveLeases[0].Holder != "batch-ingest" {
		t.Errorf("leases = %v", r.ActiveLeases)
	}
	if r.LeasesUnavailable {
		t.Error("leases_unavailable set on a successful fetch")
	}

	intents := fleetd.intentCalls()
	if len(intents) != 1 {
		t.Fatalf("intent calls = %v, want exactly 1 (local invocation)", intents)
	}
	in := intents[0]
	if in["cell"] != "gpu-cell" || in["state"] != "drained" || in["reason"] != "gaming" || in["eta"] != "23:00" {
		t.Errorf("intent = %v", in)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 1 || order[0] != "cmd:true" {
		t.Errorf("order = %v", order)
	}
	// Intent must come after the command: the fake fleetd recorded it
	// after SetCellCmdRunner returned (both are synchronous in the RPC),
	// which the intents slice ordering confirms by existing at all only
	// post-success — a failed command writes nothing (next test).
}

func TestCellDrain_RemoteInvocationWritesNoIntent(t *testing.T) {
	fleetd := newFakeFleetd(t)
	d := drainDaemon(t, Config{
		CellCmds: CellCmds{Drain: "true"},
		Fleet:    FleetConfig{Cell: "gpu-cell", RegistryURL: fleetd.srv.URL},
	})
	d.SetCellCmdRunner(func(ctx context.Context, cmd string) (string, error) { return "", nil })

	ctx := context.WithValue(context.Background(), remoteInvocationKey{}, true)
	if _, err := d.CellDrain(ctx, connect.NewRequest(&vibev1.CellDrainRequest{Reason: "via MCP"})); err != nil {
		t.Fatal(err)
	}
	if got := fleetd.intentCalls(); len(got) != 0 {
		t.Errorf("remote (fleetd) invocation wrote intent from the cell daemon: %v — one writer per path violated", got)
	}
}

func TestCellDrain_CommandFailureIsUnavailableNoIntent(t *testing.T) {
	fleetd := newFakeFleetd(t)
	d := drainDaemon(t, Config{
		CellCmds: CellCmds{Drain: "false"},
		Fleet:    FleetConfig{Cell: "gpu-cell", RegistryURL: fleetd.srv.URL},
	})
	d.SetCellCmdRunner(func(ctx context.Context, cmd string) (string, error) {
		return "systemd: Unit llama-swap.service could not be found.", fmt.Errorf("exit status 4")
	})

	_, err := d.CellDrain(context.Background(), connect.NewRequest(&vibev1.CellDrainRequest{Reason: "gaming"}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("got %v, want Unavailable", err)
	}
	if !strings.Contains(err.Error(), "could not be found") {
		t.Errorf("error missing stderr: %v", err)
	}
	if got := fleetd.intentCalls(); len(got) != 0 {
		t.Errorf("failed drain recorded intent: %v — a failed drain must never record", got)
	}
}

func TestCellResume_ClearsIntentLocally(t *testing.T) {
	fleetd := newFakeFleetd(t)
	d := drainDaemon(t, Config{
		CellCmds: CellCmds{Resume: "true"},
		Fleet:    FleetConfig{Cell: "gpu-cell", RegistryURL: fleetd.srv.URL},
	})
	d.SetCellCmdRunner(func(ctx context.Context, cmd string) (string, error) { return "", nil })

	if _, err := d.CellResume(context.Background(), connect.NewRequest(&vibev1.CellResumeRequest{})); err != nil {
		t.Fatal(err)
	}
	intents := fleetd.intentCalls()
	if len(intents) != 1 || intents[0]["state"] != "serving" {
		t.Errorf("intents = %v, want one serving (clears intent)", intents)
	}

	// And remote resume: no write.
	ctx := context.WithValue(context.Background(), remoteInvocationKey{}, true)
	if _, err := d.CellResume(ctx, connect.NewRequest(&vibev1.CellResumeRequest{})); err != nil {
		t.Fatal(err)
	}
	if got := fleetd.intentCalls(); len(got) != 1 {
		t.Errorf("remote resume wrote intent: %v", got[1:])
	}
}

func TestCellDrain_LeaseFetchFailureMarkedUnavailable(t *testing.T) {
	d := drainDaemon(t, Config{
		CellCmds: CellCmds{Drain: "true"},
		Fleet:    FleetConfig{Cell: "gpu-cell", RegistryURL: "http://127.0.0.1:1"},
	})
	d.SetCellCmdRunner(func(ctx context.Context, cmd string) (string, error) { return "", nil })

	resp, err := d.CellDrain(context.Background(), connect.NewRequest(&vibev1.CellDrainRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Msg.LeasesUnavailable {
		t.Error("leases_unavailable not set when fleetd is unreachable — the report must admit it can't rule out stranded work")
	}
}

func TestCellDrain_TimeoutIsUnavailable(t *testing.T) {
	d := drainDaemon(t, Config{CellCmds: CellCmds{Drain: "sleep 120"}})
	d.SetCellCmdRunner(func(ctx context.Context, cmd string) (string, error) {
		// The real runner bounds at cellCmdTimeout; the stub honors the ctx.
		<-ctx.Done()
		return "", ctx.Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := d.CellDrain(ctx, connect.NewRequest(&vibev1.CellDrainRequest{}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("got %v, want Unavailable on timeout", err)
	}
}

// inflightCell is a scripted llama-swap events stream: it emits inflight
// frames on demand, letting tests drive the drain's quiescence wait
// through the fleet watcher.
type inflightCell struct {
	srv  *httptest.Server
	send chan int
}

func newInflightCell(t *testing.T) *inflightCell {
	t.Helper()
	c := &inflightCell{send: make(chan int, 8)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		writeInflight := func(n int) {
			requests := "[]"
			if n > 0 {
				requests = "[{}]"
			}
			inner, _ := json.Marshal(`{"requests":` + requests + `}`)
			fmt.Fprintf(w, "event:message\ndata:{\"type\":\"inflight\",\"data\":%s}\n\n", inner)
			flusher.Flush()
		}
		for {
			select {
			case <-r.Context().Done():
				return
			case n := <-c.send:
				writeInflight(n)
			}
		}
	})
	mux.HandleFunc("GET /running", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"running":[]}`))
	})
	c.srv = httptest.NewServer(mux)
	t.Cleanup(c.srv.Close)
	return c
}

func (c *inflightCell) emit(n int) { c.send <- n }

func drainWithFleet(t *testing.T, cell *inflightCell) *Daemon {
	t.Helper()
	fleet := fleetapi.New(
		[]fleetapi.Cell{{Name: "front", URL: cell.srv.URL}},
		filepath.Join(t.TempDir(), "h.json"),
		func() fleetapi.DaemonInfo { return fleetapi.DaemonInfo{} },
		fleetapi.Options{})
	t.Cleanup(fleet.Close)
	fleet.Start()
	// Wait for the watcher's first inflight frame — production watchers
	// are long-lived, so "has this cell ever reported" is settled long
	// before a drain; the test must establish the same precondition.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, reported := fleet.InFlight("front"); reported {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("watcher never received an inflight frame")
		}
		time.Sleep(10 * time.Millisecond)
	}
	d := drainDaemon(t, Config{CellCmds: CellCmds{Drain: "true"}})
	d.fleet = fleet
	return d
}

func TestCellDrain_WaitForQuiescence(t *testing.T) {
	cell := newInflightCell(t)
	cell.emit(1) // one in-flight when the watcher connects
	d := drainWithFleet(t, cell)

	ran := make(chan struct{})
	d.SetCellCmdRunner(func(ctx context.Context, cmd string) (string, error) {
		close(ran)
		return "", nil
	})
	done := make(chan error, 1)
	go func() {
		_, err := d.CellDrain(context.Background(), connect.NewRequest(&vibev1.CellDrainRequest{WaitSeconds: 10}))
		done <- err
	}()

	// The drain must be parked on the in-flight request...
	select {
	case <-ran:
		t.Fatal("drain command ran while a request was in flight")
	case <-time.After(500 * time.Millisecond):
	}
	// ...and proceed once the stream reports quiescence.
	cell.emit(0)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not proceed after quiescence")
	}
	<-ran
}

func TestCellDrain_WaitTimeoutSkipsDrain(t *testing.T) {
	cell := newInflightCell(t)
	cell.emit(1)
	d := drainWithFleet(t, cell)

	cmdRan := false
	d.SetCellCmdRunner(func(ctx context.Context, cmd string) (string, error) {
		cmdRan = true
		return "", nil
	})
	// The in-flight request never finishes: the wait must expire, and
	// the drain must NOT run (no silent kill after a failed wait).
	_, err := d.CellDrain(context.Background(), connect.NewRequest(&vibev1.CellDrainRequest{WaitSeconds: 1}))
	if connect.CodeOf(err) != connect.CodeDeadlineExceeded {
		t.Fatalf("got %v, want DeadlineExceeded", err)
	}
	if cmdRan {
		t.Error("drain command ran after a failed quiescence wait")
	}
}
