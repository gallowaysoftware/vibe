package benchreplay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/swaptest"
	"github.com/gallowaysoftware/vibe/internal/vibe/modelprobe"
	"github.com/gallowaysoftware/vibe/internal/vibe/observed"
	"github.com/stretchr/testify/require"
)

// fleet-control C25, unit gates U1-U7 and U10-U13.
//
// U1 and U2 come first because they are the privacy invariant and the rest
// of the phase is only safe if they hold.

// ── U1: Report cannot carry a body ──────────────────────────────────────

// reportFields is the COMPLETE field set of Report and everything
// reachable from it. Every entry is a count, an instant, a def name, a
// metric name from modelprobe's constants, or a string from a closed set
// declared in shape.go.
//
// Adding a field means editing this map, which is the point: it is a
// decision, not a diff. The technique is usagemeter's
// TestActivityRow_CannotCarryABody, applied to the type that leaves the
// package that reads captures.
var reportFields = map[string]bool{
	"Report.Cell": true, "Report.Sample": true, "Report.Incumbent": true,
	"Report.Candidate": true, "Report.Paired": true, "Report.Divergence": true,
	"Report.Rows": true,

	"SampleStats.Walked": true, "SampleStats.Candidates": true,
	"SampleStats.SkippedBasis": true, "SampleStats.Evicted": true,
	"SampleStats.Malformed": true, "SampleStats.Replayable": true,
	"SampleStats.Floor": true, "SampleStats.Refused": true,
	"SampleStats.RefusedStatus": true,

	"SideScore.Model": true, "SideScore.Metric": true, "SideScore.Requests": true,
	"SideScore.Answered": true, "SideScore.Failed": true, "SideScore.ToolsOffered": true,
	"SideScore.ToolCalled": true, "SideScore.ToolDeclared": true,
	"SideScore.ToolUndeclared": true, "SideScore.ArgsValidJSON": true,
	"SideScore.ArgsHaveRequired": true, "SideScore.SchemaAsked": true,
	"SideScore.SchemaConformed": true, "SideScore.FinishStop": true,
	"SideScore.FinishLength": true, "SideScore.FinishToolCalls": true,
	"SideScore.FinishFilter": true, "SideScore.FinishOther": true,
	"SideScore.FinishNone": true, "SideScore.Empty": true,
	"SideScore.Truncated": true, "SideScore.OutTokens": true,
	"SideScore.ToolCallRate": true, "SideScore.ArgsValidRate": true,
	"SideScore.FailureRate": true,

	"PairedScore.N": true, "PairedScore.MedianRatio": true, "PairedScore.Why": true,

	"DivergenceScore.N": true, "DivergenceScore.IncumbentDisagreed": true,
	"DivergenceScore.CandidateDisagreed": true, "DivergenceScore.Claimed": true,
	"DivergenceScore.Excess": true,

	"RequestScore.ActivityID": true, "RequestScore.IncumbentStatus": true,
	"RequestScore.CandidateStatus": true, "RequestScore.IncumbentTokS": true,
	"RequestScore.CandidateTokS": true, "RequestScore.IncumbentTool": true,
	"RequestScore.CandidateTool": true, "RequestScore.IncumbentFinish": true,
	"RequestScore.CandidateFinish": true,
}

// closedSetStrings names every string-typed field on the graph and the set
// it draws from. A string field that is not here fails the walk, because
// "a def name" and "whatever the model wrote" are the same type and only
// one of them may leave this package.
var closedSetStrings = map[string][]string{
	"Report.Cell":                  nil, // a cell name from the daemon config
	"SideScore.Model":              nil, // a def name
	"SideScore.Metric":             {modelprobe.MetricDecode, modelprobe.MetricE2E, metricMixed, ""},
	"PairedScore.Why":              {PairedOK, PairedMixedMetrics, PairedNoPairs, PairedUnderFloor},
	"RequestScore.IncumbentTool":   {ToolNone, ToolDeclared, ToolUndeclared},
	"RequestScore.CandidateTool":   {ToolNone, ToolDeclared, ToolUndeclared},
	"RequestScore.IncumbentFinish": {FinishStop, FinishLength, FinishToolCalls, FinishFilter, FinishOther, FinishNone},
	"RequestScore.CandidateFinish": {FinishStop, FinishLength, FinishToolCalls, FinishFilter, FinishOther, FinishNone},
}

func TestReportCannotCarryABody(t *testing.T) {
	seen := map[reflect.Type]bool{}
	var walk func(rt reflect.Type)
	walk = func(rt reflect.Type) {
		for rt.Kind() == reflect.Pointer || rt.Kind() == reflect.Slice || rt.Kind() == reflect.Array {
			if rt.Kind() == reflect.Slice && rt.Elem().Kind() == reflect.Uint8 {
				t.Errorf("%s is a []byte. A capture body is exactly that, and Report is the ONE thing that leaves this package", rt)
				return
			}
			rt = rt.Elem()
		}
		if rt.Kind() != reflect.Struct || seen[rt] {
			return
		}
		seen[rt] = true
		if rt.String() == "time.Time" {
			return // stdlib; cannot hold operator text
		}
		// observed.Value is this repo's own gated type (C20), but it is
		// GENERIC, and returning on the name alone was an escape hatch a
		// review found: an `observed.Value[string]` field would satisfy the
		// walk (its Kind is Struct, so the closed-set requirement below
		// never fires) and could carry a prompt. The parameter is what
		// matters, so it is the thing inspected.
		if strings.HasPrefix(rt.String(), "observed.Value[") {
			arg := rt.Field(0).Type // the unexported `v T`
			switch arg.Kind() {
			case reflect.Int, reflect.Int64, reflect.Float64, reflect.Bool:
			default:
				t.Errorf("observed.Value[%s] on this graph: the wrapper is gated, its type argument is not. "+
					"A Value[string] passes a walk that stops at the wrapper's name and can still carry a prompt", arg)
			}
			return
		}
		for i := range rt.NumField() {
			f := rt.Field(i)
			key := rt.Name() + "." + f.Name
			if !reportFields[key] {
				t.Errorf("%s %s is not in reportFields. Every field that leaves this package must be a count, an instant, "+
					"a def name, a metric name or a closed-set string; if it can hold a prompt, a completion, a tool "+
					"name or any other capture text, it does not belong on this graph (fleet-control C25 §3)", key, f.Type)
				continue
			}
			switch f.Type.Kind() {
			case reflect.Map, reflect.Interface, reflect.Chan, reflect.Func, reflect.UnsafePointer:
				t.Errorf("%s is a %s; a %s can hold anything, and this graph may hold only what reportFields names",
					key, f.Type.Kind(), f.Type.Kind())
			case reflect.String:
				if _, ok := closedSetStrings[key]; !ok {
					t.Errorf("%s is a string with no declared closed set. A string is how a prompt gets out; "+
						"declare its set in closedSetStrings or make it a count", key)
				}
			}
			walk(f.Type)
		}
	}
	walk(reflect.TypeOf(Report{}))

	if len(seen) < 5 {
		t.Fatalf("walked %d struct type(s); Report reaches five, so this walk is INERT", len(seen))
	}
}

// TestClosedSetStringsAreActuallyClosed is the other half of U1: the
// allowlist above is a claim about VALUES, and a type test alone cannot
// check it. Every producer of one of those strings is exercised here.
func TestClosedSetStringsAreActuallyClosed(t *testing.T) {
	inSet := func(key, got string) {
		t.Helper()
		set := closedSetStrings[key]
		if set == nil {
			return
		}
		for _, want := range set {
			if got == want {
				return
			}
		}
		t.Errorf("%s produced %q, which is outside its declared set %v — that is capture-derived text on the loose", key, got, set)
	}
	// normalizeFinish is the gate on finish_reason, which arrives as a
	// free string from whatever engine answered.
	for _, in := range []string{"stop", "length", "tool_calls", "content_filter", "",
		"STOP", "  length  ", "end_turn", "function_call",
		"my prompt was: hunter2", "\x00\x01", strings.Repeat("x", 4096)} {
		inSet("RequestScore.IncumbentFinish", normalizeFinish(in))
	}
	// The tool outcome, including a model that invented a name.
	facts, ok := extractRequest([]byte(toolRequest("SECRET", 64)))
	require.True(t, ok)
	for _, name := range []string{"read_file", "rm_minus_rf", "SECRET-as-a-tool-name"} {
		sh := shapeOf(200, []byte(toolResponse(name, `{"path":"x"}`, 4, 10)), facts)
		inSet("RequestScore.IncumbentTool", sh.toolOutcome)
	}
	inSet("RequestScore.IncumbentTool", shapeOf(200, []byte(proseResponse("hi", "stop", 2, 10)), facts).toolOutcome)
}

// TestSetCannotBeSerialized pins mechanism 1's other half: the harvested
// sample is not a value anybody can write down.
func TestSetCannotBeSerialized(t *testing.T) {
	rt := reflect.TypeOf(Set{})
	for i := range rt.NumField() {
		if f := rt.Field(i); f.IsExported() {
			t.Errorf("Set.%s is exported; the sample must not be reachable from outside this package", f.Name)
		}
	}
	f := newFakeCell(t)
	f.row(9, "incumbent", "/v1/chat/completions", 200, toolRequest("PRIVATE-PROMPT-MARKER", 64), "")
	set, err := Harvest(t.Context(), Options{LlamaSwapURL: f.url}, "incumbent", false)
	require.NoError(t, err)
	// staticcheck's SA9005 ("no exported fields, nor custom marshaling") is
	// not a warning here — it is the property under test, reported by the
	// linter as well as by the assertion below.
	b, err := json.Marshal(set) //nolint:staticcheck // SA9005 IS the guarantee: a Set must not be serializable.
	require.NoError(t, err)
	require.Equal(t, "{}", string(b), "a marshalled Set must be empty; a sample that can be written down is a sample that can be leaked")
}

// ── U2: the package writes no file ──────────────────────────────────────

// osReadersAllowed is every os function this package may call. Everything
// else in `os` — and every writer anywhere — is refused, because the leak
// this prevents is one line long and looks helpful:
//
//	// cache the sample so a resume doesn't re-harvest
var osReadersAllowed = map[string]bool{"ReadFile": true, "Stat": true, "Getenv": true, "IsNotExist": true}

var writerPackagesForbidden = map[string]string{
	"os/exec":  "shelling out is a second process with the sample's bytes in its argv",
	"log":      "a log line is a file, and llama-swap's own error strings are built from the upstream's body",
	"log/slog": "same",
	"bufio":    "the only reason to buffer here is to write",
}

func TestPackageWritesNoFile(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	files := 0
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		files++
		file, perr := parser.ParseFile(fset, n, nil, 0)
		require.NoError(t, perr)
		for _, imp := range file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if why, bad := writerPackagesForbidden[path]; bad {
				t.Errorf("%s imports %q: %s (fleet-control C25 §3, mechanism 2)", n, path, why)
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			sel, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "os" {
				return true
			}
			if !osReadersAllowed[sel.Sel.Name] {
				t.Errorf("%s:%d calls os.%s. This package holds the verbatim prompts and completions of real "+
					"requests and it must not be able to put them anywhere: it reads one config key and "+
					"otherwise touches no filesystem (fleet-control C25 §3, mechanism 2)",
					n, fset.Position(sel.Pos()).Line, sel.Sel.Name)
			}
			return true
		})
	}
	if files < 4 {
		t.Fatalf("examined %d production file(s); this package has five, so the scan is INERT", files)
	}
}

// ── U3: a marker in a capture reaches no output ─────────────────────────

const marker = "PRIVATE-PROMPT-9f3a1c my ssh key is hunter2 and the patient's name is"

// TestCaptureTextReachesNoOutput drives a capture whose every text field is
// the marker through the SUCCESS path and through every refusal path, and
// asserts the marker appears in none of: the rendered report, the
// marshalled report, the marshalled Set, or any error string.
func TestCaptureTextReachesNoOutput(t *testing.T) {
	f := newFakeCell(t)
	f.warm("incumbent", "candidate")
	for id := int64(1); id <= 4; id++ {
		f.row(id, "incumbent", "/v1/chat/completions", 200,
			toolRequest(marker, 64),
			streamedToolResponse(marker, `{"path":`, `"`+marker+`"}`, "tool_calls", 12))
	}
	// The candidate invents a tool name made of the marker, and answers
	// prose made of it — model output is private traffic too.
	f.chat = func(model string, n int) scripted {
		if model == "candidate" {
			if n%2 == 0 {
				return scripted{200, toolResponse(marker+"-tool", `{"`+marker+`":1}`, 9, 22)}
			}
			return scripted{200, proseResponse(marker, "stop", 9, 22)}
		}
		return scripted{200, toolResponse("read_file", `{"path":"x"}`, 10, 20)}
	}

	set, err := Harvest(t.Context(), Options{LlamaSwapURL: f.url}, "incumbent", false)
	require.NoError(t, err)
	rep, err := set.Run(t.Context(), "gpu-cell",
		Side{Model: "incumbent"}, Side{Model: "candidate"})
	require.NoError(t, err)

	var out bytes.Buffer
	rep.Render(&out)
	require.NotEmpty(t, out.String())
	requireNoMarker(t, "the rendered report", out.String())

	repJSON, err := json.Marshal(rep)
	require.NoError(t, err)
	requireNoMarker(t, "the marshalled report (this is what a journal stores)", string(repJSON))

	setJSON, err := json.Marshal(set) //nolint:staticcheck // SA9005: see TestSetCannotBeSerialized.
	require.NoError(t, err)
	requireNoMarker(t, "the marshalled sample", string(setJSON))

	// The report's own caveats are computed strings; they are output too.
	requireNoMarker(t, "the caveats", strings.Join(rep.Caveats(), "\n"))

	// And every refusal path. Each is driven for real, and each error
	// string is checked — including the one that reports a capture it
	// could not use, which is the path most likely to quote the thing.
	t.Run("refusals", func(t *testing.T) {
		g := newFakeCell(t)
		g.row(1, "incumbent", "/v1/chat/completions", 200, toolRequest(marker, 64), streamedProseResponse(marker, "stop", 3))
		// not resident
		s, err := Harvest(t.Context(), Options{LlamaSwapURL: g.url}, "incumbent", false)
		require.NoError(t, err)
		_, err = s.Run(t.Context(), "c", Side{Model: "incumbent"}, Side{Model: "candidate"})
		require.Error(t, err)
		requireNoMarker(t, "the not-resident refusal", err.Error())
		require.ErrorIs(t, err, ErrNotResident)

		// a warm that fails, with the far side's body in the error
		g.warm("incumbent")
		_, err = s.Run(t.Context(), "c", Side{Model: "incumbent", Warm: func(context.Context) error {
			return errors.New("HTTP 500: " + marker)
		}}, Side{Model: "candidate"})
		require.Error(t, err)
		require.Contains(t, err.Error(), marker,
			"this branch is the CONTROL: a caller-supplied warm error is the caller's string, not a capture, "+
				"so it must pass through — otherwise the assertions above would pass because nothing propagates at all")

		// captures off
		dir := t.TempDir()
		cfg := filepath.Join(dir, "config.yaml")
		require.NoError(t, os.WriteFile(cfg, []byte("captureBuffer: 0\nmodels:\n  a: {}\n"), 0o600))
		_, err = Harvest(t.Context(), Options{LlamaSwapURL: g.url, ConfigPath: cfg}, "incumbent", false)
		require.ErrorIs(t, err, ErrCapturesDisabled)
		requireNoMarker(t, "the captures-disabled refusal", err.Error())

		// nothing to replay
		h := newFakeCell(t)
		h.rowWithoutCapture(7, "incumbent", "/v1/chat/completions")
		_, err = Harvest(t.Context(), Options{LlamaSwapURL: h.url}, "incumbent", false)
		require.ErrorIs(t, err, ErrNoCaptures)
		requireNoMarker(t, "the empty-buffer refusal", err.Error())

		// already applied
		_, err = Harvest(t.Context(), Options{LlamaSwapURL: g.url}, "incumbent", true)
		require.ErrorIs(t, err, ErrAlreadyApplied)
		requireNoMarker(t, "the already-applied refusal", err.Error())
	})
}

func requireNoMarker(t *testing.T, where, body string) {
	t.Helper()
	// Split, so an assertion cannot pass because the marker was chopped
	// into pieces that are each individually absent.
	for _, frag := range []string{marker, "hunter2", "patient's name", "PRIVATE-PROMPT-9f3a1c"} {
		if strings.Contains(body, frag) {
			t.Fatalf("%s contains %q. A capture holds the verbatim prompt and completion of a real request; "+
				"this package emits only scores (fleet-control C25 §3, mechanism 4)", where, frag)
		}
	}
}

// ── U4: an undeclared tool name is never echoed ─────────────────────────

func TestUndeclaredToolNameIsReportedAndNeverEchoed(t *testing.T) {
	facts, ok := extractRequest([]byte(toolRequest("prompt", 64)))
	require.True(t, ok)

	cases := []struct {
		name string
		call string
		want string
	}{
		{"the tool the request declared", "read_file", ToolDeclared},
		{"one it never declared", "delete_everything", ToolUndeclared},
		{"a name that is itself operator text", "read_file_" + marker, ToolUndeclared},
		{"an empty name", "", ToolUndeclared},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sh := shapeOf(200, []byte(toolResponse(tc.call, `{"path":"a"}`, 4, 10)), facts)
			require.Equal(t, tc.want, sh.toolOutcome)
		})
	}

	// And end to end: the marker name must not survive into the Report or
	// its rendering, while still being COUNTED.
	f := newFakeCell(t)
	f.warm("incumbent", "candidate")
	f.row(1, "incumbent", "/v1/chat/completions", 200, toolRequest("p", 64), "")
	f.row(2, "incumbent", "/v1/chat/completions", 200, toolRequest("p", 64), "")
	f.chat = func(model string, _ int) scripted {
		if model == "candidate" {
			return scripted{200, toolResponse("hallucinated_"+marker, `{"path":"a"}`, 4, 10)}
		}
		return scripted{200, toolResponse("read_file", `{"path":"a"}`, 4, 10)}
	}
	set, err := Harvest(t.Context(), Options{LlamaSwapURL: f.url}, "incumbent", false)
	require.NoError(t, err)
	rep, err := set.Run(t.Context(), "c", Side{Model: "incumbent"}, Side{Model: "candidate"})
	require.NoError(t, err)

	require.Equal(t, 2, rep.Candidate.ToolUndeclared, "the undeclared calls must be COUNTED; a gate that passed because nothing was measured proves nothing")
	require.Equal(t, 2, rep.Incumbent.ToolDeclared)
	var out bytes.Buffer
	rep.Render(&out)
	requireNoMarker(t, "the report", out.String())
	require.Contains(t, out.String(), ToolUndeclared, "the marker for an undeclared name must be printed; only the NAME is withheld")
}

// ── U6: the noise floor suppresses the divergence claim ─────────────────

func TestNoiseFloorSuppressesTheDivergenceClaim(t *testing.T) {
	// Four samples. The RECORDED responses all call read_file. The
	// incumbent replay disagrees with its own recorded output twice
	// (temperature: a model does not reproduce itself). The candidate
	// disagrees the same number of times — so it is inside the noise and
	// there is NO claim.
	build := func(t *testing.T, candidateDisagreements int) *Report {
		t.Helper()
		f := newFakeCell(t)
		f.warm("incumbent", "candidate")
		for id := int64(1); id <= 4; id++ {
			f.row(id, "incumbent", "/v1/chat/completions", 200, toolRequest("p", 64),
				streamedToolResponse("read_file", `{"pa`, `th":"x"}`, "tool_calls", 6))
		}
		f.chat = func(model string, n int) scripted {
			disagreements := 2
			if model == "candidate" {
				disagreements = candidateDisagreements
			}
			if n < disagreements {
				return scripted{200, proseResponse("no tool for you", "stop", 5, 10)}
			}
			return scripted{200, toolResponse("read_file", `{"path":"x"}`, 6, 10)}
		}
		// RateFloor 4 so the divergence n (4) clears its OWN floor: the
		// suppression under test is the NOISE floor, and a fixture that
		// tripped the n floor instead would pass for the wrong reason.
		set, err := Harvest(t.Context(), Options{LlamaSwapURL: f.url, RateFloor: 4}, "incumbent", false)
		require.NoError(t, err)
		rep, err := set.Run(t.Context(), "c", Side{Model: "incumbent"}, Side{Model: "candidate"})
		require.NoError(t, err)
		return rep
	}

	t.Run("at the floor there is no claim", func(t *testing.T) {
		rep := build(t, 2)
		require.Equal(t, 4, rep.Divergence.N, "the floor has to be measured on a real n, or the suppression is vacuous")
		require.Equal(t, 2, rep.Divergence.IncumbentDisagreed)
		require.Equal(t, 2, rep.Divergence.CandidateDisagreed)
		require.False(t, rep.Divergence.Claimed)
		require.False(t, rep.Divergence.Excess.IsKnown(), "an excess nobody computed must be unknown, never 0")
		var out bytes.Buffer
		rep.Render(&out)
		require.Contains(t, out.String(), "NO")
		require.NotContains(t, out.String(), "ABOVE the floor")
	})

	t.Run("below the floor there is no claim either", func(t *testing.T) {
		rep := build(t, 1)
		require.False(t, rep.Divergence.Claimed)
	})

	t.Run("above the floor the claim is the EXCESS, not the raw rate", func(t *testing.T) {
		rep := build(t, 4)
		require.True(t, rep.Divergence.Claimed)
		excess, known := rep.Divergence.Excess.Observed()
		require.True(t, known)
		require.InDelta(t, 0.5, excess, 0.001, "4/4 against a floor of 2/4 is an excess of 0.5, not a divergence of 1.0")
	})
}

// TestEachRateIsGatedOnItsOwnDenominator is the review finding that the
// rate floor was applied to the SAMPLE SIZE while the rates divide by
// something else.
//
// In ordinary chat traffic most requests declare no tools at all, so a
// 40-sample run can compute `ToolCalled / ToolsOffered` over five
// requests and print it wearing a forty-sample floor's authority. That is
// D1's defect one field over.
func TestEachRateIsGatedOnItsOwnDenominator(t *testing.T) {
	f := newFakeCell(t)
	f.warm("incumbent", "candidate")
	const n = 6
	// Six samples, but only two of them declare tools.
	for id := int64(1); id <= n; id++ {
		body := plainRequest("p")
		if id <= 2 {
			body = toolRequest("p", 64)
		}
		f.row(id, "incumbent", "/v1/chat/completions", 200, body, "")
	}
	f.chat = func(string, int) scripted {
		return scripted{200, toolResponse("read_file", `{"path":"a"}`, 4, 10)}
	}
	// A floor the SAMPLE clears and the tool denominator does not.
	set, err := Harvest(t.Context(), Options{LlamaSwapURL: f.url, RateFloor: n}, "incumbent", false)
	require.NoError(t, err)
	rep, err := set.Run(t.Context(), "c", Side{Model: "incumbent"}, Side{Model: "candidate"})
	require.NoError(t, err)

	require.Equal(t, n, rep.Candidate.Requests)
	require.Equal(t, 2, rep.Candidate.ToolsOffered)
	require.True(t, rep.Candidate.FailureRate.IsKnown(),
		"the failure rate divides by Requests, which clears the floor")
	require.False(t, rep.Candidate.ToolCallRate.IsKnown(),
		"the tool-call rate divides by ToolsOffered (2), not by the sample size (6): two-of-two must not be printed as a rate")

	var out bytes.Buffer
	rep.Render(&out)
	require.Contains(t, out.String(), "unknown")
}

// TestTheDivergenceExcessHasItsOwnFloor is the second half of the same
// finding. d.N is routinely far below the sample size — non-200 rows and
// unreducible bodies drop out, and the report has a caveat saying so — so
// without a floor of its own, one sample where the incumbent agreed and
// the candidate did not prints "100 points ABOVE the floor".
func TestTheDivergenceExcessHasItsOwnFloor(t *testing.T) {
	build := func(t *testing.T, n, floor int) *Report {
		t.Helper()
		f := newFakeCell(t)
		f.warm("incumbent", "candidate")
		for id := 1; id <= n; id++ {
			f.row(int64(id), "incumbent", "/v1/chat/completions", 200, toolRequest("p", 64),
				streamedToolResponse("read_file", `{"pa`, `th":"x"}`, "tool_calls", 6))
		}
		f.chat = func(model string, _ int) scripted {
			if model == "candidate" {
				return scripted{200, proseResponse("no tool for you", "stop", 5, 10)}
			}
			return scripted{200, toolResponse("read_file", `{"path":"x"}`, 6, 10)}
		}
		set, err := Harvest(t.Context(), Options{LlamaSwapURL: f.url, RateFloor: floor}, "incumbent", false)
		require.NoError(t, err)
		rep, err := set.Run(t.Context(), "c", Side{Model: "incumbent"}, Side{Model: "candidate"})
		require.NoError(t, err)
		return rep
	}

	// One sample, incumbent agreeing, candidate disagreeing: the raw
	// arithmetic is a 100-point excess, and it is not printed.
	under := build(t, 1, 4)
	require.Equal(t, 1, under.Divergence.N)
	require.Equal(t, 1, under.Divergence.CandidateDisagreed, "the disagreement is COUNTED; only the proportion is withheld")
	require.False(t, under.Divergence.Claimed)
	require.False(t, under.Divergence.Excess.IsKnown())
	var out bytes.Buffer
	under.Render(&out)
	require.NotContains(t, out.String(), "ABOVE the floor")

	// Four of the same, against a floor of four: now it is claimed, so the
	// suppression above is the floor firing rather than the fixture being
	// broken.
	at := build(t, 4, 4)
	require.Equal(t, 4, at.Divergence.N)
	require.True(t, at.Divergence.Claimed)
	require.True(t, at.Divergence.Excess.IsKnown())
}

func TestDivergenceIsNotMeasuredWhenTheRecordedResponseCannotBeReduced(t *testing.T) {
	f := newFakeCell(t)
	f.warm("incumbent", "candidate")
	// A non-200 row: llama-swap stores the request and never the response
	// body. The most interesting request in a sample, and the one with
	// nothing to diverge from.
	f.row(1, "incumbent", "/v1/chat/completions", 500, toolRequest("p", 64), "")
	f.row(2, "incumbent", "/v1/chat/completions", 200, toolRequest("p", 64), "")
	set, err := Harvest(t.Context(), Options{LlamaSwapURL: f.url}, "incumbent", false)
	require.NoError(t, err)
	rep, err := set.Run(t.Context(), "c", Side{Model: "incumbent"}, Side{Model: "candidate"})
	require.NoError(t, err)
	require.Equal(t, 0, rep.Divergence.N)
	require.False(t, rep.Divergence.Claimed)
	var out bytes.Buffer
	rep.Render(&out)
	require.Contains(t, out.String(), "NOT MEASURED",
		"a divergence nobody could measure must say so; rendering it as agreement is the absent-evidence class")
}

// ── U7: every refusal fires by name and writes no config ────────────────

func TestEveryRefusalFiresByNameAndWritesNothing(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "llama-swap.yaml")
	const original = "captureBuffer: 0\nmodels:\n  incumbent:\n    cmd: llama-server\n"
	require.NoError(t, os.WriteFile(cfg, []byte(original), 0o600))

	t.Run("captureBuffer: 0 names the key", func(t *testing.T) {
		f := newFakeCell(t)
		f.row(1, "incumbent", "/v1/chat/completions", 200, plainRequest("p"), "")
		_, err := Harvest(t.Context(), Options{LlamaSwapURL: f.url, ConfigPath: cfg}, "incumbent", false)
		require.ErrorIs(t, err, ErrCapturesDisabled)
		require.Contains(t, err.Error(), "captureBuffer: 0")
	})

	t.Run("and the config is not written", func(t *testing.T) {
		after, err := os.ReadFile(cfg)
		require.NoError(t, err)
		require.Equal(t, original, string(after),
			"a benchmark that turned captures on would have taken an intent decision that belongs to the operator")
	})

	t.Run("no rows at all", func(t *testing.T) {
		f := newFakeCell(t)
		_, err := Harvest(t.Context(), Options{LlamaSwapURL: f.url}, "incumbent", false)
		require.ErrorIs(t, err, ErrNoCaptures)
	})

	t.Run("rows for another model only", func(t *testing.T) {
		f := newFakeCell(t)
		f.row(1, "somebody-else", "/v1/chat/completions", 200, plainRequest("p"), "")
		_, err := Harvest(t.Context(), Options{LlamaSwapURL: f.url}, "incumbent", false)
		require.ErrorIs(t, err, ErrNoCaptures)
	})

	t.Run("an embedding row is dropped by basis, not replayed", func(t *testing.T) {
		f := newFakeCell(t)
		f.row(1, "incumbent", "/v1/embeddings", 200, `{"model":"incumbent","input":["x"]}`, "")
		_, err := Harvest(t.Context(), Options{LlamaSwapURL: f.url}, "incumbent", false)
		require.ErrorIs(t, err, ErrNoCaptures)
		require.Contains(t, err.Error(), "1 were not replayable chat requests")
	})

	// The review finding this covers: on a cell that keys its own
	// llama-swap — the case C15 exists for, and the one swapauth.go's own
	// comment says matters — EVERY capture fetch 401s, and folding that
	// into "there is nothing to replay" invents a fact about the
	// operator's workload out of a credential problem.
	t.Run("a refused fetch is not an idle box", func(t *testing.T) {
		keyed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/api/captures/") {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			writeJSON(w, map[string]any{
				"data": []fakeRow{
					{ID: 1, Model: "incumbent", ReqPath: "/v1/chat/completions", Status: 200, HasCapture: true},
					{ID: 2, Model: "incumbent", ReqPath: "/v1/chat/completions", Status: 200, HasCapture: true},
				},
				"page": 1, "limit": 999, "total": 2, "total_pages": 1,
			})
		}))
		t.Cleanup(keyed.Close)
		_, err := Harvest(t.Context(), Options{LlamaSwapURL: keyed.URL}, "incumbent", false)
		require.ErrorIs(t, err, ErrNoCaptures)
		require.Contains(t, err.Error(), "REFUSED")
		require.Contains(t, err.Error(), "401")
		require.Contains(t, err.Error(), "not about what the cell has served",
			"a 401 on every fetch says what this process could READ; rendering it as a fact about the workload is the absent-evidence class")
	})

	// The other half of the same finding: an endpoint this package cannot
	// rebuild a request for is not a request the MODEL failed.
	t.Run("a chat-basis path the replay cannot rebuild is skipped, not failed", func(t *testing.T) {
		for _, path := range []string{"/v1/completions", "/infill", "/completion", "/v1/responses", "/v1/messages"} {
			f := newFakeCell(t)
			f.row(1, "incumbent", path, 200, plainRequest("p"), "")
			_, err := Harvest(t.Context(), Options{LlamaSwapURL: f.url}, "incumbent", false)
			require.ErrorIs(t, err, ErrNoCaptures, "%s must not be harvested", path)
			require.Contains(t, err.Error(), "1 were not replayable chat requests", "%s", path)
		}
		// And the two spellings that ARE replayable still are, or the
		// filter above would be refusing the whole feature.
		for _, path := range []string{"/v1/chat/completions", "/v/chat/completions"} {
			f := newFakeCell(t)
			f.row(1, "incumbent", path, 200, plainRequest("p"), "")
			set, err := Harvest(t.Context(), Options{LlamaSwapURL: f.url}, "incumbent", false)
			require.NoError(t, err, "%s", path)
			require.Equal(t, 1, set.Len())
		}
	})

	t.Run("an unreadable config is UNKNOWN, never enabled", func(t *testing.T) {
		require.False(t, CapturesDisabled(filepath.Join(dir, "nope.yaml")).IsKnown())
		require.False(t, CapturesDisabled("").IsKnown())
		// An absent captureBuffer is upstream's 10 MB default, which is ON.
		on := filepath.Join(dir, "on.yaml")
		require.NoError(t, os.WriteFile(on, []byte("models:\n  a: {}\n"), 0o600))
		v, known := CapturesDisabled(on).Observed()
		require.True(t, known)
		require.False(t, v)
	})

	t.Run("a non-resident model is refused, and no request is issued", func(t *testing.T) {
		f := newFakeCell(t)
		f.row(1, "incumbent", "/v1/chat/completions", 200, plainRequest("p"), "")
		set, err := Harvest(t.Context(), Options{LlamaSwapURL: f.url}, "incumbent", false)
		require.NoError(t, err)
		_, err = set.Run(t.Context(), "c", Side{Model: "incumbent"}, Side{Model: "candidate"})
		require.ErrorIs(t, err, ErrNotResident)
		require.Empty(t, f.sentBodies("incumbent"), "a replay must not load a model, so it must not have sent one either")
	})

	t.Run("an unreadable /running is a refusal, never a yes", func(t *testing.T) {
		f := newFakeCell(t)
		f.row(1, "incumbent", "/v1/chat/completions", 200, plainRequest("p"), "")
		set, err := Harvest(t.Context(), Options{LlamaSwapURL: f.url}, "incumbent", false)
		require.NoError(t, err)
		// Point the replay at an address nothing can bind.
		set.opt.LlamaSwapURL = "http://127.0.0.1:1"
		_, err = set.Run(t.Context(), "c", Side{Model: "incumbent"}, Side{Model: "candidate"})
		require.Error(t, err)
		require.Contains(t, err.Error(), "must not load a model")
	})
}

// ── U10: median of ratios, and no ratio across metrics ──────────────────

func TestPairedRatioIsAMedianOfRatiosNotARatioOfMeans(t *testing.T) {
	f := newFakeCell(t)
	f.warm("incumbent", "candidate")
	for id := int64(1); id <= 5; id++ {
		f.row(id, "incumbent", "/v1/chat/completions", 200, plainRequest("p"), "")
	}
	// Four ordinary requests where the candidate is 2x, and ONE enormous
	// one where it is 0.1x. A ratio of means would be dominated by the
	// giant request's token count; the median of per-request ratios is 2.
	f.chat = func(model string, n int) scripted {
		giant := n == 0
		switch {
		case model == "incumbent" && giant:
			return scripted{200, proseResponse("x", "stop", 100000, 10)}
		case model == "candidate" && giant:
			return scripted{200, proseResponse("x", "stop", 100000, 1)}
		case model == "incumbent":
			return scripted{200, proseResponse("x", "stop", 10, 10)}
		default:
			return scripted{200, proseResponse("x", "stop", 10, 20)}
		}
	}
	set, err := Harvest(t.Context(), Options{LlamaSwapURL: f.url}, "incumbent", false)
	require.NoError(t, err)
	rep, err := set.Run(t.Context(), "c", Side{Model: "incumbent"}, Side{Model: "candidate"})
	require.NoError(t, err)

	require.Equal(t, PairedOK, rep.Paired.Why)
	require.Equal(t, 5, rep.Paired.N)
	got, known := rep.Paired.MedianRatio.Observed()
	require.True(t, known)
	require.InDelta(t, 2.0, got, 0.0001,
		"the median of {0.1, 2, 2, 2, 2} is 2. A ratio of MEANS would be about 0.5, decided entirely by one request")

	// The figure a ratio-of-means would have produced, computed here so
	// the assertion above is a DISCRIMINATION rather than a coincidence:
	// sum(candidate rates)/sum(incumbent rates) = (1+20*4)/(10+10*4) =
	// 1.62, and a token-weighted mean is dominated by the giant request
	// and lands near 0.1. Neither is 2.
	meanOfRatios := (1 + 20*4) / float64(10+10*4)
	require.Greater(t, math.Abs(2.0-meanOfRatios), 0.3,
		"if the mean and the median agreed on this fixture the assertion above would prove nothing")
}

func TestNoRatioWhenTheTwoSidesUsedDifferentMetrics(t *testing.T) {
	f := newFakeCell(t)
	f.warm("incumbent", "candidate")
	for id := int64(1); id <= 3; id++ {
		f.row(id, "incumbent", "/v1/chat/completions", 200, plainRequest("p"), "")
	}
	// The candidate's engine reports no timings block, so its rate is
	// wall-clock. decode_tok_s excludes queueing and e2e_tok_s does not;
	// one column for both would manufacture a regression out of a parser
	// difference.
	f.chat = func(model string, _ int) scripted {
		if model == "candidate" {
			return scripted{200, proseResponseNoTimings("x", "stop", 10)}
		}
		return scripted{200, proseResponse("x", "stop", 10, 10)}
	}
	set, err := Harvest(t.Context(), Options{LlamaSwapURL: f.url}, "incumbent", false)
	require.NoError(t, err)
	rep, err := set.Run(t.Context(), "c", Side{Model: "incumbent"}, Side{Model: "candidate"})
	require.NoError(t, err)

	require.Equal(t, modelprobe.MetricDecode, rep.Incumbent.Metric)
	require.Equal(t, modelprobe.MetricE2E, rep.Candidate.Metric)
	require.Equal(t, PairedMixedMetrics, rep.Paired.Why)
	require.False(t, rep.Paired.MedianRatio.IsKnown(), "an unmeasurable ratio must be unknown, never 1.0")
	var out bytes.Buffer
	rep.Render(&out)
	require.Contains(t, out.String(), "NO ratio")
}

func TestASideThatMixedItsOwnMetricsIsNotComparable(t *testing.T) {
	f := newFakeCell(t)
	f.warm("incumbent", "candidate")
	for id := int64(1); id <= 3; id++ {
		f.row(id, "incumbent", "/v1/chat/completions", 200, plainRequest("p"), "")
	}
	f.chat = func(_ string, n int) scripted {
		if n == 0 {
			return scripted{200, proseResponseNoTimings("x", "stop", 10)}
		}
		return scripted{200, proseResponse("x", "stop", 10, 10)}
	}
	set, err := Harvest(t.Context(), Options{LlamaSwapURL: f.url}, "incumbent", false)
	require.NoError(t, err)
	rep, err := set.Run(t.Context(), "c", Side{Model: "incumbent"}, Side{Model: "candidate"})
	require.NoError(t, err)
	require.Equal(t, metricMixed, rep.Incumbent.Metric)
	require.Equal(t, PairedMixedMetrics, rep.Paired.Why)
	require.False(t, rep.Paired.MedianRatio.IsKnown())
}

// ── U11: n = 0 is a refusal; under the floor there is a table, no rate ──

func TestBelowTheFloorThereIsATableAndNoRate(t *testing.T) {
	f := newFakeCell(t)
	f.warm("incumbent", "candidate")
	for id := int64(1); id <= 3; id++ {
		f.row(id, "incumbent", "/v1/chat/completions", 200, toolRequest("p", 64), "")
	}
	f.chat = func(string, int) scripted {
		return scripted{200, toolResponse("read_file", `{"path":"a"}`, 4, 10)}
	}
	set, err := Harvest(t.Context(), Options{LlamaSwapURL: f.url}, "incumbent", false)
	require.NoError(t, err)
	rep, err := set.Run(t.Context(), "c", Side{Model: "incumbent"}, Side{Model: "candidate"})
	require.NoError(t, err)

	require.Equal(t, 3, rep.Sample.Replayable)
	require.Equal(t, DefaultRateFloor, rep.Sample.Floor)
	require.Len(t, rep.Rows, 3, "under the floor the per-request rows are the answer")
	// Every rate unknown — and every COUNT still present, so the test is
	// not passing because nothing was measured.
	require.False(t, rep.Candidate.ToolCallRate.IsKnown())
	require.False(t, rep.Candidate.ArgsValidRate.IsKnown())
	require.False(t, rep.Candidate.FailureRate.IsKnown())
	require.Equal(t, 3, rep.Candidate.ToolCalled)

	// The paired SCALAR has its own, smaller floor (C8's five), and three
	// is under that too — so no ratio either, and no "0% faster".
	require.Equal(t, PairedUnderFloor, rep.Paired.Why)
	require.False(t, rep.Paired.MedianRatio.IsKnown())

	var out bytes.Buffer
	rep.Render(&out)
	require.Contains(t, out.String(), "unknown")
	require.NotContains(t, out.String(), "0%",
		`a median of three ratios rendered as "0% faster" is a claim three requests decided`)
	require.Contains(t, out.String(), "below the rate floor")
}

func TestAtTheFloorTheRatesAppear(t *testing.T) {
	f := newFakeCell(t)
	f.warm("incumbent", "candidate")
	const n = 4
	for id := int64(1); id <= n; id++ {
		f.row(id, "incumbent", "/v1/chat/completions", 200, toolRequest("p", 64), "")
	}
	f.chat = func(model string, i int) scripted {
		// The candidate skips the tool on one of four.
		if model == "candidate" && i == 0 {
			return scripted{200, proseResponse("I would rather not", "stop", 4, 10)}
		}
		return scripted{200, toolResponse("read_file", `{"path":"a"}`, 4, 10)}
	}
	set, err := Harvest(t.Context(), Options{LlamaSwapURL: f.url, RateFloor: n}, "incumbent", false)
	require.NoError(t, err)
	rep, err := set.Run(t.Context(), "c", Side{Model: "incumbent"}, Side{Model: "candidate"})
	require.NoError(t, err)

	inc, known := rep.Incumbent.ToolCallRate.Observed()
	require.True(t, known)
	require.InDelta(t, 1.0, inc, 0.001)
	cand, known := rep.Candidate.ToolCallRate.Observed()
	require.True(t, known)
	require.InDelta(t, 0.75, cand, 0.001)
	require.Empty(t, rep.Rows, "at or above the floor the aggregate is the answer and the table is not printed")
}

// TestThePairedScalarHasItsOwnSmallerFloor pins the two-floor rule: C8's
// five for a paired SCALAR, twenty for a PROPORTION. Both directions, at
// the boundary, because a floor nobody has watched fire is a number.
func TestThePairedScalarHasItsOwnSmallerFloor(t *testing.T) {
	build := func(t *testing.T, n int) *Report {
		t.Helper()
		f := newFakeCell(t)
		f.warm("incumbent", "candidate")
		for id := 1; id <= n; id++ {
			f.row(int64(id), "incumbent", "/v1/chat/completions", 200, plainRequest("p"), "")
		}
		f.chat = func(model string, _ int) scripted {
			if model == "candidate" {
				return scripted{200, proseResponse("x", "stop", 10, 20)}
			}
			return scripted{200, proseResponse("x", "stop", 10, 10)}
		}
		set, err := Harvest(t.Context(), Options{LlamaSwapURL: f.url}, "incumbent", false)
		require.NoError(t, err)
		rep, err := set.Run(t.Context(), "c", Side{Model: "incumbent"}, Side{Model: "candidate"})
		require.NoError(t, err)
		return rep
	}

	under := build(t, PairedFloor-1)
	require.Equal(t, PairedUnderFloor, under.Paired.Why)
	require.Equal(t, PairedFloor-1, under.Paired.N, "the pairs are still counted; only the scalar is withheld")
	require.False(t, under.Paired.MedianRatio.IsKnown())

	at := build(t, PairedFloor)
	require.Equal(t, PairedOK, at.Paired.Why)
	got, known := at.Paired.MedianRatio.Observed()
	require.True(t, known)
	require.InDelta(t, 2.0, got, 0.0001)
	// And the PROPORTIONS are still unknown at PairedFloor, because their
	// floor is DefaultRateFloor and the two are different numbers.
	require.False(t, at.Candidate.FailureRate.IsKnown())
	require.Less(t, PairedFloor, DefaultRateFloor)
}

func TestZeroSamplesIsARefusalNotAScoreOfZero(t *testing.T) {
	f := newFakeCell(t)
	// Rows exist and every one advertises a capture; the buffer has
	// dropped all of them. That is EXACTLY the shape that must not
	// become "this model tool-called 0% of the time".
	for id := int64(1); id <= 5; id++ {
		f.rowWithoutCapture(id, "incumbent", "/v1/chat/completions")
	}
	set, err := Harvest(t.Context(), Options{LlamaSwapURL: f.url}, "incumbent", false)
	require.ErrorIs(t, err, ErrNoCaptures)
	require.Nil(t, set)
	require.Contains(t, err.Error(), "5 had been evicted")

	// And Run on an empty Set refuses rather than scoring an empty sample.
	var empty Set
	_, err = empty.Run(t.Context(), "c", Side{Model: "a"}, Side{Model: "b"})
	require.ErrorIs(t, err, ErrNoCaptures)
	var nilSet *Set
	require.Equal(t, 0, nilSet.Len())
}

// ── U12: an evicted capture is counted and never retried ────────────────

func TestAnEvictedCaptureIsCountedAndNeverRetried(t *testing.T) {
	f := newFakeCell(t)
	f.warm("incumbent", "candidate")
	f.row(1, "incumbent", "/v1/chat/completions", 200, plainRequest("p"), "")
	f.rowWithoutCapture(2, "incumbent", "/v1/chat/completions")
	f.row(3, "incumbent", "/v1/chat/completions", 200, plainRequest("p"), "")

	set, err := Harvest(t.Context(), Options{LlamaSwapURL: f.url}, "incumbent", false)
	require.NoError(t, err)
	require.Equal(t, 2, set.Len())
	require.Equal(t, 1, set.stats.Evicted)
	require.Equal(t, 3, set.stats.Candidates)
	require.Equal(t, 1, f.captureReads(2),
		"an evicted capture is read ONCE. Retrying asks for a row that is by definition older than the one that displaced it, and a harvest that spins on it never returns")

	rep, err := set.Run(t.Context(), "c", Side{Model: "incumbent"}, Side{Model: "candidate"})
	require.NoError(t, err)
	require.Equal(t, 1, rep.Sample.Evicted)
	var out bytes.Buffer
	rep.Render(&out)
	require.Contains(t, out.String(), "evicted",
		"the loss has to appear on the same screen as the number; an n that shrank silently is an n nobody can judge")
	require.NotContains(t, out.String(), "REFUSED",
		"nothing was refused here; a banner that fires when it should not is one nobody reads when it should")
}

// TestAPartiallyRefusedHarvestSaysSoOnTheReport is the mixed case the
// refusal string cannot cover: some fetches answered and some 401'd, so
// the harvest SUCCEEDS with a short n. Without the banner the report shows
// a denominator that shrank for a reason it never names, and the reason is
// a credential problem wearing a workload's clothes.
func TestAPartiallyRefusedHarvestSaysSoOnTheReport(t *testing.T) {
	f := newFakeCell(t)
	f.warm("incumbent", "candidate")
	f.row(1, "incumbent", "/v1/chat/completions", 200, plainRequest("p"), "")
	f.row(2, "incumbent", "/v1/chat/completions", 200, plainRequest("p"), "")
	// Row 3 advertises a capture the cell will not hand over.
	f.rowWithoutCapture(3, "incumbent", "/v1/chat/completions")
	f.refuse(3, http.StatusUnauthorized)

	set, err := Harvest(t.Context(), Options{LlamaSwapURL: f.url}, "incumbent", false)
	require.NoError(t, err)
	require.Equal(t, 2, set.Len())
	require.Equal(t, 1, set.stats.Refused)
	require.Equal(t, http.StatusUnauthorized, set.stats.RefusedStatus)
	require.Equal(t, 0, set.stats.Evicted, "a 401 is not an eviction; conflating them loses the only actionable half")

	rep, err := set.Run(t.Context(), "c", Side{Model: "incumbent"}, Side{Model: "candidate"})
	require.NoError(t, err)
	var out bytes.Buffer
	rep.Render(&out)
	require.Contains(t, out.String(), "REFUSED")
	require.Contains(t, out.String(), "401")
	require.Contains(t, out.String(), "not what this cell served")
}

// ── the harvest against the SHARED double ───────────────────────────────

// TestHarvestRunsAgainstTheSharedDouble drives the production fetch path
// against internal/swaptest's Cell, which models llama-swap's own activity
// rows and the real GET /api/captures/{id} route with upstream's status
// codes. The fake in fake_test.go scripts chat responses the shared double
// cannot; this is what keeps the WIRE half honest.
func TestHarvestRunsAgainstTheSharedDouble(t *testing.T) {
	for _, wire := range swaptest.Wires() {
		t.Run(wire.String(), func(t *testing.T) {
			cell := swaptest.NewCell(t, swaptest.WithWire(wire),
				swaptest.WithModels(swaptest.Model{ID: "incumbent", State: "ready", TTL: 900}))
			id := cell.Request("incumbent", "/v1/chat/completions",
				swaptest.Tokens{Input: 100, Output: 40, Cache: 0, Draft: -1, DraftAcc: -1}, 900*time.Millisecond)
			embedID := cell.Request("incumbent", "/v1/embeddings",
				swaptest.Tokens{Input: 24, Cache: -1, Draft: -1, DraftAcc: -1}, 30*time.Millisecond)
			// SYNTHETIC. Nothing in this repo may hold a real capture.
			cell.SetCapture(id, swaptest.Capture{
				ReqPath:  "/v1/chat/completions",
				ReqBody:  []byte(toolRequest("synthetic", 64)),
				RespBody: []byte(streamedToolResponse("read_file", `{"pa`, `th":"x"}`, "tool_calls", 6)),
			})
			cell.SetCapture(embedID, swaptest.Capture{ReqPath: "/v1/embeddings", ReqBody: []byte(`{"input":["x"]}`)})

			set, err := Harvest(t.Context(), Options{LlamaSwapURL: cell.URL()}, "incumbent", false)
			require.NoError(t, err)
			require.Equal(t, 1, set.Len(), "the chat row is harvested and the embedding row is dropped by basis")
			require.Equal(t, 1, set.stats.SkippedBasis)
			require.Equal(t, 1, cell.CaptureReads(), "the embedding row's capture must never be fetched at all")

			// The recorded SSE response reduced to a shape, including the
			// tool-call arguments joined across chunk boundaries.
			rec := set.items[0].recorded
			require.True(t, rec.known)
			require.True(t, rec.hasToolCall)
			require.Equal(t, ToolDeclared, rec.toolOutcome)
			require.True(t, rec.argsValid, "streamed tool-call ARGUMENTS concatenate by contract; streamed TEXT is what has no stable boundary")
			require.True(t, rec.argsRequired)

			// And the reload case: a fresh, empty buffer over the same
			// activity rows is what -watch-config produces, and it is
			// refused rather than scored.
			cell.DropAllCaptures()
			_, err = Harvest(t.Context(), Options{LlamaSwapURL: cell.URL()}, "incumbent", false)
			require.ErrorIs(t, err, ErrNoCaptures)
		})
	}
}

// ── the replay rewrites exactly two fields ──────────────────────────────

func TestReplayRewritesOnlyTheModelAndTheStreamFlag(t *testing.T) {
	f := newFakeCell(t)
	f.warm("incumbent", "candidate")
	f.row(1, "incumbent", "/v1/chat/completions", 200, toolRequest("p", 777), "")
	set, err := Harvest(t.Context(), Options{LlamaSwapURL: f.url}, "incumbent", false)
	require.NoError(t, err)
	_, err = set.Run(t.Context(), "c", Side{Model: "incumbent"}, Side{Model: "candidate"})
	require.NoError(t, err)

	sent := f.sentBodies("candidate")
	require.Len(t, sent, 1)
	var got map[string]any
	require.NoError(t, json.Unmarshal(sent[0], &got))

	require.Equal(t, "candidate", got["model"], "the model id must name the side being measured")
	require.Equal(t, false, got["stream"])
	require.NotContains(t, got, "stream_options", "stream_options is dropped with the stream flag it configures")

	// Everything else is the client's own. §5's argument against injecting
	// a seed applies to all of it: a sample you had to falsify is no
	// longer YOUR workload, which was the whole point of the phase.
	require.Equal(t, 0.7, got["temperature"], "the client's temperature is what makes the noise floor real")
	require.Equal(t, float64(777), got["max_tokens"])
	require.Contains(t, got, "tools")
	require.Contains(t, got, "messages")
	require.NotContains(t, got, "seed", "injecting a seed would edit the client's request; §5 refuses it by name")
}

// ── truncation and finish reasons ───────────────────────────────────────

func TestStructuralOutcomesAreCountedAgainstTheRequestsOwnBudget(t *testing.T) {
	facts, ok := extractRequest([]byte(toolRequest("p", 16)))
	require.True(t, ok)
	require.Equal(t, 16, facts.maxTokens)

	atBudget := shapeOf(200, []byte(proseResponse("x", "stop", 16, 10)), facts)
	require.True(t, atBudget.truncated, "a response that used the request's whole budget is truncation, whatever finish_reason says")

	byReason := shapeOf(200, []byte(proseResponse("x", "length", 4, 10)), facts)
	require.True(t, byReason.truncated)
	require.Equal(t, FinishLength, byReason.finish)

	empty := shapeOf(200, []byte(proseResponse("", "stop", 0, 10)), facts)
	require.True(t, empty.empty)

	// A tool call is not an empty response even with no content.
	tool := shapeOf(200, []byte(toolResponse("read_file", `{"path":"a"}`, 4, 10)), facts)
	require.False(t, tool.empty)
	require.True(t, tool.argsRequired)

	bad := shapeOf(200, []byte(toolResponse("read_file", `{"path": `, 4, 10)), facts)
	require.False(t, bad.argsValid)
	missing := shapeOf(200, []byte(toolResponse("read_file", `{"other":1}`, 4, 10)), facts)
	require.True(t, missing.argsValid)
	require.False(t, missing.argsRequired, "the request's own schema names `path` as required; a call without it is incomplete")
}

// TestSchemaConformanceIsGroundedInTheRequest is §5's fourth metric. The
// ground truth is again in the REQUEST — response_format is something the
// client asked for — so a model that ignored it is measurable without any
// reference to the recorded answer.
func TestSchemaConformanceIsGroundedInTheRequest(t *testing.T) {
	asked, ok := extractRequest([]byte(schemaRequest("prompt")))
	require.True(t, ok)
	require.True(t, asked.wantsSchema)

	conformed := shapeOf(200, []byte(proseResponse(`{"answer":1}`, "stop", 6, 10)), asked)
	require.True(t, conformed.schemaWanted)
	require.True(t, conformed.schemaOK)

	ignored := shapeOf(200, []byte(proseResponse("Sure! Here is the JSON you asked for:", "stop", 6, 10)), asked)
	require.True(t, ignored.schemaWanted)
	require.False(t, ignored.schemaOK)

	// A request that asked for nothing is not a request the model failed.
	plain, ok := extractRequest([]byte(plainRequest("prompt")))
	require.True(t, ok)
	require.False(t, plain.wantsSchema)
	require.False(t, shapeOf(200, []byte(proseResponse("hello", "stop", 2, 10)), plain).schemaWanted)

	// A STREAMED recorded response does not claim schema conformance at
	// all: judging it would need the text reassembled, and §5 refuses to
	// reassemble streamed text. Wanted is dropped rather than answered no.
	streamed := shapeOfRecorded(200, "text/event-stream", []byte(streamedProseResponse(`{"answer":1}`, "stop", 6)), asked)
	require.True(t, streamed.known)
	require.False(t, streamed.schemaWanted,
		"an unanswerable question is dropped, not answered `did not conform` — that would be a failure nobody measured")
}

func TestAnUnparseableResponseIsUnknownNotAnEmptyOne(t *testing.T) {
	facts, ok := extractRequest([]byte(plainRequest("p")))
	require.True(t, ok)
	for _, body := range []string{"", "not json", `{}`, `{"choices":[]}`} {
		sh := shapeOf(200, []byte(body), facts)
		require.False(t, sh.known,
			"a response nobody could read is not a response that finished with none, called no tool and produced no tokens — that is three claims from zero evidence (%q)", body)
	}
}

func TestAFailedRequestIsAFailureNotAFastZero(t *testing.T) {
	f := newFakeCell(t)
	f.warm("incumbent", "candidate")
	for id := int64(1); id <= 2; id++ {
		f.row(id, "incumbent", "/v1/chat/completions", 200, plainRequest("p"), "")
	}
	f.chat = func(model string, _ int) scripted {
		if model == "candidate" {
			return scripted{http.StatusInternalServerError, `{"error":{"message":"context length exceeded: ` + marker + `"}}`}
		}
		return scripted{200, proseResponse("x", "stop", 10, 10)}
	}
	set, err := Harvest(t.Context(), Options{LlamaSwapURL: f.url}, "incumbent", false)
	require.NoError(t, err)
	rep, err := set.Run(t.Context(), "c", Side{Model: "incumbent"}, Side{Model: "candidate"})
	require.NoError(t, err)

	require.Equal(t, 2, rep.Candidate.Failed)
	require.Equal(t, 0, rep.Candidate.Answered)
	require.Equal(t, 0, rep.Candidate.FinishStop, "a failed request must not land in a finish-reason bucket")
	require.Equal(t, PairedNoPairs, rep.Paired.Why)
	var out bytes.Buffer
	rep.Render(&out)
	requireNoMarker(t, "the report after a 500 whose body quoted the prompt", out.String())
	require.Contains(t, out.String(), "HTTP 500")
}

// ── the sample cap ──────────────────────────────────────────────────────

func TestTheSampleIsCappedAtTheNewestNAndSaysHowManyWereAvailable(t *testing.T) {
	f := newFakeCell(t)
	for id := int64(1); id <= 10; id++ {
		f.row(id, "incumbent", "/v1/chat/completions", 200, plainRequest("p"), "")
	}
	set, err := Harvest(t.Context(), Options{LlamaSwapURL: f.url, MaxSample: 3}, "incumbent", false)
	require.NoError(t, err)
	require.Equal(t, 3, set.Len())
	require.Equal(t, 10, set.stats.Candidates, "the number AVAILABLE is reported beside the number replayed; a cap that hid the denominator would let n look like the whole buffer")
	for _, item := range set.items {
		require.Contains(t, []int64{1, 2, 3}, item.activityID)
	}
}

// TestTheHarvestHasItsOwnWallBound. MaxPages by up to 999 rows, each
// candidate row costing a fetch with its own 30 s timeout, and a row that
// 404s or 401s never counting toward MaxSample: the fetch count has no cap
// of its own. It runs under the trial's four-hour lease and C11 hold and
// immediately in front of a config write that evicts every resident model.
func TestTheHarvestHasItsOwnWallBound(t *testing.T) {
	f := newFakeCell(t)
	for id := int64(1); id <= 6; id++ {
		f.row(id, "incumbent", "/v1/chat/completions", 200, plainRequest("p"), "")
	}
	now := time.Unix(1_700_000_000, 0)
	tick := func() time.Time {
		now = now.Add(time.Minute)
		return now
	}
	_, err := Harvest(t.Context(), Options{
		LlamaSwapURL: f.url, Now: tick, HarvestBudget: 2 * time.Minute,
	}, "incumbent", false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "budget")
	require.Less(t, f.captureReads(6), 1, "the walk must stop; if the last row was fetched the deadline never elapsed")

	// The same fixture inside a budget it fits: the assertion above is the
	// bound firing rather than the harvest being broken.
	set, err := Harvest(t.Context(), Options{LlamaSwapURL: f.url}, "incumbent", false)
	require.NoError(t, err)
	require.Equal(t, 6, set.Len())
}

func TestOptionsDefaultsAreTheDocumentedConstants(t *testing.T) {
	o := Options{}.withDefaults()
	require.Equal(t, DefaultMaxSample, o.MaxSample)
	require.Equal(t, DefaultMaxPages, o.MaxPages)
	require.Equal(t, DefaultRateFloor, o.RateFloor)
	require.Equal(t, DefaultReplayTimeout, o.ReplayTimeout)
	require.Equal(t, DefaultSideBudget, o.SideBudget)
	require.Equal(t, DefaultHarvestBudget, o.HarvestBudget)
	require.NotNil(t, o.HTTP)
	require.NotNil(t, o.Now)
}

// TestASideThatOutrunsItsBudgetRefusesRatherThanTruncating pins the one
// bound a per-request timeout does not give: 40 samples at a 5-minute
// per-request timeout is nearly four hours PER SIDE, and the sample is
// real agentic traffic with its own max_tokens.
//
// The refusal, rather than a short n, is the point. A side that replayed
// part of the sample while the other replayed all of it is not a paired
// comparison, and printing one would be a metric lying about its own n.
func TestASideThatOutrunsItsBudgetRefusesRatherThanTruncating(t *testing.T) {
	f := newFakeCell(t)
	f.warm("incumbent", "candidate")
	for id := int64(1); id <= 5; id++ {
		f.row(id, "incumbent", "/v1/chat/completions", 200, plainRequest("p"), "")
	}

	// A clock that jumps a minute per read, so the budget ELAPSES rather
	// than being asserted against a value nothing reaches — the shape three
	// tests in this repo got wrong by borrowing their deadline from the
	// caller.
	now := time.Unix(1_700_000_000, 0)
	tick := func() time.Time {
		now = now.Add(time.Minute)
		return now
	}
	set, err := Harvest(t.Context(), Options{LlamaSwapURL: f.url}, "incumbent", false)
	require.NoError(t, err)
	set.opt.Now = tick
	set.opt.SideBudget = 2 * time.Minute

	_, err = set.Run(t.Context(), "c", Side{Model: "incumbent"}, Side{Model: "candidate"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "budget")
	require.Contains(t, err.Error(), "not a paired comparison")
	require.Less(t, len(f.sentBodies("incumbent")), 5,
		"the budget must stop the run; if every request went out, the deadline never elapsed and this test proves nothing")

	// And a budget it fits inside produces a full score, so the assertion
	// above is the budget firing rather than the fixture being broken.
	set2, err := Harvest(t.Context(), Options{LlamaSwapURL: f.url}, "incumbent", false)
	require.NoError(t, err)
	rep, err := set2.Run(t.Context(), "c", Side{Model: "incumbent"}, Side{Model: "candidate"})
	require.NoError(t, err)
	require.Equal(t, 5, rep.Incumbent.Requests)
	require.Equal(t, 5, rep.Candidate.Requests)
}

// TestNothingCrossesABox is mechanism 5, as a scan. This package must
// reach llama-swap on loopback and nothing else: no fleetd client, no
// announce type, no MCP tool.
func TestNothingCrossesABox(t *testing.T) {
	forbidden := []string{"fleetapi", "fleetmcp", "fleetannounce", "vibeclient", "daemon", "fleetnotify"}
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	files := 0
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		files++
		b, rerr := os.ReadFile(n)
		require.NoError(t, rerr)
		for _, pkg := range forbidden {
			require.NotContains(t, string(b), `vibe/`+pkg+`"`,
				"%s imports %s: a replay emits no announce field, adds no route, adds no MCP tool and sends fleetd nothing (C25 §3, mechanism 5)", n, pkg)
		}
	}
	require.GreaterOrEqual(t, files, 4)
}

// observedIsUsed keeps the import honest in a file that otherwise only
// reads through the accessors.
var _ = observed.Known(1.0)

var _ = fmt.Sprint
