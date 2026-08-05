package daemon_test

// C13, the half that needs the daemon: the outbound credential probe is
// the ONE thing in the doctor path that leaves the box, and this pins
// what it sends (a read-only Status RPC, with the credential the
// actuation verbs resolve) and what it concludes from each answer.
// Also the route's own boundary, because C12's rule is that a route is
// exactly as safe as its declaration.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/gallowaysoftware/vibe/internal/vibe/daemon"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
	vibev1 "github.com/gallowaysoftware/vibe/proto/vibe/v1"
)

// cellDaemonFake records what the doctor's outbound probe sends it, and
// answers with the status the test asks for.
type cellDaemonFake struct {
	mu     sync.Mutex
	paths  []string
	auth   []string
	status int
}

func (c *cellDaemonFake) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.paths = append(c.paths, r.URL.Path)
		c.auth = append(c.auth, r.Header.Get("Authorization"))
		st := c.status
		c.mu.Unlock()
		if st != 0 && st != http.StatusOK {
			http.Error(w, "no", st)
			return
		}
		// A Connect unary response: the proto codec the client asks for,
		// unframed, which is what the Connect protocol specifies for unary.
		body, err := proto.Marshal(&vibev1.StatusResponse{Status: &vibev1.Status{Running: false}})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/proto")
		_, _ = w.Write(body)
	}
}

func (c *cellDaemonFake) seen() ([]string, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.paths...), append([]string(nil), c.auth...)
}

// TestDoctor_OutboundProbeSendsOnlyAReadOnlyStatusRPC. The doctor path
// must be safe to run mid-incident, so the one call it makes off-box is
// pinned by PATH, not by intent: any future check that reached for a
// verb here would change this list.
func TestDoctor_OutboundProbeSendsOnlyAReadOnlyStatusRPC(t *testing.T) {
	cfgHome, _, _ := setupXDG(t)
	front := fleetCellFake(t, `{"object":"list","data":[]}`)
	gpu := fleetCellFake(t, `{"object":"list","data":[]}`)

	cell := &cellDaemonFake{}
	cellSrv := httptest.NewServer(cell.handler())
	t.Cleanup(cellSrv.Close)

	tokPath := filepath.Join(t.TempDir(), "gpu.token")
	if err := os.WriteFile(tokPath, []byte("gpu-cell-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeFleetHosts(t, cfgHome, fmt.Sprintf(`
cells:
  front:    { url: "%s", class: always_on }
  gpu-cell: { url: "%s", class: opportunistic, daemon_url: "%s", token_file: "%s" }
`, front.URL, gpu.URL, cellSrv.URL, tokPath))

	httpAddr, token := startFleetd(t)

	rep := fetchDoctor(t, httpAddr, token)
	seenPaths, seenAuth := cell.seen()
	if len(seenPaths) == 0 {
		t.Fatal("the outbound credential probe never called the cell daemon")
	}
	for _, p := range seenPaths {
		if !strings.HasSuffix(p, "/Status") {
			t.Errorf("doctor called %s on the cell daemon; the only permitted call is the read-only Status RPC", p)
		}
	}
	for _, a := range seenAuth {
		if a != "Bearer gpu-cell-token" {
			t.Errorf("Authorization = %q, want the per-cell token_file (the credential the verbs resolve)", a)
		}
	}
	got := doctorCheck(t, rep, "auth.outbound", "gpu-cell")
	if got.Level != fleetapi.LevelOK {
		t.Errorf("accepted credential → %s (%s)", got.Level, got.Detail)
	}
	if !strings.Contains(got.Detail, "token_file") {
		t.Errorf("detail = %q, want the credential source named", got.Detail)
	}
	if strings.Contains(got.Detail, "gpu-cell-token") {
		t.Errorf("detail = %q leaks the token VALUE", got.Detail)
	}
}

// TestDoctor_OutboundProbeReportsA401AsARefusal: a 401 is the far side
// answering. It must not read as "the box might be off".
func TestDoctor_OutboundProbeReportsA401AsARefusal(t *testing.T) {
	cfgHome, _, _ := setupXDG(t)
	front := fleetCellFake(t, `{"object":"list","data":[]}`)
	gpu := fleetCellFake(t, `{"object":"list","data":[]}`)

	cell := &cellDaemonFake{status: http.StatusUnauthorized}
	cellSrv := httptest.NewServer(cell.handler())
	t.Cleanup(cellSrv.Close)

	tokPath := filepath.Join(t.TempDir(), "gpu.token")
	if err := os.WriteFile(tokPath, []byte("stale-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeFleetHosts(t, cfgHome, fmt.Sprintf(`
cells:
  front:    { url: "%s", class: always_on }
  gpu-cell: { url: "%s", class: opportunistic, daemon_url: "%s", token_file: "%s" }
`, front.URL, gpu.URL, cellSrv.URL, tokPath))
	httpAddr, token := startFleetd(t)

	got := doctorCheck(t, fetchDoctor(t, httpAddr, token), "auth.outbound", "gpu-cell")
	if got.Level != fleetapi.LevelFail {
		t.Fatalf("401 from the cell → %s, want fail", got.Level)
	}
	if strings.Contains(got.Detail, "stale-token") {
		t.Errorf("detail = %q leaks the token VALUE", got.Detail)
	}
}

// TestDoctorRoute_RefusesTheGuestToken: C12's grant is state and events
// only, and the doctor report is the least guest-shaped document in the
// fleet — config paths, credential sources, disk figures, cert expiries.
func TestDoctorRoute_RefusesTheGuestToken(t *testing.T) {
	cfgHome, stateHome, _ := setupXDG(t)
	front := fleetCellFake(t, `{"object":"list","data":[]}`)
	writeFleetHosts(t, cfgHome, fmt.Sprintf("cells:\n  front: { url: \"%s\", class: always_on }\n", front.URL))

	guestPath := filepath.Join(stateHome, "vibe", "guest-token")
	if err := os.WriteFile(guestPath, []byte("guest-tok-0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	httpAddr, token := startFleetdWithConfig(t, func(cfg *daemon.Config) {
		cfg.Fleet.GuestTokenFile = guestPath
	})

	for _, tc := range []struct {
		tok  string
		want int
	}{
		{"guest-tok-0123456789abcdef", http.StatusUnauthorized},
		{"", http.StatusUnauthorized},
		{token, http.StatusOK},
	} {
		r, _ := http.NewRequest(http.MethodGet, "http://"+httpAddr+"/api/fleet/doctor", nil)
		if tc.tok != "" {
			r.Header.Set("Authorization", "Bearer "+tc.tok)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("GET /api/fleet/doctor with %q: HTTP %d, want %d", tc.tok, resp.StatusCode, tc.want)
		}
	}
}

func fetchDoctor(t *testing.T, httpAddr, token string) fleetapi.DoctorReport {
	t.Helper()
	r, _ := http.NewRequest(http.MethodGet, "http://"+httpAddr+"/api/fleet/doctor", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("doctor: HTTP %d: %s", resp.StatusCode, body)
	}
	var rep fleetapi.DoctorReport
	if err := json.Unmarshal(body, &rep); err != nil {
		t.Fatalf("decode doctor report: %v\n%s", err, body)
	}
	if len(rep.Checks) == 0 {
		t.Fatal("empty doctor report")
	}
	return rep
}

func doctorCheck(t *testing.T, rep fleetapi.DoctorReport, id, cell string) fleetapi.DoctorCheck {
	t.Helper()
	for _, c := range rep.Checks {
		if c.ID == id && c.Cell == cell {
			return c
		}
	}
	var have []string
	for _, c := range rep.Checks {
		have = append(have, c.ID+"/"+c.Cell)
	}
	t.Fatalf("no check %s/%s; have %v", id, cell, have)
	return fleetapi.DoctorCheck{}
}

func writeFleetHosts(t *testing.T, cfgHome, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(cfgHome, "vibe", "hosts.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func startFleetd(t *testing.T) (string, string) {
	t.Helper()
	return startFleetdWithConfig(t, nil)
}

func startFleetdWithConfig(t *testing.T, tweak func(*daemon.Config)) (string, string) {
	t.Helper()
	httpAddr := fmt.Sprintf("127.0.0.1:%d", pickFreePort(t))
	cfg := daemon.Config{
		ProxyPort:     pickFreePort(t),
		DisableProxy:  true,
		HTTPAddr:      httpAddr,
		LlamaBinary:   fakeBinary,
		FleetRegistry: true,
	}
	if tweak != nil {
		tweak(&cfg)
	}
	d := daemon.New(cfg)
	_, _ = startDaemon(t, d)
	tokenBytes, err := os.ReadFile(paths.TokenFile())
	if err != nil {
		t.Fatal(err)
	}
	return httpAddr, strings.TrimSpace(string(tokenBytes))
}
