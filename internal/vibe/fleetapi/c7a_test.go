package fleetapi

// C7a fleetd-side coverage: the cumulative delta/epoch rules, the
// structural front skip (no double count), explicit-Location day
// bucketing across midnight and both DST discontinuities, and
// flush/replay idempotency across a restart.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
)

func toronto(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Toronto")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	return loc
}

func newLedger(t *testing.T, loc *time.Location) (*usageLedger, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	return newUsageLedger(path, loc), path
}

func usageOf(model string, req, inFresh, inCached, out int64) AnnounceUsageModel {
	return AnnounceUsageModel{Model: model, Basis: "chat", Req: req, InFresh: inFresh, InCached: inCached, Out: out}
}

func bucketFor(l *usageLedger, k usageKey) UsageBucket {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b := l.buckets[k]; b != nil {
		return *b
	}
	return UsageBucket{}
}

func TestUsageFold_SameEpochAddsOnlyTheDelta(t *testing.T) {
	loc := toronto(t)
	l, _ := newLedger(t, loc)
	at := time.Date(2026, 8, 3, 14, 0, 0, 0, loc)

	l.fold("gpu", &AnnounceUsage{Epoch: "e1", Models: []AnnounceUsageModel{usageOf("qwen", 10, 1000, 5000, 400)}}, at)
	l.fold("gpu", &AnnounceUsage{Epoch: "e1", Models: []AnnounceUsageModel{usageOf("qwen", 13, 1300, 9000, 470)}}, at.Add(time.Minute))

	got := bucketFor(l, usageKey{Day: "2026-08-03", Cell: "gpu", Model: "qwen", Basis: "chat", Epoch: "e1"})
	if got.Req != 13 || got.InFresh != 1300 || got.InCached != 9000 || got.Out != 470 {
		t.Fatalf("bucket = %+v, want the second announce's cumulative totals exactly once", got)
	}
	if got.LastTotal == nil || got.LastTotal.InFresh != 1300 {
		t.Fatalf("last_total not carried on the bucket line: %+v", got.LastTotal)
	}
}

func TestUsageFold_SameEpochLowerTotalIsTreatedAsAReset(t *testing.T) {
	loc := toronto(t)
	l, _ := newLedger(t, loc)
	at := time.Date(2026, 8, 3, 14, 0, 0, 0, loc)

	l.fold("gpu", &AnnounceUsage{Epoch: "e1", Models: []AnnounceUsageModel{usageOf("qwen", 10, 1000, 0, 400)}}, at)
	// Totals went backwards without an epoch change: add the whole new
	// total rather than a negative delta.
	l.fold("gpu", &AnnounceUsage{Epoch: "e1", Models: []AnnounceUsageModel{usageOf("qwen", 2, 200, 0, 80)}}, at.Add(time.Minute))

	got := bucketFor(l, usageKey{Day: "2026-08-03", Cell: "gpu", Model: "qwen", Basis: "chat", Epoch: "e1"})
	if got.Req != 12 || got.InFresh != 1200 || got.Out != 480 {
		t.Fatalf("bucket = %+v, want 10+2 / 1000+200 / 400+80", got)
	}
}

func TestUsageFold_NewEpochStartsANewRowAndKeepsTheOld(t *testing.T) {
	loc := toronto(t)
	l, _ := newLedger(t, loc)
	at := time.Date(2026, 8, 3, 14, 0, 0, 0, loc)

	l.fold("gpu", &AnnounceUsage{Epoch: "e1", Models: []AnnounceUsageModel{usageOf("qwen", 10, 1000, 0, 400)}}, at)
	l.fold("gpu", &AnnounceUsage{Epoch: "e2", Models: []AnnounceUsageModel{usageOf("qwen", 3, 300, 0, 90)}}, at.Add(time.Minute))

	old := bucketFor(l, usageKey{Day: "2026-08-03", Cell: "gpu", Model: "qwen", Basis: "chat", Epoch: "e1"})
	fresh := bucketFor(l, usageKey{Day: "2026-08-03", Cell: "gpu", Model: "qwen", Basis: "chat", Epoch: "e2"})
	if old.Req != 10 || old.InFresh != 1000 {
		t.Errorf("old epoch row was disturbed: %+v", old)
	}
	if fresh.Req != 3 || fresh.InFresh != 300 {
		t.Errorf("new epoch row = %+v, want the new epoch's totals as the opening balance", fresh)
	}
}

// The no-double-count rule is a WHITELIST: the front sees every request
// a cell also sees, so its announce must be skipped by NAME, not
// filtered out by some property that could change.
func TestUsageFold_FrontCellIsSkippedStructurallyByName(t *testing.T) {
	loc := toronto(t)
	l, _ := newLedger(t, loc)
	at := time.Date(2026, 8, 3, 14, 0, 0, 0, loc)

	same := &AnnounceUsage{Epoch: "e1", Models: []AnnounceUsageModel{usageOf("qwen", 5, 500, 0, 100)}}
	l.fold("front", same, at)
	l.fold("gpu", same, at)

	l.mu.Lock()
	n := len(l.buckets)
	l.mu.Unlock()
	if n != 1 {
		t.Fatalf("ledger has %d buckets, want 1 (the front's identical announce must not fold)", n)
	}
	if got := bucketFor(l, usageKey{Day: "2026-08-03", Cell: "front", Model: "qwen", Basis: "chat", Epoch: "e1"}); got.Req != 0 {
		t.Fatalf("a front bucket exists: %+v", got)
	}
	if got := bucketFor(l, usageKey{Day: "2026-08-03", Cell: "gpu", Model: "qwen", Basis: "chat", Epoch: "e1"}); got.Req != 5 {
		t.Fatalf("cell bucket = %+v, want exactly one copy", got)
	}
	// And the skip is spelled with the registry's own constant, so
	// renaming the front cell can't quietly re-enable double counting.
	l2, _ := newLedger(t, loc)
	l2.fold(fleetcfg.FrontCell, same, at)
	l2.foldResidency(fleetcfg.FrontCell, []AnnounceModel{{ID: "qwen", State: "ready"}}, 15, at)
	l2.foldResidency(fleetcfg.FrontCell, []AnnounceModel{{ID: "qwen", State: "ready"}}, 15, at.Add(time.Minute))
	l2.mu.Lock()
	n2 := len(l2.buckets)
	l2.mu.Unlock()
	if n2 != 0 {
		t.Fatalf("fleetcfg.FrontCell announce folded: %d buckets", n2)
	}
}

// A session that spans local midnight must land in two buckets split at
// the LOCAL boundary. Truncate(24*time.Hour) would put the split at
// 19:00 local (UTC midnight) with no error of any kind.
func TestUsageDayKey_SplitsAtLocalMidnightNotUTCMidnight(t *testing.T) {
	loc := toronto(t)
	before := time.Date(2026, 8, 3, 23, 45, 0, 0, loc)
	after := time.Date(2026, 8, 4, 0, 15, 0, 0, loc)

	if got := dayKey(before, loc); got != "2026-08-03" {
		t.Errorf("23:45 local → %q, want 2026-08-03", got)
	}
	if got := dayKey(after, loc); got != "2026-08-04" {
		t.Errorf("00:15 local → %q, want 2026-08-04", got)
	}
	// 21:00 local on Aug 3 is 01:00 UTC on Aug 4 — the case a UTC-based
	// bucket gets wrong.
	evening := time.Date(2026, 8, 3, 21, 0, 0, 0, loc)
	if got := dayKey(evening, loc); got != "2026-08-03" {
		t.Errorf("21:00 local (01:00Z next day) → %q, want 2026-08-03", got)
	}
	if got := dayKey(evening, time.UTC); got != "2026-08-04" {
		t.Errorf("the same instant in UTC → %q, want 2026-08-04 (proving the Location is what decides)", got)
	}
}

func TestUsageDayKey_HandlesBothDSTDiscontinuities(t *testing.T) {
	loc := toronto(t)
	// Spring forward: 2026-03-08 is 23 h long; 02:30 local does not exist
	// and normalizes to 03:30 the same day.
	spring := time.Date(2026, 3, 8, 2, 30, 0, 0, loc)
	if got := dayKey(spring, loc); got != "2026-03-08" {
		t.Errorf("spring-forward gap minute → %q, want 2026-03-08", got)
	}
	if got := dayKey(time.Date(2026, 3, 8, 23, 59, 0, 0, loc), loc); got != "2026-03-08" {
		t.Errorf("end of the 23 h day → %q, want 2026-03-08", got)
	}
	// Fall back: 2026-11-01 is 25 h long and 01:30 local happens twice.
	// Both instants belong to that day.
	firstOneThirty := time.Date(2026, 11, 1, 1, 30, 0, 0, loc)
	secondOneThirty := firstOneThirty.Add(time.Hour)
	if got := dayKey(firstOneThirty, loc); got != "2026-11-01" {
		t.Errorf("first 01:30 → %q, want 2026-11-01", got)
	}
	if got := dayKey(secondOneThirty, loc); got != "2026-11-01" {
		t.Errorf("repeated 01:30 → %q, want 2026-11-01", got)
	}
	if got := dayKey(time.Date(2026, 11, 1, 23, 59, 0, 0, loc), loc); got != "2026-11-01" {
		t.Errorf("end of the 25 h day → %q, want 2026-11-01", got)
	}
}

// The cursor is keyed without the day, so the first fold after local
// midnight bills the delta — not the cell's whole lifetime again.
func TestUsageFold_DayRolloverBillsTheDeltaNotTheLifetime(t *testing.T) {
	loc := toronto(t)
	l, _ := newLedger(t, loc)

	l.fold("gpu", &AnnounceUsage{Epoch: "e1", Models: []AnnounceUsageModel{usageOf("qwen", 100, 100000, 0, 20000)}},
		time.Date(2026, 8, 3, 23, 59, 0, 0, loc))
	l.fold("gpu", &AnnounceUsage{Epoch: "e1", Models: []AnnounceUsageModel{usageOf("qwen", 101, 100500, 0, 20100)}},
		time.Date(2026, 8, 4, 0, 1, 0, 0, loc))

	d1 := bucketFor(l, usageKey{Day: "2026-08-03", Cell: "gpu", Model: "qwen", Basis: "chat", Epoch: "e1"})
	d2 := bucketFor(l, usageKey{Day: "2026-08-04", Cell: "gpu", Model: "qwen", Basis: "chat", Epoch: "e1"})
	if d1.Req != 100 || d1.InFresh != 100000 {
		t.Fatalf("day 1 = %+v", d1)
	}
	if d2.Req != 1 || d2.InFresh != 500 || d2.Out != 100 {
		t.Fatalf("day 2 = %+v, want the delta only", d2)
	}
}

func TestUsageLedger_FlushAndReplayAreIdempotent(t *testing.T) {
	loc := toronto(t)
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	at := time.Date(2026, 8, 3, 14, 0, 0, 0, loc)

	l1 := newUsageLedger(path, loc)
	l1.fold("gpu", &AnnounceUsage{Epoch: "e1", Models: []AnnounceUsageModel{usageOf("qwen", 10, 1000, 200, 400)}}, at)
	if err := l1.flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// fleetd restarts and the cell re-announces the SAME cumulative
	// totals: no bucket may move.
	l2 := newUsageLedger(path, loc)
	before := bucketFor(l2, usageKey{Day: "2026-08-03", Cell: "gpu", Model: "qwen", Basis: "chat", Epoch: "e1"})
	if before.Req != 10 || before.InFresh != 1000 {
		t.Fatalf("replay lost the bucket: %+v", before)
	}
	l2.fold("gpu", &AnnounceUsage{Epoch: "e1", Models: []AnnounceUsageModel{usageOf("qwen", 10, 1000, 200, 400)}}, at.Add(time.Hour))
	after := bucketFor(l2, usageKey{Day: "2026-08-03", Cell: "gpu", Model: "qwen", Basis: "chat", Epoch: "e1"})
	if after.Req != 10 || after.InFresh != 1000 {
		t.Fatalf("replayed announce moved the bucket: %+v", after)
	}

	// A kill between fold and flush loses nothing either: the cell's
	// totals are cumulative, so the next announce carries the arrears.
	l3 := newUsageLedger(path, loc)
	l3.fold("gpu", &AnnounceUsage{Epoch: "e1", Models: []AnnounceUsageModel{usageOf("qwen", 12, 1200, 260, 470)}}, at.Add(2*time.Hour))
	// (no flush — simulate the kill)
	l4 := newUsageLedger(path, loc)
	l4.fold("gpu", &AnnounceUsage{Epoch: "e1", Models: []AnnounceUsageModel{usageOf("qwen", 12, 1200, 260, 470)}}, at.Add(3*time.Hour))
	recovered := bucketFor(l4, usageKey{Day: "2026-08-03", Cell: "gpu", Model: "qwen", Basis: "chat", Epoch: "e1"})
	if recovered.Req != 12 || recovered.InFresh != 1200 || recovered.Out != 470 {
		t.Fatalf("arrears not recovered after an unflushed fold: %+v", recovered)
	}
}

func TestUsageLedger_CompactCollapsesReplayHistoryToOneLinePerKey(t *testing.T) {
	loc := toronto(t)
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	at := time.Date(2026, 8, 3, 14, 0, 0, 0, loc)

	l := newUsageLedger(path, loc)
	for i := int64(1); i <= 5; i++ {
		l.fold("gpu", &AnnounceUsage{Epoch: "e1", Models: []AnnounceUsageModel{usageOf("qwen", i, i*100, 0, i*10)}}, at)
		if err := l.flush(); err != nil {
			t.Fatalf("flush %d: %v", i, err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := len(strings.Split(strings.TrimSpace(string(data)), "\n")); got != 5 {
		t.Fatalf("pre-compaction line count = %d, want 5 appends", got)
	}

	// A restart compacts.
	l2 := newUsageLedger(path, loc)
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after compact: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("post-compaction line count = %d, want 1", len(lines))
	}
	got := bucketFor(l2, usageKey{Day: "2026-08-03", Cell: "gpu", Model: "qwen", Basis: "chat", Epoch: "e1"})
	if got.Req != 5 || got.InFresh != 500 {
		t.Fatalf("compaction changed the totals: %+v", got)
	}
}

func TestUsageFold_LostRowsLandOnACellRowNotAModelRow(t *testing.T) {
	loc := toronto(t)
	l, _ := newLedger(t, loc)
	at := time.Date(2026, 8, 3, 14, 0, 0, 0, loc)

	l.fold("gpu", &AnnounceUsage{Epoch: "e1", LostRows: 38, Models: []AnnounceUsageModel{usageOf("qwen", 2, 20, 0, 10)}}, at)

	model := bucketFor(l, usageKey{Day: "2026-08-03", Cell: "gpu", Model: "qwen", Basis: "chat", Epoch: "e1"})
	if model.LostRows != 0 {
		t.Errorf("lost_rows landed on a model row: %+v", model)
	}
	cell := bucketFor(l, usageKey{Day: "2026-08-03", Cell: "gpu", Basis: usageBasisCell, Epoch: "e1"})
	if cell.LostRows != 38 {
		t.Errorf("cell row lost_rows = %d, want 38", cell.LostRows)
	}
}

func TestUsageResidency_CreditsTheGapAndCapsAnOfflineStretch(t *testing.T) {
	loc := toronto(t)
	l, _ := newLedger(t, loc)
	at := time.Date(2026, 8, 3, 14, 0, 0, 0, loc)
	models := []AnnounceModel{{ID: "qwen", State: "ready"}, {ID: "cold", State: "stopped"}}

	// The first announce has no predecessor and credits nothing.
	l.foldResidency("gpu", models, 15, at)
	l.foldResidency("gpu", models, 15, at.Add(15*time.Second))

	got := bucketFor(l, usageKey{Day: "2026-08-03", Cell: "gpu", Model: "qwen", Basis: usageBasisResident})
	if got.ResidentS != 15 {
		t.Fatalf("resident_s = %d, want 15", got.ResidentS)
	}
	if cold := bucketFor(l, usageKey{Day: "2026-08-03", Cell: "gpu", Model: "cold", Basis: usageBasisResident}); cold.ResidentS != 0 {
		t.Errorf("a stopped model was credited residency: %+v", cold)
	}

	// A six-hour absence credits at most the staleness bound, not six
	// hours of imagined residency.
	l.foldResidency("gpu", models, 15, at.Add(6*time.Hour))
	got = bucketFor(l, usageKey{Day: "2026-08-03", Cell: "gpu", Model: "qwen", Basis: usageBasisResident})
	if want := int64(15 + staleAfter(15).Seconds()); got.ResidentS != want {
		t.Fatalf("resident_s = %d, want %d (the gap capped at the staleness bound)", got.ResidentS, want)
	}
}

// Heartbeats land on a jittered cadence, so truncating each gap to whole
// seconds loses half a second on average — a steady under-count of the
// residency figure C7b turns into energy.
func TestUsageResidency_RoundsRatherThanTruncatesTheGap(t *testing.T) {
	loc := toronto(t)
	l, _ := newLedger(t, loc)
	at := time.Date(2026, 8, 3, 14, 0, 0, 0, loc)
	models := []AnnounceModel{{ID: "qwen", State: "ready"}}

	l.foldResidency("gpu", models, 15, at)
	// Four 15.6 s gaps: truncation yields 60 s, rounding yields 64 s, and
	// the real elapsed time is 62.4 s.
	for i := 1; i <= 4; i++ {
		l.foldResidency("gpu", models, 15, at.Add(time.Duration(i)*15600*time.Millisecond))
	}
	got := bucketFor(l, usageKey{Day: "2026-08-03", Cell: "gpu", Model: "qwen", Basis: usageBasisResident})
	if got.ResidentS != 64 {
		t.Fatalf("resident_s = %d, want 64 (rounded); 60 means the gap was truncated", got.ResidentS)
	}
}

// The ledger's day boundary must come from an explicit Location.
// Truncate rounds against absolute time since the Unix epoch and lands
// on UTC midnight regardless of the Location the value carries — with no
// error, no type mismatch, and no way to notice from the output.
func TestNoTruncateBasedDayBucketing(t *testing.T) {
	forbidden := "Truncate(24" + " *" + " time.Hour)"
	alsoForbidden := "Truncate(24" + "*time.Hour)"
	for _, dir := range []string{".", "../usagemeter"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || e.Name() == "c7a_test.go" {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatalf("read %s: %v", e.Name(), err)
			}
			if strings.Contains(string(data), forbidden) || strings.Contains(string(data), alsoForbidden) {
				t.Errorf("%s/%s truncates to a 24h boundary; day buckets must use an explicit *time.Location", dir, e.Name())
			}
		}
	}
}

func TestUsageReport_ServesRawCountsWithNoPrices(t *testing.T) {
	loc := toronto(t)
	dir := t.TempDir()
	s := New(
		[]Cell{{Name: "front", URL: "http://127.0.0.1:1"}, {Name: "gpu", URL: "http://127.0.0.1:1"}},
		filepath.Join(dir, "hist.json"), testDaemonInfo,
		Options{
			IntentPath: filepath.Join(dir, "intent.json"),
			UsagePath:  filepath.Join(dir, "usage.jsonl"),
			Timezone:   loc,
		},
	)
	t.Cleanup(s.Close)
	mux := http.NewServeMux()
	s.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	at := time.Now().In(loc)
	s.usage.fold("gpu", &AnnounceUsage{Epoch: "e1", Models: []AnnounceUsageModel{usageOf("qwen", 4, 400, 900, 120)}}, at)

	resp, err := http.Get(ts.URL + "/api/fleet/usage")
	if err != nil {
		t.Fatalf("GET /api/fleet/usage: %v", err)
	}
	var served UsageReport
	if err := json.NewDecoder(resp.Body).Decode(&served); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if len(served.Buckets) != 1 || served.Buckets[0].Cell != "gpu" {
		t.Fatalf("served buckets = %+v", served.Buckets)
	}

	rep := s.UsageReport("")
	if len(rep.Buckets) != 1 {
		t.Fatalf("report buckets = %d, want 1", len(rep.Buckets))
	}
	if rep.TZ != loc.String() {
		t.Errorf("report tz = %q, want %q", rep.TZ, loc.String())
	}
	blob, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, forbidden := range []string{"usd", "cost", "price", "dollar", "saving"} {
		if strings.Contains(strings.ToLower(string(blob)), forbidden) {
			t.Errorf("usage report mentions %q; C7a stores counts, C7b prices them", forbidden)
		}
	}
	// Filtering by day works.
	if got := s.UsageReport("2999-01-01"); len(got.Buckets) != 0 {
		t.Errorf("since-filter returned %d buckets, want 0", len(got.Buckets))
	}
}

// An announce carrying usage must still be a valid announce, and a
// version-skew basis string must not cost the cell its heartbeat (which
// would take presence and the intent echo down with the accounting).
func TestAnnounceUsage_IngestsAndToleratesAnUnknownBasis(t *testing.T) {
	dir := t.TempDir()
	s := New(
		[]Cell{{Name: "front", URL: "http://127.0.0.1:1"}, {Name: "gpu", URL: "http://127.0.0.1:1"}},
		filepath.Join(dir, "hist.json"), testDaemonInfo,
		Options{IntentPath: filepath.Join(dir, "intent.json"), UsagePath: filepath.Join(dir, "usage.jsonl"), Timezone: time.UTC},
	)
	t.Cleanup(s.Close)

	req := &AnnounceRequest{
		V: AnnounceVersion, Cell: "gpu", Seq: 1,
		Intent: &AnnounceIntent{State: "serving", Since: time.Now().UTC()},
		Models: []AnnounceModel{{ID: "qwen", State: "ready"}},
		Usage: &AnnounceUsage{Epoch: "e1", Models: []AnnounceUsageModel{
			{Model: "qwen", Basis: "chat", Req: 3, InFresh: 300, InCached: 700, Out: 90},
			{Model: "qwen", Basis: "future-basis-from-a-newer-cell", Req: 1, InFresh: 10},
		}},
	}
	if err := validateAnnounce(req); err != nil {
		t.Fatalf("validateAnnounce rejected a usage-bearing announce: %v", err)
	}
	s.recordAnnounce(req)

	rep := s.UsageReport("")
	if len(rep.Buckets) < 2 {
		t.Fatalf("buckets = %d, want the chat bucket and the unknown-basis bucket", len(rep.Buckets))
	}
	// A control character in a usage string is still rejected, same as
	// every other announce-supplied string that reaches a status surface.
	bad := &AnnounceRequest{V: AnnounceVersion, Cell: "gpu", Usage: &AnnounceUsage{Epoch: "e1", Models: []AnnounceUsageModel{{Model: "q\x00wen", Basis: "chat"}}}}
	if err := validateAnnounce(bad); err == nil {
		t.Error("validateAnnounce accepted a control character in usage.models[].model")
	}
}

// ─── adversarial review pass ────────────────────────────────────────────

// lost_rows arrives cumulatively like every other counter. Assigning it
// to the bucket instead of folding a delta made every day after a loss
// repeat the same number, so any cross-day sum was wrong.
func TestUsageFold_LostRowsFoldAsADeltaAcrossDays(t *testing.T) {
	loc := toronto(t)
	l, _ := newLedger(t, loc)
	d1 := time.Date(2026, 8, 3, 12, 0, 0, 0, loc)
	d2 := time.Date(2026, 8, 4, 12, 0, 0, 0, loc)

	l.fold("gpu", &AnnounceUsage{Epoch: "e1", LostRows: 40}, d1)
	l.fold("gpu", &AnnounceUsage{Epoch: "e1", LostRows: 40}, d1.Add(time.Minute))
	l.fold("gpu", &AnnounceUsage{Epoch: "e1", LostRows: 55}, d2)

	day1 := bucketFor(l, usageKey{Day: "2026-08-03", Cell: "gpu", Basis: usageBasisCell, Epoch: "e1"})
	day2 := bucketFor(l, usageKey{Day: "2026-08-04", Cell: "gpu", Basis: usageBasisCell, Epoch: "e1"})
	if day1.LostRows != 40 {
		t.Errorf("day 1 lost_rows = %d, want 40 (the repeated cumulative value must not add)", day1.LostRows)
	}
	if day2.LostRows != 15 {
		t.Errorf("day 2 lost_rows = %d, want 15 (the delta, not the cumulative 55)", day2.LostRows)
	}
}

// A usage block that carries ONLY a loss still folds: a cell that lost
// every row it tried to read has something to say.
func TestUsageFold_ModellessLossStillLands(t *testing.T) {
	loc := toronto(t)
	l, _ := newLedger(t, loc)
	l.fold("gpu", &AnnounceUsage{Epoch: "e1", LostRows: 7}, time.Date(2026, 8, 3, 12, 0, 0, 0, loc))
	if got := bucketFor(l, usageKey{Day: "2026-08-03", Cell: "gpu", Basis: usageBasisCell, Epoch: "e1"}); got.LostRows != 7 {
		t.Fatalf("model-less loss dropped: %+v", got)
	}
}

// Compaction rewrites the file from memory. Compacting after a PARTIAL
// read would delete whatever didn't parse, turning a transient read
// error into permanent data loss.
func TestUsageLedger_DegradedReadSkipsCompaction(t *testing.T) {
	loc := toronto(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.jsonl")
	if err := os.WriteFile(path, []byte(`{"d":"2026-08-03","cell":"gpu","model":"qwen","basis":"chat","epoch":"e1","req":5}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Unreadable file: load() must not leave an empty ledger that
	// compaction then writes over the real one.
	if err := os.Chmod(path, 0o000); err != nil {
		t.Skipf("cannot chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
	if f, err := os.Open(path); err == nil {
		f.Close()
		t.Skip("running as a user that ignores file modes (root?)")
	}

	_ = newUsageLedger(path, loc)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), `"req":5`) {
		t.Fatalf("compaction erased an unread ledger: %q", data)
	}
}
