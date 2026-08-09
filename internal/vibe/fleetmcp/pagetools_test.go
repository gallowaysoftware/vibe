package fleetmcp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
)

// The fleet page's buttons, checked against the tools that receive them.
//
// This lives here rather than beside the page because it is a WIRE
// CONTRACT between two packages: fleetmcp imports fleetapi, so only this
// side can see both the served markup and the schemas the tools declare.
// internal/vibe/fleetapi/pagelive_test.go's TestFleetPage_DrainWaitsAndAsks
// holds the page's half (that the drain button passes a wait at all, and
// asks first); this holds the half that the argument names it passes are
// names the receiving tool knows.
//
// The gap it closes is the one a same-package test structurally cannot:
// the drain button's `wait_seconds` was added to the page as a fix for a
// button that truncated live streams on one click. Nothing anywhere
// asserted that the tool on the other end still calls it `wait_seconds`.
// A rename in fleetmcp's schema would leave the page sending an argument
// that is silently dropped — the button reads as fixed, the drain is
// immediate again, and the operator is told their stream was safe.

// fleetPageURL is the page's public path. Hardcoded rather than imported
// because the constant is unexported in fleetapi; a moved page fails the
// 200 assertion below, which is the right outcome for a URL humans
// bookmark.
const fleetPageURL = "/ui/fleet"

// pageToolCall is one `runTool(btn, "name", { k: v, … })` site.
type pageToolCall struct {
	tool string
	args []string
	src  string
}

var pageToolCallRE = regexp.MustCompile(`runTool\([^,]+,\s*"([a-z_]+)"\s*,\s*\{([^}]*)\}\s*\)`)

// pageToolCalls finds every tools/call the page can issue. runTool is the
// single funnel — rpc() has exactly one caller — so enumerating its call
// sites enumerates the page's whole mutation surface.
//
// mk(label, tool, args) forwards to runTool, so its sites are matched by
// the same shape one level up.
func pageToolCalls(t *testing.T, page string) []pageToolCall {
	t.Helper()
	js := page
	if i := strings.Index(js, "<script>"); i >= 0 {
		js = js[i:]
	}
	// Comments first: this page discusses its own tool calls in prose.
	js = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(js, "")
	js = regexp.MustCompile(`(?m)^\s*//.*$`).ReplaceAllString(js, "")

	shapes := []*regexp.Regexp{
		pageToolCallRE,
		regexp.MustCompile(`mk\([^,]+,\s*"([a-z_]+)"\s*,\s*\{([^}]*)\}\s*\)`),
	}
	var out []pageToolCall
	for _, re := range shapes {
		for _, m := range re.FindAllStringSubmatch(js, -1) {
			c := pageToolCall{tool: m[1], src: strings.TrimSpace(m[0])}
			for _, part := range strings.Split(m[2], ",") {
				k, _, ok := strings.Cut(part, ":")
				if k = strings.TrimSpace(k); ok && k != "" {
					c.args = append(c.args, k)
				}
			}
			out = append(out, c)
		}
	}
	return out
}

// TestFleetPageToolCallsMatchTheToolSchemas: every tool the page can
// invoke exists, and every argument name it sends is one the receiving
// tool declares. Both directions of a rename fail here.
func TestFleetPageToolCallsMatchTheToolSchemas(t *testing.T) {
	dir := t.TempDir()
	fleet := fleetapi.New(
		[]fleetapi.Cell{{Name: "front", URL: "http://127.0.0.1:1", Class: "always_on"}},
		filepath.Join(dir, "history.json"),
		func() fleetapi.DaemonInfo { return fleetapi.DaemonInfo{} },
		fleetapi.Options{IntentPath: filepath.Join(dir, "intent.json")},
	)
	t.Cleanup(fleet.Close)
	mux := http.NewServeMux()
	fleet.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + fleetPageURL)
	if err != nil {
		t.Fatalf("GET %s: %v", fleetPageURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d — the page has moved, and every bookmark and every guard that reads it now "+
			"reads nothing", fleetPageURL, resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	calls := pageToolCalls(t, string(raw))
	if len(calls) == 0 {
		t.Fatal("no runTool call sites found in the served page: this guard is checking nothing")
	}
	// The button this test exists for. Named explicitly so a page that
	// stopped offering a drain (or renamed the tool) cannot make this
	// guard vacuously green.
	var sawDrain bool

	// Every tool this facade advertises, by name, with its declared
	// argument names.
	props := map[string]map[string]bool{}
	for _, d := range (&Server{}).toolDefs() {
		names := map[string]bool{}
		if p, ok := d.InputSchema["properties"].(map[string]any); ok {
			for k := range p {
				names[k] = true
			}
		}
		props[d.Name] = names
	}

	for _, c := range calls {
		declared, ok := props[c.tool]
		if !ok {
			t.Errorf("the page can invoke %q, which this facade does not advertise: the button returns an "+
				"'unknown tool' error the operator reads as a fleet failure. Site: %s", c.tool, c.src)
			continue
		}
		if c.tool == "drain_cell" {
			sawDrain = true
			if !slices.Contains(c.args, "wait_seconds") {
				t.Errorf("the page's drain button passes no wait_seconds (%v). llama-swap's SIGTERM cancels "+
					"in-flight streams immediately, so one click truncates whatever is generating — the "+
					"tool's own description says never to tell an operator otherwise.", c.args)
			}
		}
		for _, a := range c.args {
			if !declared[a] {
				t.Errorf("the page sends %q.%s, which is not in that tool's inputSchema. An argument the "+
					"schema does not declare is DROPPED, silently: the button still reports success and "+
					"the thing the argument was for does not happen. Site: %s", c.tool, a, c.src)
			}
		}
	}
	if !sawDrain {
		t.Error("no drain_cell call site on the page: the wait_seconds contract this test exists for is no " +
			"longer being checked against anything")
	}
}
