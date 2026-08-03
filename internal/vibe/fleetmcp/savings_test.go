package fleetmcp

// C7b exposure: the fleet_savings tool. It serves the IDENTICAL document
// GET /api/fleet/savings serves, so an agent and the page can never
// disagree about the money — and the caveat travels in the payload,
// because an agent quoting the headline without it is the failure mode
// the whole phase exists to avoid.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/prices"
)

func newSavingsFacade(t *testing.T) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	hosts := &fleetcfg.File{
		Cells: map[string]fleetcfg.Cell{
			"front": {URL: "http://127.0.0.1:1", Class: fleetcfg.ClassAlwaysOn},
			"gpu": {URL: "http://127.0.0.1:1", Class: fleetcfg.ClassAlwaysOn,
				Power:       &fleetcfg.Power{Source: fleetcfg.PowerDeclared, WattsIdle: 100, WattsBusy: 400},
				CapitalCost: 3100, CapitalNote: "example: dual-use GPU, upgrade delta"},
		},
		Pricing: &fleetcfg.Pricing{
			ElectricityPricePerKWh: 0.15,
			Models:                 map[string]fleetcfg.ModelPricing{"qwen3.6-27b": {Twin: "acme/chat-30b"}},
		},
	}
	table := &prices.Table{Schema: prices.SchemaVersion, Snapshots: []prices.Snapshot{{
		EffectiveFrom: "2020-01-01", Base: true,
		Rows: []prices.Row{{Provider: "host-a", Model: "acme/chat-30b", Key: "chat30b", In: 0.2, Out: 0.8, CacheRead: 0.02, Open: true}},
	}}}
	fleet := fleetapi.New(
		[]fleetapi.Cell{{Name: "front", URL: "http://127.0.0.1:1"}, {Name: "gpu", URL: "http://127.0.0.1:1"}},
		dir+"/history.json",
		func() fleetapi.DaemonInfo { return fleetapi.DaemonInfo{} },
		fleetapi.Options{IntentPath: dir + "/intent.json", UsagePath: dir + "/usage.jsonl",
			Timezone: time.UTC, Hosts: hosts, Prices: table},
	)
	t.Cleanup(fleet.Close)
	s := New(fleet, hosts, Options{})
	mux := http.NewServeMux()
	fleet.Register(mux)
	s.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func TestFleetSavingsTool_PricesTheLedgerAndCarriesTheCaveat(t *testing.T) {
	ts := newSavingsFacade(t)
	postAnnounce(t, ts, fleetapi.AnnounceRequest{
		V: fleetapi.AnnounceVersion, Cell: "gpu", Seq: 1,
		Intent: &fleetapi.AnnounceIntent{State: "serving", Since: time.Now().UTC()},
		Models: []fleetapi.AnnounceModel{{ID: "qwen3.6-27b", State: "ready"}},
		Usage: &fleetapi.AnnounceUsage{Epoch: "e1", Models: []fleetapi.AnnounceUsageModel{
			{Model: "qwen3.6-27b", Basis: "chat", Req: 10, InFresh: 1_000_000, InCached: 12_000_000, Out: 500_000},
		}},
	})

	text, isErr := toolText(t, rpc(t, ts, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fleet_savings","arguments":{"window":"30d"}}}`))
	if isErr {
		t.Fatalf("fleet_savings errored: %s", text)
	}
	var rep fleetapi.SavingsReport
	if err := json.Unmarshal([]byte(text), &rep); err != nil {
		t.Fatalf("decode fleet_savings: %v (%s)", err, text)
	}
	if rep.Totals.Gross == nil {
		t.Fatalf("no money in the savings document: %s", text)
	}
	// 1M @ 0.20 + 12M @ 0.02 + 0.5M @ 0.80 = 0.20 + 0.24 + 0.40
	if got := *rep.Totals.Gross; got < 0.8399 || got > 0.8401 {
		t.Errorf("gross = %.4f, want 0.84", got)
	}
	if rep.Caveat == "" || !strings.Contains(rep.Caveat, "upper bound") {
		t.Error("the caveat must ride with the number for agents too")
	}
	if len(rep.Payback) != 1 || rep.Payback[0].Cell != "gpu" {
		t.Errorf("payback = %+v, want the one cell with a capital cost", rep.Payback)
	}

	// The HTTP surface and the tool are the same document.
	resp, err := http.Get(ts.URL + "/api/fleet/savings?window=30d")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var viaHTTP fleetapi.SavingsReport
	if err := json.NewDecoder(resp.Body).Decode(&viaHTTP); err != nil {
		t.Fatal(err)
	}
	if viaHTTP.Totals.Gross == nil || *viaHTTP.Totals.Gross != *rep.Totals.Gross {
		t.Errorf("HTTP gross %v != tool gross %v", viaHTTP.Totals.Gross, rep.Totals.Gross)
	}
}

func TestFleetSavingsTool_RejectsAMalformedWindow(t *testing.T) {
	ts := newSavingsFacade(t)
	text, isErr := toolText(t, rpc(t, ts, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fleet_savings","arguments":{"window":"forever"}}}`))
	if !isErr {
		t.Fatalf("window=forever accepted: %s", text)
	}
}

func TestFleetSavingsTool_IsListed(t *testing.T) {
	ts := newSavingsFacade(t)
	resp := rpc(t, ts, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	raw, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		t.Fatalf("decode tools/list: %v (%s)", err, raw)
	}
	for _, tool := range listed.Tools {
		if tool.Name == "fleet_savings" {
			return
		}
	}
	t.Errorf("fleet_savings missing from tools/list: %s", raw)
}
