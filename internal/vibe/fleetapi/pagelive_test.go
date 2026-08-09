package fleetapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"slices"
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
//
// # What each test here can and cannot see
//
// An adversarial pass found six of the seven asserting that a STRING
// appears in the page. Three of those could not see their own defect: a
// reintroduced `refresh().catch(()=>{})` (no spaces) or a
// `text.slice(0,60)` sails past a NotContains of the spaced spelling.
// That is the weakest possible guard on the newest safety feature, so
// the split is now explicit and each test says which side it is on.
//
// COMPUTED — these derive a value from the served bytes and assert on
// the value, so a respelling cannot pass them:
//
//   - OfflineStateFromHandlerToDOM parses the LIVENESS table out of the
//     served page and runs the page's own selection rule over it.
//   - NeutralisedBadgesAreNotGreen RESOLVES THE CASCADE: it parses the
//     shipped stylesheet, matches selectors against a real element
//     context with specificity and source order, and asserts the colour
//     a browser would paint. A later rule that re-asserted the serving
//     green would pass every grep and fail this.
//   - ToolResultsSurviveTheNextRender walks the served HTML's tag
//     nesting and asserts #toolresult is not INSIDE the element render()
//     rebuilds — the containment claim, computed, not quoted.
//   - SleepBlockIsRendered traces every key the renderer reads against
//     the keys the live handler actually emits, in both directions.
//
// STRUCTURAL — control flow in a language nothing here executes. Made
// EXHAUSTIVE rather than literal: they enumerate every call site / every
// timer and assert a property of all of them, so the class is closed
// even though the mechanism is not run:
//
//   - NoCallerSwallowsAFailedRefresh — every `refresh()` call site.
//   - TheAgeKeepsCounting — every setInterval and its teardown.
//   - DrainWaitsAndAsks — the button's argument shape. Its other half,
//     that those argument names are ones the receiving tool declares and
//     honours, is a cross-package wire contract and lives in
//     internal/vibe/fleetmcp (TestFleetPageToolCallsMatchTheToolSchemas):
//     fleetmcp imports fleetapi, so the check can only run from there.
//
// Running the page's JS is the one thing that would collapse the second
// group into the first, and it needs a JS engine. This repo takes no new
// dependencies for a test, and a headless browser in the blocking job
// would cost more than the class is worth. Saying so beats leaving a
// grep dressed as a behavioural test.

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
//
// COMPUTED, not grepped. The previous version asserted that the text
// `body.notlive .badge { … }` did not contain `--green`, which cannot
// see the two ways this actually breaks: a LATER rule re-asserting the
// serving colour (the cascade's whole point), and a --gray that someone
// has redefined to a green. So this resolves the cascade — every rule
// in the shipped stylesheet, matched against a real element context by
// specificity and source order, with var() substitution — and asserts
// the colour a browser would paint.
func TestFleetPage_NeutralisedBadgesAreNotGreen(t *testing.T) {
	page, _, _ := livePageServer(t)

	// The wiring: the flag drives the body class, and the body class is
	// what the stylesheet keys on. This one IS a literal, and it is the
	// hinge everything below hangs off, so it is asserted separately.
	require.Contains(t, page, `document.body.classList.toggle("notlive", !!r.neutralise)`,
		"the ladder's neutralise flag is not wired to anything")

	sheet := parseStylesheet(t, page)
	// The custom properties are read out of the sheet too: a --gray that
	// had been edited to a green would satisfy every "contains --gray"
	// assertion ever written and repaint the whole neutralised table.
	require.NotEqual(t, sheet.vars["--green"], sheet.vars["--gray"],
		"--gray and --green resolve to the same colour, so neutralising a badge repaints it as SERVING")
	require.NotEmpty(t, sheet.vars["--green"], "the page defines no --green, so nothing below is comparing colours")

	badge := []node{{tag: "body"}, {tag: "span", classes: []string{"badge", "b-serving"}}}
	notlive := []node{{tag: "body", classes: []string{"notlive"}}, {tag: "span", classes: []string{"badge", "b-serving"}}}

	live := sheet.computed(badge, "color")
	require.Equal(t, sheet.vars["--green"], live,
		"a SERVING badge on a live page does not paint green, so the neutralised comparison below proves nothing")

	dead := sheet.computed(notlive, "color")
	require.NotEqual(t, live, dead,
		"a SERVING badge paints the SAME colour whether or not the page has gone dark — this is the whole defect: "+
			"a green badge asserting an observation nobody is making")
	require.Equal(t, sheet.vars["--gray"], dead, "the neutralised badge is not the muted grey the ladder promises")
	// The tinted background carries the same claim as the text colour.
	require.NotEqual(t, sheet.computed(badge, "background"), sheet.computed(notlive, "background"),
		"the badge keeps its green tint while the page is not live")

	// And the per-model colours, which carry the same claim one column
	// over. m-ready is the green one; m-degraded is the red one, and a
	// neutralised page must stop shouting that too.
	for _, cls := range []string{"m-ready", "m-degraded"} {
		liveEl := []node{{tag: "body"}, {tag: "span", classes: []string{cls}}}
		deadEl := []node{{tag: "body", classes: []string{"notlive"}}, {tag: "span", classes: []string{cls}}}
		require.NotEqualf(t, sheet.computed(liveEl, "color"), sheet.computed(deadEl, "color"),
			"a .%s model still renders in its live colour on a page that has gone dark", cls)
	}
}

// ── a stylesheet, resolved ────────────────────────────────────────────
//
// Enough CSS to answer "what colour would a browser paint this" for the
// selector shapes this page uses: comma-separated lists of descendant
// chains of compound selectors (`body.notlive .m-ready, …`). Specificity
// and source order decide; var() is substituted from :root.
//
// Deliberately small. It is not a CSS engine and does not need to be —
// it needs to be something a respelled rule cannot slip past, which a
// substring search is not.

type node struct {
	tag     string
	id      string
	classes []string
}

type cssRule struct {
	sel   string
	decls map[string]string
	order int
}

type stylesheet struct {
	rules []cssRule
	vars  map[string]string
}

var (
	styleBlockRE = regexp.MustCompile(`(?s)<style>(.*?)</style>`)
	cssCommentRE = regexp.MustCompile(`(?s)/\*.*?\*/`)
	cssRuleRE    = regexp.MustCompile(`(?s)([^{}]+)\{([^{}]*)\}`)
	cssVarRE     = regexp.MustCompile(`var\(\s*(--[a-zA-Z0-9_-]+)\s*\)`)
)

func parseStylesheet(t *testing.T, page string) *stylesheet {
	t.Helper()
	blocks := styleBlockRE.FindAllStringSubmatch(page, -1)
	require.NotEmpty(t, blocks, "the served page carries no <style> block, so nothing below is resolving anything")
	s := &stylesheet{vars: map[string]string{}}
	for _, b := range blocks {
		for _, m := range cssRuleRE.FindAllStringSubmatch(cssCommentRE.ReplaceAllString(b[1], ""), -1) {
			decls := map[string]string{}
			for _, d := range strings.Split(m[2], ";") {
				prop, val, ok := strings.Cut(d, ":")
				if !ok {
					continue
				}
				decls[strings.TrimSpace(prop)] = strings.TrimSpace(val)
			}
			sel := strings.TrimSpace(m[1])
			if sel == ":root" {
				for k, v := range decls {
					if strings.HasPrefix(k, "--") {
						s.vars[k] = v
					}
				}
			}
			s.rules = append(s.rules, cssRule{sel: sel, decls: decls, order: len(s.rules)})
		}
	}
	require.NotEmpty(t, s.rules, "no rules parsed out of the page's stylesheet")
	return s
}

// computed returns prop's winning value for the element at the end of
// chain (root-first), with var() resolved. "" when nothing declares it.
func (s *stylesheet) computed(chain []node, prop string) string {
	best, bestSpec, bestOrder := "", -1, -1
	for _, r := range s.rules {
		for _, sel := range strings.Split(r.sel, ",") {
			sel = strings.TrimSpace(sel)
			if sel == "" || !selectorMatches(sel, chain) {
				continue
			}
			v, ok := r.decls[prop]
			if !ok {
				continue
			}
			if spec := specificity(sel); spec > bestSpec || (spec == bestSpec && r.order > bestOrder) {
				best, bestSpec, bestOrder = v, spec, r.order
			}
		}
	}
	// Resolve one level of var(); the page's custom properties are all
	// literal colours, so one level is all there is.
	return cssVarRE.ReplaceAllStringFunc(best, func(m string) string {
		return s.vars[cssVarRE.FindStringSubmatch(m)[1]]
	})
}

// selectorMatches walks the compound parts right-to-left: the last must
// match the element itself, and each earlier one must match some
// ancestor, in order. Descendant combinators only — the page uses no
// child/sibling combinators, and a selector carrying one would fail to
// match here rather than match wrongly.
func selectorMatches(sel string, chain []node) bool {
	parts := strings.Fields(sel)
	if len(parts) == 0 || len(chain) == 0 {
		return false
	}
	if !compoundMatches(parts[len(parts)-1], chain[len(chain)-1]) {
		return false
	}
	i := len(chain) - 2
	for p := len(parts) - 2; p >= 0; p-- {
		for {
			if i < 0 {
				return false
			}
			if compoundMatches(parts[p], chain[i]) {
				i--
				break
			}
			i--
		}
	}
	return true
}

func compoundMatches(compound string, n node) bool {
	tag, id, classes := splitCompound(compound)
	if tag != "" && tag != "*" && tag != n.tag {
		return false
	}
	if id != "" && id != n.id {
		return false
	}
	for _, c := range classes {
		if !slices.Contains(n.classes, c) {
			return false
		}
	}
	return true
}

func splitCompound(compound string) (tag, id string, classes []string) {
	for i := 0; i < len(compound); {
		j := strings.IndexAny(compound[i+1:], ".#")
		end := len(compound)
		if j >= 0 {
			end = i + 1 + j
		}
		tok := compound[i:end]
		switch {
		case strings.HasPrefix(tok, "."):
			classes = append(classes, tok[1:])
		case strings.HasPrefix(tok, "#"):
			id = tok[1:]
		default:
			tag = tok
		}
		i = end
	}
	return tag, id, classes
}

func specificity(sel string) int {
	n := 0
	for _, compound := range strings.Fields(sel) {
		tag, id, classes := splitCompound(compound)
		if id != "" {
			n += 100
		}
		n += 10 * len(classes)
		if tag != "" && tag != "*" {
			n++
		}
	}
	return n
}

// TestFleetPage_NoCallerSwallowsAFailedRefresh is the defect stated
// exactly. `refresh().catch(() => {})` at every call site is what made a
// dead fleetd invisible: the throw was discarded, render() never ran, and
// the last good table stayed on screen looking live.
//
// STRUCTURAL, and now EXHAUSTIVE. The previous version was a NotContains
// of one spelling — `refresh().catch(() => {})`, spaces and all — which
// is a guard that cannot see its own defect: `refresh().catch(()=>{})`,
// `.catch(function(){})` or `.catch(e => {})` reintroduce it verbatim and
// pass. So instead: find EVERY call site of refresh() in the served page
// and assert none of them attaches a rejection handler that does nothing,
// whatever the handler is spelled like.
func TestFleetPage_NoCallerSwallowsAFailedRefresh(t *testing.T) {
	page, _, _ := livePageServer(t)
	js := pageScript(t, page)

	// Every `refresh()` in the script, with what follows it. The one
	// legitimate rejection handler in the codebase is tick()'s, whose body
	// records the error and re-renders; an EMPTY body is the defect.
	sites := regexp.MustCompile(`refresh\(\)((?s).{0,80})`).FindAllStringSubmatch(js, -1)
	require.NotEmpty(t, sites, "no refresh() call sites found at all — this guard is measuring nothing")
	empty := regexp.MustCompile(`^\s*\.catch\(\s*(\(\s*[a-zA-Z0-9_,\s]*\s*\)|[a-zA-Z0-9_]+)\s*=>\s*\{\s*\}|^\s*\.catch\(\s*function\s*\([^)]*\)\s*\{\s*\}`)
	for _, s := range sites {
		require.NotRegexpf(t, empty, s[1],
			"a refresh() call site discards its rejection into an empty handler: %q. The page then keeps "+
				"rendering the last good state with every SERVING badge green and no indication that "+
				"nothing is arriving — which is the entire defect this file exists for.", strings.TrimSpace(s[0]))
	}

	// The single handler that replaced them records the failure and shows
	// it, rather than dropping it.
	require.Contains(t, page, "lastFetchError =",
		"nothing records that the last state fetch failed")
	require.Contains(t, page, "function scheduleStateRetry(",
		"there is no reconnect path: an unreachable fleetd is only retried on the 30s poll")
	require.Contains(t, page, `onclick="reconnect()"`,
		"there is no manual reconnect control on the banner")
}

// pageScript returns the page's inline <script> source, comments
// stripped. Stripping matters: this page documents its own defects in
// prose, and the literal `.catch(() => {})` appears in three comments
// describing what used to be there. A guard that searched the whole page
// would be reading those.
func pageScript(t *testing.T, page string) string {
	t.Helper()
	blocks := regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindAllStringSubmatch(page, -1)
	require.NotEmpty(t, blocks, "the served page carries no <script> block")
	var b strings.Builder
	line := regexp.MustCompile(`(?m)^\s*//.*$`)
	block := regexp.MustCompile(`(?s)/\*.*?\*/`)
	for _, m := range blocks {
		b.WriteString(line.ReplaceAllString(block.ReplaceAllString(m[1], ""), ""))
		b.WriteString("\n")
	}
	return b.String()
}

// TestFleetPage_TheAgeKeepsCounting pins the second half of "last
// updated N ago": a relative age that is only recomputed when an update
// arrives freezes at the moment the updates stop, which is precisely
// when a reader needs it to move.
// STRUCTURAL, and now EXHAUSTIVE on the half that has a class: every
// interval the page starts must be held in a variable boot() clears.
// boot() runs again on a mid-session token rotation (401 → gate → save →
// boot), so an unheld interval is not a tidiness point — it is a second
// renderLiveness firing every second, forever, for the rest of the tab's
// life.
func TestFleetPage_TheAgeKeepsCounting(t *testing.T) {
	page, _, _ := livePageServer(t)
	js := pageScript(t, page)

	require.Contains(t, js, "setInterval(renderLiveness, 1000)",
		"the liveness banner is not on its own timer, so the age stops counting exactly when fleetd stops answering")
	require.Contains(t, js, `"last updated " + relS(age) + " ago"`,
		"the banner does not render a relative age")

	// Every setInterval in the script, and the variable it was assigned
	// to. An assignment-less `setInterval(...)` has no handle at all.
	starts := regexp.MustCompile(`(?m)^\s*(?:([A-Za-z0-9_]+)\s*=\s*)?setInterval\(`).FindAllStringSubmatch(js, -1)
	require.NotEmpty(t, starts, "no setInterval call sites found at all — this guard is measuring nothing")
	cleared := map[string]bool{}
	for _, m := range regexp.MustCompile(`clearInterval\(\s*([A-Za-z0-9_]+)\s*\)`).FindAllStringSubmatch(js, -1) {
		cleared[m[1]] = true
	}
	for _, m := range starts {
		require.NotEmptyf(t, m[1],
			"a setInterval is started without keeping its handle (%q), so nothing can ever stop it: a token "+
				"rotation re-runs boot() and stacks another one on top", strings.TrimSpace(m[0]))
		require.Truef(t, cleared[m[1]],
			"the interval held in %s is never passed to clearInterval. boot() runs again on a mid-session "+
				"token rotation, so every rotation leaves another live timer behind — including the one "+
				"that keeps the liveness age counting.", m[1])
	}
	require.True(t, cleared["livenessTimer"],
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

// renderedContainerID is the id of the element render() replaces
// wholesale, read out of render() itself rather than assumed. If render()
// is ever pointed at a different container, the containment assertion
// follows it instead of quietly checking the wrong element.
func renderedContainerID(t *testing.T, js string) string {
	t.Helper()
	body := funcBody(t, js, "function render(st) {")
	// `const tb = $("cells"); tb.innerHTML = "";`
	handle := regexp.MustCompile(`(?m)^\s*(?:const|let|var)\s+([A-Za-z0-9_]+)\s*=\s*\$\("([a-zA-Z0-9_-]+)"\)`)
	for _, m := range handle.FindAllStringSubmatch(body, -1) {
		if regexp.MustCompile(regexp.QuoteMeta(m[1]) + `\.innerHTML\s*=`).MatchString(body) {
			return m[2]
		}
	}
	t.Fatal("render() no longer replaces any element's innerHTML: this guard cannot tell what it rebuilds, so " +
		"it cannot tell whether the result panel would survive it")
	return ""
}

var (
	htmlCommentRE = regexp.MustCompile(`(?s)<!--.*?-->`)
	htmlTagRE     = regexp.MustCompile(`</?([a-zA-Z][a-zA-Z0-9]*)((?:"[^"]*"|'[^']*'|[^>"'])*)>`)
	htmlIDRE      = regexp.MustCompile(`\bid="([^"]*)"`)
	// Elements with no closing tag; anything else is assumed to nest.
	htmlVoid = map[string]bool{
		"area": true, "base": true, "br": true, "col": true, "embed": true, "hr": true,
		"img": true, "input": true, "link": true, "meta": true, "param": true,
		"source": true, "track": true, "wbr": true,
	}
)

// elementAncestors returns the ids of the elements enclosing the element
// with the given id, in the served markup. The containment question is a
// fact about the document, so it is answered from the document.
//
// A deliberately small scanner: this page is hand-written, well-formed,
// and has no unclosed tags. `<script>` and `<style>` bodies are removed
// first so a `<` inside JS cannot be read as a tag.
func elementAncestors(t *testing.T, page, id string) []string {
	t.Helper()
	clean := htmlCommentRE.ReplaceAllString(page, "")
	clean = regexp.MustCompile(`(?s)<script>.*?</script>`).ReplaceAllString(clean, "")
	clean = regexp.MustCompile(`(?s)<style>.*?</style>`).ReplaceAllString(clean, "")

	var stack []string // ids (possibly "") of the open elements
	for _, m := range htmlTagRE.FindAllStringSubmatch(clean, -1) {
		tag := strings.ToLower(m[1])
		if strings.HasPrefix(m[0], "</") {
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			continue
		}
		if htmlVoid[tag] || strings.HasSuffix(strings.TrimSpace(m[2]), "/") {
			continue
		}
		self := ""
		if a := htmlIDRE.FindStringSubmatch(m[2]); a != nil {
			self = a[1]
		}
		if self == id {
			out := make([]string, 0, len(stack))
			for _, s := range stack {
				if s != "" {
					out = append(out, s)
				}
			}
			return out
		}
		stack = append(stack, self)
	}
	t.Fatalf("no element with id=%q in the served markup, so nothing about where it sits can be checked", id)
	return nil
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
//
// STRUCTURAL, and honestly so: what a prompt() returns and what the
// operator then sees is browser behaviour nothing here runs. Two things
// were done about that rather than leaving a grep looking behavioural.
//
// First, the negative assertion matches the SHAPE of a wait-less drain
// (any drain_cell call whose argument object has no wait_seconds) rather
// than the one literal line the defect happened to be written on.
//
// Second, the half that CAN be executed — that `wait_seconds` is a name
// the receiving tool declares and does not silently drop — is a
// cross-package wire contract and runs in
// internal/vibe/fleetmcp/pagetools_test.go, which can see both the served
// page and the tool schemas. A rename on either side fails there.
func TestFleetPage_DrainWaitsAndAsks(t *testing.T) {
	page, _, _ := livePageServer(t)
	js := pageScript(t, page)

	sites := regexp.MustCompile(`"drain_cell"\s*,\s*\{([^}]*)\}`).FindAllStringSubmatch(js, -1)
	require.NotEmpty(t, sites, "no drain_cell call site on the page: the loop below would pass over nothing")
	for _, m := range sites {
		require.Containsf(t, m[1], "wait_seconds",
			"a drain_cell call site passes no wait_seconds (args: %s). llama-swap's SIGTERM cancels in-flight "+
				"streams immediately, so that one click truncates whatever is generating.", strings.TrimSpace(m[1]))
	}
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
// COMPUTED for the containment claim. "The panel lives outside the tbody
// render() rebuilds" is a fact about the DOM, so it is derived from the
// served markup's nesting rather than asserted by not finding a word
// inside a function body. The truncation half stays structural, but
// matches the SHAPE (`slice(0, 60)` at any spacing) rather than one
// spelling — the old NotContains of `text.slice(0, 60)` could not see
// `text.slice(0,60)`.
func TestFleetPage_ToolResultsSurviveTheNextRender(t *testing.T) {
	page, _, _ := livePageServer(t)
	js := pageScript(t, page)

	require.NotRegexp(t, regexp.MustCompile(`\.slice\(\s*0\s*,\s*60\s*\)`), js,
		"tool results are still truncated to 60 characters")
	require.NotRegexp(t, regexp.MustCompile(`\.title\s*=`), js,
		"tool results still go into a hover-only title the next render() destroys")
	require.Contains(t, page, `$("tr-body").textContent = text;`,
		"the full tool result is not rendered anywhere")
	require.Contains(t, page, `<pre id="tr-body">`, "the result area does not preserve newlines")

	// The containment claim, computed from the markup the server sent.
	// render() rebuilds $("cells").innerHTML, so anything INSIDE #cells is
	// destroyed 1.5s later by flash()'s tick() — which is the defect with
	// more markup rather than a fix for it.
	rebuilt := renderedContainerID(t, js)
	ancestors := elementAncestors(t, page, "toolresult")
	require.NotContains(t, ancestors, rebuilt,
		"#toolresult sits INSIDE #"+rebuilt+", the element render() replaces wholesale: the next refresh "+
			"destroys the result before anyone reads it, which is the defect this panel replaced")
	require.Contains(t, elementAncestors(t, page, "tr-body"), "toolresult",
		"#tr-body is not inside #toolresult, so dismissing the panel leaves its text on screen")
	require.Contains(t, page, "function dismissResult(", "the result panel cannot be dismissed")

	// The arithmetic that made the old field structurally unusable: the
	// warm reply's prefix is longer than the field it was written to.
	const prefix = "Warming %s through the front (JIT load started in the background). ETA from history: ~"
	require.Greater(t, len(prefix), 60,
		"this assertion is stale — the warm prefix now fits in 60 characters and the arithmetic argument no longer holds")
}
