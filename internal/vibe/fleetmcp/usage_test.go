package fleetmcp

// C7a exposure: the fleet_usage tool. The phase ships no UI, so this
// tool IS the payoff — a phase with no visible payoff is the classic
// phase that never gets finished.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
)

func newUsageFacade(t *testing.T) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	cells := map[string]fleetcfg.Cell{
		"front": {URL: "http://127.0.0.1:1", Class: fleetcfg.ClassAlwaysOn},
		"gpu":   {URL: "http://127.0.0.1:1", Class: fleetcfg.ClassAlwaysOn},
	}
	fleet := fleetapi.New(
		[]fleetapi.Cell{{Name: "front", URL: "http://127.0.0.1:1"}, {Name: "gpu", URL: "http://127.0.0.1:1"}},
		dir+"/history.json",
		func() fleetapi.DaemonInfo { return fleetapi.DaemonInfo{} },
		fleetapi.Options{IntentPath: dir + "/intent.json", UsagePath: dir + "/usage.jsonl", Timezone: time.UTC},
	)
	t.Cleanup(fleet.Close)
	s := New(fleet, &fleetcfg.File{Cells: cells}, Options{})
	mux := http.NewServeMux()
	// Both surfaces on one mux, exactly as the daemon mounts them, so the
	// test drives the real announce endpoint rather than an in-process
	// shortcut.
	fleet.Register(mux)
	s.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func postAnnounce(t *testing.T, ts *httptest.Server, req fleetapi.AnnounceRequest) {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal announce: %v", err)
	}
	resp, err := http.Post(ts.URL+"/api/fleet/announce", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("POST announce: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST announce: HTTP %d", resp.StatusCode)
	}
}

func TestFleetUsageTool_ReturnsRawCountsFromTheLedger(t *testing.T) {
	ts := newUsageFacade(t)

	postAnnounce(t, ts, fleetapi.AnnounceRequest{
		V: fleetapi.AnnounceVersion, Cell: "gpu", Seq: 1,
		Intent: &fleetapi.AnnounceIntent{State: "serving", Since: time.Now().UTC()},
		Models: []fleetapi.AnnounceModel{{ID: "qwen3.6-27b", State: "ready"}},
		Usage: &fleetapi.AnnounceUsage{Epoch: "e1", Models: []fleetapi.AnnounceUsageModel{
			{Model: "qwen3.6-27b", Basis: "chat", Req: 7, InFresh: 900, InCached: 41000, Out: 2200, PokeReq: 96, UnmeasuredReq: 4, ErrReq: 1},
		}},
	})

	text, isErr := toolText(t, rpc(t, ts, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fleet_usage","arguments":{}}}`))
	if isErr {
		t.Fatalf("fleet_usage errored: %s", text)
	}
	var rep fleetapi.UsageReport
	if err := json.Unmarshal([]byte(text), &rep); err != nil {
		t.Fatalf("decode fleet_usage: %v (%s)", err, text)
	}
	if len(rep.Buckets) == 0 {
		t.Fatalf("fleet_usage returned no buckets: %s", text)
	}
	var chat *fleetapi.UsageBucket
	for i := range rep.Buckets {
		if rep.Buckets[i].Basis == "chat" {
			chat = &rep.Buckets[i]
		}
	}
	if chat == nil {
		t.Fatalf("no chat-basis bucket in %s", text)
	}
	if chat.Cell != "gpu" || chat.Model != "qwen3.6-27b" {
		t.Errorf("bucket attribution = %s/%s, want gpu/qwen3.6-27b", chat.Cell, chat.Model)
	}
	if chat.InFresh != 900 || chat.InCached != 41000 || chat.Out != 2200 {
		t.Errorf("counts = %+v", chat)
	}
	if chat.PokeReq != 96 || chat.Req != 7 {
		t.Errorf("self-traffic must stay out of the requests figure: req=%d poke_req=%d", chat.Req, chat.PokeReq)
	}
	for _, forbidden := range []string{"usd", "cost", "price", "dollar", "saving"} {
		if strings.Contains(strings.ToLower(text), forbidden) {
			t.Errorf("fleet_usage output mentions %q; C7a returns tokens, C7b prices them", forbidden)
		}
	}
}

func TestFleetUsageTool_RejectsANegativeWindow(t *testing.T) {
	ts := newUsageFacade(t)
	text, isErr := toolText(t, rpc(t, ts, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fleet_usage","arguments":{"days":-1}}}`))
	if !isErr {
		t.Fatalf("days=-1 accepted: %s", text)
	}
}
