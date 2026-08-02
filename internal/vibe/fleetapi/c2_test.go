package fleetapi

// C2 coverage: advisory leases (CRUD, TTL expiry, pre-drain visibility)
// and Wake-on-LAN (packet shape, delivery paths, error cases).

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMagicPacketShape(t *testing.T) {
	mac, err := net.ParseMAC("aa:bb:cc:dd:ee:ff")
	if err != nil {
		t.Fatal(err)
	}
	p := magicPacket(mac)
	if len(p) != 102 {
		t.Fatalf("len = %d, want 102", len(p))
	}
	for i := range 6 {
		if p[i] != 0xFF {
			t.Fatalf("sync byte %d = %x", i, p[i])
		}
	}
	for i := range 16 {
		if !bytes.Equal(p[6+i*6:12+i*6], mac) {
			t.Fatalf("MAC repetition %d wrong: %x", i, p[6+i*6:12+i*6])
		}
	}
}

func TestSendWakePacketDelivery(t *testing.T) {
	ln, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	target := ln.LocalAddr().String()

	s := New([]Cell{{
		Name: "gpu-cell", URL: "http://127.0.0.1:1",
		Wake: &WakeSpec{MAC: "aa:bb:cc:dd:ee:ff", Broadcast: target},
	}}, filepath.Join(t.TempDir(), "h.json"), testDaemonInfo, Options{})
	defer s.Close()

	resp, err := s.SendWake(t.Context(), "gpu-cell")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Sent != "packet" || resp.Target != target {
		t.Errorf("resp = %+v", resp)
	}
	_ = ln.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1024)
	n, _, err := ln.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("no packet received: %v", err)
	}
	if n != 102 || buf[0] != 0xFF || buf[6] != 0xaa {
		t.Errorf("packet = %d bytes %x…", n, buf[:12])
	}
}

func TestSendWakeFallbackCmd(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "woke")
	s := New([]Cell{{
		Name: "gpu-cell", URL: "http://127.0.0.1:1",
		Wake: &WakeSpec{MAC: "aa:bb:cc:dd:ee:ff", Cmd: "touch " + marker},
	}}, filepath.Join(t.TempDir(), "h.json"), testDaemonInfo, Options{})
	defer s.Close()

	resp, err := s.SendWake(t.Context(), "gpu-cell")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Sent != "cmd" {
		t.Errorf("resp = %+v", resp)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("fallback command did not run: %v", err)
	}
}

func TestSendWakeErrors(t *testing.T) {
	s := New([]Cell{
		{Name: "front", URL: "http://127.0.0.1:1"},
	}, filepath.Join(t.TempDir(), "h.json"), testDaemonInfo, Options{})
	defer s.Close()

	if _, err := s.SendWake(t.Context(), "typo"); err == nil || !strings.Contains(err.Error(), "unknown cell") {
		t.Errorf("unknown cell: %v", err)
	}
	if _, err := s.SendWake(t.Context(), "front"); err == nil || !strings.Contains(err.Error(), "no wake") {
		t.Errorf("no wake config: %v", err)
	}
}

func TestWakeHTTPEndpoint(t *testing.T) {
	s := New([]Cell{
		{Name: "front", URL: "http://127.0.0.1:1"},
	}, filepath.Join(t.TempDir(), "h.json"), testDaemonInfo,
		Options{IntentPath: filepath.Join(t.TempDir(), "intent.json")})
	defer s.Close()
	mux := http.NewServeMux()
	s.Register(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Unknown cell → 400; no wake config → 404.
	resp, err := http.Post(ts.URL+"/api/fleet/wake", "application/json", strings.NewReader(`{"cell":"typo"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown cell: HTTP %d, want 400", resp.StatusCode)
	}
	resp, err = http.Post(ts.URL+"/api/fleet/wake", "application/json", strings.NewReader(`{"cell":"front"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("no wake config: HTTP %d, want 404", resp.StatusCode)
	}
}

func leaseClient(t *testing.T, ts *httptest.Server, method, body string) (int, map[string]any) {
	t.Helper()
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
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestLeaseCRUDAndTTL(t *testing.T) {
	cell := newFakeCell(t)
	_, ts, opts := newFleetdServer(t, []Cell{
		{Name: "gpu-cell", URL: cell.srv.URL, Class: "opportunistic"},
	})

	// Take a lease.
	code, out := leaseClient(t, ts, http.MethodPost, `{"cell":"gpu-cell","model":"qwen","holder":"batch-ingest","note":"mid-batch, 2.1M rows left","ttl":"2h"}`)
	if code != http.StatusOK {
		t.Fatalf("POST lease: %d %v", code, out)
	}
	// Visible in fleet state under the cell (the pre-drain view).
	st := getState(t, ts)
	if len(st.Cells[0].Leases) != 1 || st.Cells[0].Leases[0].Holder != "batch-ingest" {
		t.Fatalf("state leases = %+v", st.Cells[0].Leases)
	}
	// And in the GET endpoint.
	resp, err := http.Get(ts.URL + "/api/fleet/leases?cell=gpu-cell")
	if err != nil {
		t.Fatal(err)
	}
	var wrap struct {
		Leases []Lease `json:"leases"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&wrap)
	resp.Body.Close()
	if len(wrap.Leases) != 1 || wrap.Leases[0].Note != "mid-batch, 2.1M rows left" {
		t.Errorf("GET leases = %+v", wrap.Leases)
	}
	// Persisted on disk.
	if _, err := os.Stat(opts.LeasePath); err != nil {
		t.Errorf("lease file not written: %v", err)
	}
	// Refresh (same key, last-write-wins): still one lease.
	code, _ = leaseClient(t, ts, http.MethodPost, `{"cell":"gpu-cell","model":"qwen","holder":"batch-ingest","note":"refreshed","ttl":"2h"}`)
	if code != http.StatusOK {
		t.Fatal("refresh failed")
	}
	st = getState(t, ts)
	if len(st.Cells[0].Leases) != 1 || st.Cells[0].Leases[0].Note != "refreshed" {
		t.Errorf("after refresh: %+v", st.Cells[0].Leases)
	}
	// Delete by key.
	code, _ = leaseClient(t, ts, http.MethodDelete, `{"cell":"gpu-cell","model":"qwen","holder":"batch-ingest"}`)
	if code != http.StatusOK {
		t.Fatalf("DELETE: %d", code)
	}
	st = getState(t, ts)
	if len(st.Cells[0].Leases) != 0 {
		t.Errorf("after delete: %+v", st.Cells[0].Leases)
	}
	// Validation: bad ttl, unknown cell, missing fields.
	code, _ = leaseClient(t, ts, http.MethodPost, `{"cell":"gpu-cell","model":"qwen","holder":"h","ttl":"forever"}`)
	if code != http.StatusBadRequest {
		t.Errorf("bad ttl: %d, want 400", code)
	}
	code, _ = leaseClient(t, ts, http.MethodPost, `{"cell":"typo","model":"qwen","holder":"h","ttl":"2h"}`)
	if code != http.StatusBadRequest {
		t.Errorf("unknown cell: %d, want 400", code)
	}
	code, _ = leaseClient(t, ts, http.MethodPost, `{"cell":"gpu-cell","model":"","holder":"h","ttl":"2h"}`)
	if code != http.StatusBadRequest {
		t.Errorf("missing model: %d, want 400", code)
	}
}

func TestLeaseTTLExpiry(t *testing.T) {
	cell := newFakeCell(t)
	_, ts, _ := newFleetdServer(t, []Cell{
		{Name: "gpu-cell", URL: cell.srv.URL, Class: "opportunistic"},
	})
	// 1ms TTL: expires before the read. A crashed consumer must not
	// haunt the fleet.
	code, _ := leaseClient(t, ts, http.MethodPost, `{"cell":"gpu-cell","model":"qwen","holder":"crashed-batch","ttl":"1ms"}`)
	if code != http.StatusOK {
		t.Fatal("POST failed")
	}
	time.Sleep(10 * time.Millisecond)
	st := getState(t, ts)
	if len(st.Cells[0].Leases) != 0 {
		t.Errorf("expired lease still visible: %+v — a crashed consumer haunts the fleet", st.Cells[0].Leases)
	}
}

func TestLeaseRoutesAbsentWithoutStore(t *testing.T) {
	cell := newFakeCell(t)
	_, ts, _ := newTestServer(t, cell.srv.URL)
	resp, err := http.Get(ts.URL + "/api/fleet/leases")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET leases without store: HTTP %d, want 404", resp.StatusCode)
	}
	code, _ := leaseClient(t, ts, http.MethodPost, `{"cell":"front","model":"m","holder":"h","ttl":"2h"}`)
	if code != http.StatusNotFound {
		t.Errorf("POST lease without store: %d, want 404", code)
	}
}
