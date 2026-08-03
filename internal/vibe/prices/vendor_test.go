package prices

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The vendoring tool is tested entirely against httptest servers. It is
// the only way it CAN be tested — CI has no network, and the tool exists
// precisely because the runtime must not fetch anything.

const modelsDevFixture = `{
  "host-a": {"id": "host-a", "models": {
    "acme/chat-30b": {"id": "acme/chat-30b", "name": "Acme Chat 30B", "open_weights": true,
                      "cost": {"input": 0.2, "output": 0.8, "cache_read": 0.05}},
    "acme/embed-1":  {"id": "acme/embed-1", "name": "Acme Embed 1", "open_weights": true,
                      "cost": {"input": 0.02, "output": 0}}
  }},
  "host-b": {"id": "host-b", "models": {
    "chat-30b": {"id": "chat-30b", "name": "Acme Chat 30B", "open_weights": true,
                 "cost": {"input": 0.3, "output": 0.9}},
    "priceless": {"id": "priceless", "name": "No Price Model", "open_weights": true,
                  "cost": {"input": 0, "output": 0}}
  }},
  "openrouter": {"id": "openrouter", "models": {
    "acme/chat-30b": {"id": "acme/chat-30b", "name": "Acme Chat 30B", "open_weights": true,
                      "cost": {"input": 0.25, "output": 0.85}}
  }}
}`

const liteLLMFixture = `{
  "sample_spec": {"litellm_provider": "x", "mode": "chat"},
  "acme/embed-1": {"litellm_provider": "host-a", "mode": "embedding",
                   "input_cost_per_token": 2e-08, "output_cost_per_token": 0},
  "acme/rerank-1": {"litellm_provider": "host-a", "mode": "rerank",
                    "input_cost_per_query": 0.002, "input_cost_per_token": 0, "output_cost_per_token": 0},
  "acme/chat-30b": {"litellm_provider": "host-a", "mode": "chat",
                    "input_cost_per_token": 2e-07, "output_cost_per_token": 8e-07}
}`

func vendorServers(t *testing.T, md, ll string) (string, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models.json":
			_, _ = w.Write([]byte(md))
		case "/litellm.json":
			_, _ = w.Write([]byte(ll))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/models.json", srv.URL + "/litellm.json"
}

func testVendorConfig(t *testing.T, md, ll string) VendorConfig {
	t.Helper()
	mdURL, llURL := vendorServers(t, md, ll)
	return VendorConfig{
		ModelsDevURL: mdURL,
		LiteLLMURL:   llURL,
		// "-" skips the commit lookup: the fixtures have no GitHub.
		ModelsDevCommitURL: "-",
		LiteLLMCommitURL:   "-",
		EffectiveFrom:      "2026-08-01",
		Now:                func() time.Time { return time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC) },
	}
}

func TestVendor_NormalizesPrunesAndEnriches(t *testing.T) {
	cfg := testVendorConfig(t, modelsDevFixture, liteLLMFixture)
	tbl, rep, err := cfg.Vendor(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Base {
		t.Error("the first vendoring must produce a base snapshot")
	}
	res := tbl.At("2026-08-02")

	// The same model at two hosts joins on the normalized name, which is
	// what makes a median across hosts possible at all.
	hosts := res.Hosts("Acme Chat 30B", true)
	if len(hosts) != 2 {
		t.Fatalf("chat hosts = %d, want 2 (host-a + host-b; openrouter excluded)", len(hosts))
	}
	for _, h := range hosts {
		if h.Provider == "openrouter" {
			t.Error("openrouter rows must never be vendored (redistribution is not permitted)")
		}
	}
	if rep.Excluded["openrouter"] == 0 {
		t.Error("the report must say what was excluded and why")
	}

	// A row with no price at all is dropped: it is not a free model, it
	// is a model nobody recorded a price for, and keeping it would give
	// the median a phantom $0 host.
	if n := len(res.Hosts("No Price Model", true)); n != 0 {
		t.Errorf("priceless rows vendored = %d, want 0", n)
	}

	// LiteLLM's two fields models.dev lacks.
	embed := res.Hosts("acme/embed-1", true)
	if len(embed) != 1 || embed[0].Mode != ModeEmbedding {
		t.Fatalf("embedding mode not enriched from LiteLLM: %+v", embed)
	}
	rerank := res.Hosts("acme/rerank-1", false)
	if len(rerank) != 1 || rerank[0].Mode != ModeRerank || rerank[0].PerQuery != 0.002 {
		t.Fatalf("rerank row not carried from LiteLLM: %+v", rerank)
	}

	// Per-token upstream figures become per-MTok.
	if embed[0].In != 0.02 {
		t.Errorf("embedding input rate = %g, want 0.02 per MTok", embed[0].In)
	}
}

func TestVendor_FailsLoudlyOnCrossSourceDisagreement(t *testing.T) {
	// LiteLLM says this host charges 100x what models.dev says — the
	// units error this check exists for.
	bad := strings.Replace(liteLLMFixture, `"input_cost_per_token": 2e-07`, `"input_cost_per_token": 2e-05`, 1)
	cfg := testVendorConfig(t, modelsDevFixture, bad)
	_, _, err := cfg.Vendor(context.Background(), nil)
	if err == nil {
		t.Fatal("vendoring succeeded despite a 100x cross-source disagreement")
	}
	var de *DisagreementError
	if !errors.As(err, &de) {
		t.Fatalf("error = %T (%v), want *DisagreementError", err, err)
	}
	if len(de.Conflicts) == 0 || !strings.Contains(err.Error(), "acme/chat-30b") {
		t.Errorf("the failure must name the offenders: %v", err)
	}

	// With a reviewed count the run proceeds, and the conflicting row is
	// DROPPED rather than resolved by picking a side.
	cfg.MaxDisagreements = len(de.Conflicts)
	tbl, rep, err := cfg.Vendor(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Disagreements) != len(de.Conflicts) {
		t.Errorf("report disagreements = %d, want %d", len(rep.Disagreements), len(de.Conflicts))
	}
	for _, h := range tbl.At("2026-08-02").Hosts("Acme Chat 30B", true) {
		if h.Provider == "host-a" {
			t.Error("a row the sources disagree about must be dropped, not vendored")
		}
	}
	if len(tbl.Snapshots[0].Disagreements) == 0 {
		t.Error("the artifact must carry the disagreements it dropped (an auditable trail, not a silent drop)")
	}
}

func TestVendor_AppendsAnOverlayNotAWholeTable(t *testing.T) {
	cfg := testVendorConfig(t, modelsDevFixture, liteLLMFixture)
	base, _, err := cfg.Vendor(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}

	// One host cuts its price, another disappears.
	cut := strings.Replace(modelsDevFixture, `"input": 0.3, "output": 0.9`, `"input": 0.1, "output": 0.3`, 1)
	cfg2 := testVendorConfig(t, cut, liteLLMFixture)
	cfg2.EffectiveFrom = "2026-09-01"
	next, rep, err := cfg2.Vendor(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Snapshots) != 2 {
		t.Fatalf("snapshots = %d, want 2", len(next.Snapshots))
	}
	overlay := next.Snapshots[1]
	if overlay.Base {
		t.Error("the second snapshot must be an overlay, not a base")
	}
	if len(overlay.Rows) != 1 {
		t.Errorf("overlay rows = %d, want 1 (only the changed row)", len(overlay.Rows))
	}
	if len(rep.Changed) != 1 {
		t.Errorf("report changed = %v, want exactly the one price cut", rep.Changed)
	}

	// The old day keeps the old price. This is the whole point of dating
	// snapshots.
	before, _ := PriceAcross(next.At("2026-08-15").Hosts("Acme Chat 30B", true), Counts{Basis: BasisChat, InFresh: 1_000_000})
	after, _ := PriceAcross(next.At("2026-09-15").Hosts("Acme Chat 30B", true), Counts{Basis: BasisChat, InFresh: 1_000_000})
	if before.Median <= after.Median {
		t.Errorf("median before the cut (%.4f) must exceed the median after (%.4f)", before.Median, after.Median)
	}
}

func TestVendor_RejectsABackdatedSnapshot(t *testing.T) {
	cfg := testVendorConfig(t, modelsDevFixture, liteLLMFixture)
	base, _, err := cfg.Vendor(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg2 := testVendorConfig(t, modelsDevFixture, liteLLMFixture)
	cfg2.EffectiveFrom = "2026-07-01"
	if _, _, err := cfg2.Vendor(context.Background(), base); err == nil {
		t.Error("accepted a snapshot older than the newest one; snapshots are append-only")
	}
}

func TestVendor_IsDeterministic(t *testing.T) {
	cfg := testVendorConfig(t, modelsDevFixture, liteLLMFixture)
	var last []byte
	for i := 0; i < 5; i++ {
		tbl, _, err := cfg.Vendor(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		data, err := Encode(tbl)
		if err != nil {
			t.Fatal(err)
		}
		if last != nil && !bytes.Equal(last, data) {
			t.Fatal("two vendoring runs over the same input produced different bytes; the artifact's diffs would be meaningless")
		}
		last = data
	}
}

func TestVendor_ExampleTableIsReplacedNotAppendedTo(t *testing.T) {
	example := &Table{Schema: SchemaVersion, Example: true, Notice: exampleNotice,
		Snapshots: []Snapshot{{EffectiveFrom: "2020-01-01", Base: true, Rows: []Row{
			{Provider: "example-host", Model: "example", Key: "example", In: 1, Out: 2, Open: true},
		}}}}
	cfg := testVendorConfig(t, modelsDevFixture, liteLLMFixture)
	tbl, rep, err := cfg.Vendor(context.Background(), example)
	if err != nil {
		t.Fatal(err)
	}
	if tbl.Example {
		t.Error("a vendored table must not stay marked as example data")
	}
	if !rep.Base || len(tbl.Snapshots) != 1 {
		t.Errorf("snapshots = %d (base=%v), want one fresh base — synthetic rows must never price a real day",
			len(tbl.Snapshots), rep.Base)
	}
	if len(tbl.At("2026-08-02").Hosts("example", false)) != 0 {
		t.Error("synthetic example rows survived into a real table")
	}
}

func TestEncodeRoundTripsAndKeepsOneRowPerLine(t *testing.T) {
	cfg := testVendorConfig(t, modelsDevFixture, liteLLMFixture)
	tbl, _, err := cfg.Vendor(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := Encode(tbl)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(data)
	if err != nil {
		t.Fatalf("encoded artifact does not parse: %v", err)
	}
	if len(back.Snapshots[0].Rows) != len(tbl.Snapshots[0].Rows) {
		t.Errorf("round trip lost rows: %d → %d", len(tbl.Snapshots[0].Rows), len(back.Snapshots[0].Rows))
	}
	rowLines := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), `{"p":`) {
			rowLines++
		}
	}
	if rowLines != len(tbl.Snapshots[0].Rows) {
		t.Errorf("rows on their own lines = %d, want %d (a one-line artifact makes every future diff unreadable)",
			rowLines, len(tbl.Snapshots[0].Rows))
	}
}

func TestVendor_UpstreamFailureIsNotAPartialTable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream down", http.StatusBadGateway)
	}))
	defer srv.Close()
	cfg := VendorConfig{ModelsDevURL: srv.URL, LiteLLMURL: srv.URL, ModelsDevCommitURL: "-", LiteLLMCommitURL: "-"}
	if _, _, err := cfg.Vendor(context.Background(), nil); err == nil {
		t.Error("a failed upstream fetch must fail the run, not vendor half a table")
	}
}
