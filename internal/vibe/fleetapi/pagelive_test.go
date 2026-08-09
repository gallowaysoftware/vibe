package fleetapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The page's offline state, traced from the handler to what a reader
// sees.
//
// The defect: refresh() threw, every caller was `.catch(() => {})`, and
// render() ran only on success. So when fleetd died the table FROZE with
// every SERVING badge still green, cued by nothing but a bare
// toLocaleTimeString() in 12px dim grey. That is absent evidence read as
// a healthy value — the class this plan has refused seven times
// elsewhere — on the one surface someone glances at from a hallway.
//
// These tests stand up a real fleetapi server over a real fixture cell,
// fetch the page and the state document over HTTP, and drive the page's
// OWN liveness ladder — the JSON bytes the browser parses at load — over
// the ages that matter. The ladder is data on purpose: a guard that had
// to read a chain of ifs would be reading prose.

// livenessRow mirrors one row of the page's LIVENESS table.
type livenessRow struct {
	Level      string `json:"level"`
	Never      bool   `json:"never"`
	MinAgeS    *int   `json:"min_age_s"`
	Failing    bool   `json:"failing"`
	StreamDown bool   `json:"stream_down"`
	Cls        string `json:"cls"`
	Neutralise bool   `json:"neutralise"`
	Head       string `json:"head"`
	Detail     string `json:"detail"`
}

// livenessLadder parses the ladder out of the page the SERVER just
// handed us — not out of the file on disk, and not out of a copy in this
// test. These are the bytes a browser evaluates.
func livenessLadder(t *testing.T, page string) []livenessRow {
	t.Helper()
	const open = "const LIVENESS = JSON.parse(`"
	i := strings.Index(page, open)
	require.GreaterOrEqual(t, i, 0, "the page has no LIVENESS ladder; this guard is measuring nothing")
	rest := page[i+len(open):]
	end := strings.Index(rest, "`")
	require.Greater(t, end, 0, "the LIVENESS ladder is not terminated")
	var rows []livenessRow
	require.NoError(t, json.Unmarshal([]byte(rest[:end]), &rows),
		"the LIVENESS ladder is not valid JSON, so the page cannot parse it either")
	require.NotEmpty(t, rows)
	return rows
}

// pickLiveness is the page's own selection rule: the first row whose
// declared conditions all hold. Six lines, and the only thing about this
// mechanism that is written twice — everything with content (the
// thresholds, the levels, whether the table is neutralised, the words a
// reader sees) is read out of the shipped page.
func pickLiveness(rows []livenessRow, never bool, ageS int, failing, streamDown bool) livenessRow {
	for _, r := range rows {
		if r.Never != never {
			continue
		}
		if r.MinAgeS != nil && ageS < *r.MinAgeS {
			continue
		}
		if r.Failing && !failing {
			continue
		}
		if r.StreamDown && !streamDown {
			continue
		}
		return r
	}
	return rows[len(rows)-1]
}

// newPageServer builds a fleetd-role server (the page route is mounted
// only in that role) over one fixture cell, with the real route table on
// a real listener.
func newPageServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	cell := newFakeCell(t)
	cell.runningJSON = `{"running":[{"model":"qwen","state":"ready"}]}`
	dir := t.TempDir()
	s := New([]Cell{{Name: "front", URL: cell.srv.URL, Class: "always_on"}},
		filepath.Join(dir, "hist.json"), testDaemonInfo, Options{
			IntentPath: filepath.Join(dir, "intent.json"),
		})
	s.baseBackoff = 10 * time.Millisecond
	s.maxBackoff = 50 * time.Millisecond
	mux := http.NewServeMux()
	s.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	t.Cleanup(s.Close)
	return s, ts
}

// livePageServer stands up a real fleetapi server over one fixture cell
// that answers, mounts the real route table, and returns the page and
// the state document as the wire delivers them.
func livePageServer(t *testing.T) (page string, state map[string]any, s *Server) {
	t.Helper()
	s, ts := newPageServer(t)

	resp, err := http.Get(ts.URL + fleetPagePath)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	page = string(body)

	sresp, err := http.Get(ts.URL + "/api/fleet/state")
	require.NoError(t, err)
	defer sresp.Body.Close()
	require.Equal(t, http.StatusOK, sresp.StatusCode)
	require.NoError(t, json.NewDecoder(sresp.Body).Decode(&state))
	return page, state, s
}

// TestFleetPage_OfflineStateFromHandlerToDOM is the headline: what a
// reader sees at 0s, 30s and five minutes after fleetd dies, driven from
// the served page over the document the served handler produced.
func TestFleetPage_OfflineStateFromHandlerToDOM(t *testing.T) {
	page, state, _ := livePageServer(t)
	rows := livenessLadder(t, page)

	// The handler's own claim, which is the thing that must stop being
	// rendered as green: one cell, answering, SERVING.
	cells, _ := state["cells"].([]any)
	require.Len(t, cells, 1, "fixture must produce exactly one cell row")
	first, _ := cells[0].(map[string]any)
	require.Equal(t, DisplayServing, first["display"],
		"the fixture cell is not SERVING, so nothing below proves a green badge was neutralised")
	// generated_at is the server-side half of the age the ladder clocks
	// against. If the handler stops emitting it the page silently loses
	// its "the server stopped regenerating this" signal.
	gen, ok := state["generated_at"].(string)
	require.True(t, ok, "the state document carries no generated_at")
	_, err := time.Parse(time.RFC3339Nano, gen)
	require.NoError(t, err, "generated_at is not a parseable instant, so the page's clock cannot read it")
	require.Contains(t, page, "Date.parse(st.generated_at)",
		"the page no longer clocks its age against the server's own stamp")
	// …and it subtracts the smallest gap it has ever seen before believing
	// that stamp. A constant offset between the browser's clock and
	// fleetd's — minutes, on a LAN with no NTP — would otherwise hold a
	// perfectly healthy fleet at OFFLINE forever, and a banner that is
	// always red is a banner nobody reads.
	require.Contains(t, page, "lag - minServerLagS",
		"the server's stamp is trusted raw, so clock skew alone can pin a healthy fleet at OFFLINE")

	// Healthy: the control. Without it the assertions below would pass on
	// a page that neutralises everything all the time.
	live := pickLiveness(rows, false, 3, false, false)
	require.Equal(t, "live", live.Level)
	require.False(t, live.Neutralise, "a fleet that just answered must render as live")

	// t+0: fleetd dies. The SSE stream drops within a second; the poll has
	// not come round yet and the data on screen is genuinely seconds old.
	// The two signals are DIFFERENT and are handled differently: the page
	// says the live channel is gone, and does NOT neutralise a table whose
	// data is still fresh.
	at0 := pickLiveness(rows, false, 1, false, true)
	require.Equal(t, "degraded", at0.Level)
	require.False(t, at0.Neutralise,
		"the event stream dropping must not grey out data that is one second old — that is its own lie")
	require.Contains(t, strings.ToLower(at0.Detail), "event stream")

	// t+30s: the 30s poll fires and fails. From here the rows on screen
	// are the last thing the page heard, and every badge stops being
	// coloured as an observation.
	at30 := pickLiveness(rows, false, 30, true, true)
	require.Equal(t, "stale", at30.Level)
	require.True(t, at30.Neutralise,
		"thirty seconds after fleetd died the table still renders SERVING in green — this is the whole defect")
	require.Equal(t, "STALE", at30.Head)

	// t+5min: offline, and it says so in the strongest terms the page has.
	at300 := pickLiveness(rows, false, 300, true, true)
	require.Equal(t, "offline", at300.Level)
	require.True(t, at300.Neutralise)
	require.Equal(t, "OFFLINE", at300.Head)
	require.Contains(t, at300.Detail, "LAST STATE RECEIVED")

	// A page that has never had data must not look like one that has.
	never := pickLiveness(rows, true, 0, false, true)
	require.Equal(t, "never", never.Level)
	require.True(t, never.Neutralise)

	// Age alone is enough, with no failure recorded: a tab whose timers
	// were throttled while the machine slept has old data and no error.
	throttled := pickLiveness(rows, false, 300, false, false)
	require.True(t, throttled.Neutralise,
		"five-minute-old data with no recorded failure still renders as live")
}

// TestFleetPage_NeutralisedBadgesAreNotGreen binds the ladder's
// `neutralise` flag to a visual consequence. A flag that no stylesheet
// honours is a flag that changes nothing.
func TestFleetPage_NeutralisedBadgesAreNotGreen(t *testing.T) {
	page, _, _ := livePageServer(t)

	// The wiring: the flag drives the body class, and the body class is
	// what the stylesheet keys on.
	require.Contains(t, page, `document.body.classList.toggle("notlive", !!r.neutralise)`,
		"the ladder's neutralise flag is not wired to anything")

	rule := regexp.MustCompile(`body\.notlive \.badge \{[^}]*\}`).FindString(page)
	require.NotEmpty(t, rule, "no stylesheet rule neutralises a badge while the page is not live")
	// --green is what .b-serving paints with. A neutralising rule that
	// still resolves to it would leave the exact appearance this exists
	// to remove.
	require.NotContains(t, rule, "--green")
	require.NotContains(t, rule, "76,175,125", "the neutralised badge is still the serving green")
	require.Contains(t, rule, "--gray")

	// And the per-model colours, which carry the same claim one column
	// over.
	require.Regexp(t, regexp.MustCompile(`body\.notlive \.m-ready`), page,
		"a stale model row still renders ready in green")
}

// TestFleetPage_NoCallerSwallowsAFailedRefresh is the defect stated
// exactly. `refresh().catch(() => {})` at every call site is what made a
// dead fleetd invisible: the throw was discarded, render() never ran, and
// the last good table stayed on screen looking live.
func TestFleetPage_NoCallerSwallowsAFailedRefresh(t *testing.T) {
	page, _, _ := livePageServer(t)
	require.NotContains(t, page, "refresh().catch(() => {})",
		"a refresh failure is being discarded again; the page then keeps rendering the last good state "+
			"with every SERVING badge green and no indication that nothing is arriving")
	// The single handler that replaced them records the failure and shows
	// it, rather than dropping it.
	require.Contains(t, page, "lastFetchError =",
		"nothing records that the last state fetch failed")
	require.Contains(t, page, "function scheduleStateRetry(",
		"there is no reconnect path: an unreachable fleetd is only retried on the 30s poll")
	require.Contains(t, page, `onclick="reconnect()"`,
		"there is no manual reconnect control on the banner")
}

// TestFleetPage_TheAgeKeepsCounting pins the second half of "last
// updated N ago": a relative age that is only recomputed when an update
// arrives freezes at the moment the updates stop, which is precisely
// when a reader needs it to move.
func TestFleetPage_TheAgeKeepsCounting(t *testing.T) {
	page, _, _ := livePageServer(t)
	require.Contains(t, page, "setInterval(renderLiveness, 1000)",
		"the liveness banner is not on its own timer, so the age stops counting exactly when fleetd stops answering")
	require.Contains(t, page, `"last updated " + relS(age) + " ago"`,
		"the banner does not render a relative age")
	require.Contains(t, page, "clearInterval(livenessTimer)",
		"boot() does not clear the liveness timer; a token rotation would stack another one")
}

// TestFleetPage_SleepBlockIsRendered — `st.sleep` appeared ZERO times in
// this page. WakeFailedSince is documented as the one thing in C14 that
// alarms and is a doctor LevelFail, and notifyReport() returns nil on a
// fleet with no webhook — so a wake this fleet promised and did not
// deliver had no signal anywhere a human looks.
//
// Traced from the handler: the state document is produced by a real
// server with a real failed wake on it, and the page's renderer is
// checked against the KEYS that document actually carries.
func TestFleetPage_SleepBlockIsRendered(t *testing.T) {
	s, ts := newPageServer(t)
	// Every optional field populated, deliberately: the trace below
	// checks each key the renderer reads against the keys the document
	// carries, and an `omitempty` field left zero would make a real
	// mismatch look like an absent one.
	now := time.Now().UTC()
	failedAt := now.Add(-19 * time.Minute)
	s.mu.Lock()
	s.sleepStates = append(s.sleepStates, &sleepScheduleState{
		Cell: "front", SuspendCron: "0 1 * * *", WakeCron: "0 7 * * *",
		State: "wake_failed", Detail: "cell did not come back within 10m",
		QuietForS:   1800,
		NextSuspend: &now, NextWake: &now, LastSuspend: &now, LastWake: &now,
		DeferredSince: &now, LastSkip: "cell busy at the fire time",
		WakeFailedSince: &failedAt,
	})
	s.mu.Unlock()

	resp, err := http.Get(ts.URL + "/api/fleet/state")
	require.NoError(t, err)
	defer resp.Body.Close()
	var doc map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&doc))

	sleep, ok := doc["sleep"].(map[string]any)
	require.True(t, ok, "the state document carries no sleep block")
	entries, ok := sleep["entries"].([]any)
	require.True(t, ok)
	require.Len(t, entries, 1)
	entry, _ := entries[0].(map[string]any)
	require.Contains(t, entry, "wake_failed_since",
		"a failed wake is not on the wire, so no page could render it")

	pageResp, err := http.Get(ts.URL + fleetPagePath)
	require.NoError(t, err)
	defer pageResp.Body.Close()
	raw, err := io.ReadAll(pageResp.Body)
	require.NoError(t, err)
	page := string(raw)

	require.Contains(t, page, "renderSleep(st.sleep)", "render() does not render the sleep block at all")
	body := funcBody(t, page, "function renderSleep(")
	// Every key the renderer reads off an entry must be a key the handler
	// actually emits — that is the trace, and it fails in BOTH directions
	// of a rename.
	for _, key := range regexp.MustCompile(`\be\.([a-z_]+)\b`).FindAllStringSubmatch(body, -1) {
		require.Containsf(t, entry, key[1],
			"renderSleep reads e.%s, which the state document does not carry", key[1])
	}
	require.Contains(t, body, "e.wake_failed_since",
		"the page renders the sleep schedule but not the failed wake, which is the only part of it that alarms")
	require.Contains(t, body, `"warn-item"`,
		"a promised wake that did not happen renders as ordinary status text, not as a warning")
}

// funcBody returns the source of the function starting at marker, up to
// its closing brace at column zero.
func funcBody(t *testing.T, page, marker string) string {
	t.Helper()
	i := strings.Index(page, marker)
	require.GreaterOrEqual(t, i, 0, "%s not found on the page", marker)
	rest := page[i:]
	end := strings.Index(rest, "\n}")
	require.Greater(t, end, 0, "%s has no closing brace where this guard expects one", marker)
	return rest[:end]
}

// TestFleetPage_DrainWaitsAndAsks — mk("drain", "drain_cell", {cell})
// passed no wait_seconds, so one click always cancelled every in-flight
// generation on that cell, with no confirmation and no recorded reason.
// The tool's own description says never to tell an operator their stream
// is safe; a button that truncates silently is that sentence broken by
// omission.
func TestFleetPage_DrainWaitsAndAsks(t *testing.T) {
	page, _, _ := livePageServer(t)

	require.NotContains(t, page, `mk("drain", "drain_cell", { cell: c.name });`,
		"the drain button still sends drain_cell with no wait_seconds")
	body := funcBody(t, page, "function drainCell(")
	require.Contains(t, body, "wait_seconds: DRAIN_WAIT_SECONDS",
		"the drain button does not pass a wait, so llama-swap's SIGTERM truncates whatever is generating")
	require.Contains(t, body, "prompt(", "the drain button does not confirm")
	require.Contains(t, body, "if (reason === null) return;",
		"cancelling the confirmation still drains the cell")
	require.Contains(t, body, "reason: reason.trim()",
		"the drain records no reason, which is the difference between DRAINED and DRAINED?")
	require.Contains(t, body, "CANCELLED",
		"the confirmation does not say what happens to work still generating when the wait runs out")

	wait := regexp.MustCompile(`const DRAIN_WAIT_SECONDS = (\d+);`).FindStringSubmatch(page)
	require.NotNil(t, wait, "DRAIN_WAIT_SECONDS is not declared")
	require.NotEqual(t, "0", wait[1], "a zero wait is the immediate SIGTERM this fix exists to stop")
}

// TestFleetPage_ToolResultsSurviveTheNextRender — flash() put every tool
// result into a 60-character, hover-only title= that render() destroyed
// 1.5s later. drain_cell's "WARNING: the requested wait was SKIPPED …
// cancelled any streams" and its lease list vanished unseen, and
// warm_model's ETA could not fit at all: its prefix alone is 64
// characters before the model id.
func TestFleetPage_ToolResultsSurviveTheNextRender(t *testing.T) {
	page, _, _ := livePageServer(t)

	require.NotContains(t, page, "text.slice(0, 60)",
		"tool results are still truncated to 60 characters")
	require.NotContains(t, page, "btn.title = msg",
		"tool results still go into a hover-only title the next render() destroys")
	require.Contains(t, page, `$("tr-body").textContent = text;`,
		"the full tool result is not rendered anywhere")
	// The panel must live OUTSIDE the tbody render() rebuilds, or it is
	// the same defect with more markup.
	require.Contains(t, page, `<pre id="tr-body">`, "the result area does not preserve newlines")
	require.NotContains(t, funcBody(t, page, "function render(st) {"), "toolresult",
		"the result panel is rebuilt by render(), so it still cannot outlive a refresh")
	require.Contains(t, page, "function dismissResult(", "the result panel cannot be dismissed")

	// The arithmetic that made the old field structurally unusable: the
	// warm reply's prefix is longer than the field it was written to.
	const prefix = "Warming %s through the front (JIT load started in the background). ETA from history: ~"
	require.Greater(t, len(prefix), 60,
		"this assertion is stale — the warm prefix now fits in 60 characters and the arithmetic argument no longer holds")
}
