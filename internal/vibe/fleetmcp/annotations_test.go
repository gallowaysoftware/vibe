package fleetmcp

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Tool annotations (MCP 2025-06-18 §tools). Every tool this facade
// advertises carries readOnlyHint and destructiveHint, derived from the
// ONE place each tool's behaviour is declared — the `Effect` field on
// its own toolDef — rather than from a second list somewhere that has to
// be kept in step.
//
// The hazard these tests are written against is not a missing
// annotation. It is a WRONG one: `fleet_doctor` shouting "This tool
// CHANGES NOTHING" in prose while `readOnlyHint` says otherwise (or vice
// versa) is worse than no annotation, because a client that trusts the
// flag has no way to notice the prose disagreeing.

// TestTools_EveryToolDeclaresAnEffect is the registry-completeness
// check, and the analogue of fleetapi's
// TestRoutes_EveryRouteDeclaresAnAccessLevel: toolEffect has no safe
// zero value, so a tool added without a decision fails here rather than
// advertising itself as "not read-only, not destructive" by default.
func TestTools_EveryToolDeclaresAnEffect(t *testing.T) {
	defs := (&Server{}).toolDefs()
	require.NotEmpty(t, defs, "no tools advertised; this test would pass vacuously")
	seen := map[string]bool{}
	for _, d := range defs {
		require.NotEqual(t, effectUndeclared, d.Effect,
			"tool %q declares no effect: every tool must say whether it changes nothing, "+
				"changes something reversibly, or can destroy work", d.Name)
		require.NotEmpty(t, d.Name, "a tool has no name")
		require.NotEmpty(t, d.Description, "tool %q has no description", d.Name)
		require.NotNil(t, d.InputSchema, "tool %q has no input schema", d.Name)
		require.False(t, seen[d.Name], "tool %q is declared twice", d.Name)
		seen[d.Name] = true
	}
}

// TestTools_AnnotationsAreDerivedFromTheDeclaredEffect walks the WIRE
// shape — what a client actually receives from tools/list — and checks
// each tool's hints against its declaration. It also pins that the
// facade advertises annotations at all: zero of the sixteen carried any
// before this change, on a server that advertises a protocol version
// which defines them.
func TestTools_AnnotationsAreDerivedFromTheDeclaredEffect(t *testing.T) {
	s := &Server{}
	byName := map[string]toolEffect{}
	for _, d := range s.toolDefs() {
		byName[d.Name] = d.Effect
	}
	wire := s.mcpTools()
	require.Len(t, wire, len(byName), "tools/list and toolDefs disagree about how many tools exist")
	for _, tool := range wire {
		m, ok := tool.(map[string]any)
		require.True(t, ok, "a tools/list entry is not an object")
		name, _ := m["name"].(string)
		ann, ok := m["annotations"].(map[string]any)
		require.Truef(t, ok, "tool %q carries no annotations; the facade advertises protocol %s, "+
			"which defines readOnlyHint and destructiveHint", name, mcpProtocolVersion)
		effect := byName[name]
		require.Equal(t, effect == effectRead, ann["readOnlyHint"],
			"tool %q: readOnlyHint disagrees with its declared effect", name)
		require.Equal(t, effect == effectDestructive, ann["destructiveHint"],
			"tool %q: destructiveHint disagrees with its declared effect", name)
		require.Equal(t, true, ann["openWorldHint"], "tool %q: the fleet is an open world", name)
		// A read-only tool that also claims to be destructive is the one
		// combination that cannot be true of anything.
		if ann["readOnlyHint"] == true {
			require.Equal(t, false, ann["destructiveHint"],
				"tool %q is both read-only and destructive", name)
		}
	}
}

// TestTools_ProseAndFlagAgree binds the machine-readable flag to the
// English an agent reads in the same breath. `fleet_doctor` says "This
// tool CHANGES NOTHING" and `render_front` says "DRY-RUN ONLY"; either
// sentence is a claim of read-only-ness, and a tool that makes it while
// declaring an effect that mutates is a contradiction shipped to an
// agent. The destructive verbs are checked the other way: they must keep
// warning in prose, because the flag is a hint and the sentence is what
// an operator is told.
//
// hazardSentence is a TABLE and every destructive tool must be a key,
// so a new one cannot arrive without somebody deciding what its
// description has to keep saying. The prose half used to be a single
// `if d.Name == "drain_cell"`, under a comment claiming "the two
// destructive verbs are checked the other way" — there are four, and one
// was checked.
var hazardSentence = map[string]string{
	"drain_cell":   "CANCELS in-flight streams",
	"suspend_cell": "check the cell HAS a wake path before suspending",
	"probe_model":  "a probe spends real GPU time",
	// unload_model carries no hazard sentence today: its description says
	// what eviction does and how it is undone, and nothing about what
	// happens to work in flight on the model being evicted. Recorded as
	// empty rather than papered over with a phrase from the first
	// sentence — an assertion that matches the tool's own summary proves
	// only that the summary exists. Writing the sentence is a change to
	// what agents are TOLD, which is a product decision, not a review one.
	"unload_model": "",
}

func TestTools_ProseAndFlagAgree(t *testing.T) {
	readOnlyClaims := []string{"CHANGES NOTHING", "DRY-RUN ONLY", "READ-ONLY audit", "RAW COUNTS ONLY"}
	for _, d := range (&Server{}).toolDefs() {
		for _, claim := range readOnlyClaims {
			if strings.Contains(d.Description, claim) {
				require.Equalf(t, effectRead, d.Effect,
					"tool %q says %q in its description but is declared as effect %d; "+
						"an agent reads the prose and a client reads the flag, and they must not disagree",
					d.Name, claim, d.Effect)
			}
		}
		if d.Effect != effectDestructive {
			continue
		}
		want, listed := hazardSentence[d.Name]
		require.Truef(t, listed,
			"tool %q is declared destructive but hazardSentence says nothing about it. Add it: an agent is "+
				"told what a verb costs by the description, and a destructive verb that arrives without that "+
				"decision being made is one nobody chose not to warn about", d.Name)
		if want == "" {
			continue
		}
		require.Containsf(t, d.Description, want,
			"tool %q stopped saying %q — the sentence an operator is shown before a destructive verb runs",
			d.Name, want)
	}
}

// TestTools_AdvertisedAndDispatchedAgree is the parity half: every tool
// in the table has a case in callTool's switch, and every case in that
// switch is in the table. Without it the table is a THIRD list — a tool
// advertised with a beautiful annotation and no implementation answers
// "unknown tool", and a tool implemented but not advertised carries no
// annotation at all because nothing renders it.
//
// The switch is read out of the source with go/ast rather than by
// calling callTool, because dispatching a real tool on a zero Server
// would reach a nil hosts.yaml.
func TestTools_AdvertisedAndDispatchedAgree(t *testing.T) {
	advertised := map[string]bool{}
	for _, d := range (&Server{}).toolDefs() {
		advertised[d.Name] = true
	}
	dispatched := dispatchedToolNames(t)
	require.NotEmpty(t, dispatched, "no dispatch cases found; this test would pass vacuously")

	for name := range advertised {
		require.Truef(t, dispatched[name],
			"tool %q is advertised by tools/list but callTool has no case for it: calling it answers \"unknown tool\"", name)
	}
	for name := range dispatched {
		require.Truef(t, advertised[name],
			"callTool dispatches %q but tools/list never offers it, so it carries no annotations and no schema", name)
	}
}

// dispatchedToolNames returns the tool names callTool's switch handles,
// parsed from fleetmcp.go.
func dispatchedToolNames(t *testing.T) map[string]bool {
	t.Helper()
	path := filepath.Join(".", "fleetmcp.go")
	src, err := os.ReadFile(path)
	require.NoError(t, err)
	return dispatchedToolNamesIn(t, path, src)
}

// dispatchedToolNamesIn is the parse, split out so it can be driven
// against a fixture — see TestDispatchedToolNamesIgnoresTheTimeoutSwitch.
//
// callTool contains TWO switches keyed on `name`: the deadline-sizing one
// and the dispatch one. Only a case that RETURNS answers the call, so
// only a returning case counts as an implementation.
//
// This used to fold both switches into one set, on the stated grounds
// that "the timeout one is a subset of the dispatch one, so folding them
// together cannot hide a missing case". That is false in the direction
// that matters: the timeout switch names six tools, so deleting any of
// those six from the DISPATCH switch left the name in the set anyway and
// the parity check stayed green while the tool answered "unknown tool".
// Measured on render_front — advertised, undispatched, all five
// TestTools_* green.
func dispatchedToolNamesIn(t *testing.T, path string, src []byte) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	require.NoError(t, err)

	out := map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "callTool" || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			sw, ok := n.(*ast.SwitchStmt)
			if !ok {
				return true
			}
			id, ok := sw.Tag.(*ast.Ident)
			if !ok || id.Name != "name" {
				return true
			}
			for _, stmt := range sw.Body.List {
				cc, ok := stmt.(*ast.CaseClause)
				if !ok || !caseAnswers(cc) {
					continue
				}
				for _, expr := range cc.List {
					lit, ok := expr.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					v, err := strconv.Unquote(lit.Value)
					require.NoError(t, err)
					out[v] = true
				}
			}
			return true
		})
	}
	require.NotEmpty(t, out, "callTool's dispatch switch was not found; this guard is measuring nothing")
	return out
}

// caseAnswers reports whether a `case "tool":` arm returns — i.e. whether
// it is the arm that ANSWERS the call rather than one that adjusts a
// deadline and falls through to the dispatch below.
func caseAnswers(cc *ast.CaseClause) bool {
	answers := false
	for _, stmt := range cc.Body {
		ast.Inspect(stmt, func(n ast.Node) bool {
			if _, ok := n.(*ast.ReturnStmt); ok {
				answers = true
			}
			return !answers
		})
	}
	return answers
}

// TestDispatchedToolNamesIgnoresTheTimeoutSwitch is the guard on the
// guard. TestTools_AdvertisedAndDispatchedAgree is only worth its name if
// the set it compares against is the set of tools that can actually be
// CALLED, and the fixture below is the exact shape that defeated it: a
// tool named in the deadline switch and absent from the dispatch one.
func TestDispatchedToolNamesIgnoresTheTimeoutSwitch(t *testing.T) {
	const fixture = `package fleetmcp

func (s *Server) callTool(ctx context.Context, name string, rawArgs []byte) (string, error) {
	timeout := toolTimeout
	switch name {
	case "slow_tool", "also_slow":
		timeout = 90
	}
	_ = timeout
	switch name {
	case "also_slow":
		return s.toolAlsoSlow(ctx)
	default:
		return "", errUnknown
	}
}
`
	got := dispatchedToolNamesIn(t, "fixture.go", []byte(fixture))
	require.Truef(t, got["also_slow"], "a tool with a returning case was not counted as dispatched: %v", got)
	require.Falsef(t, got["slow_tool"],
		"slow_tool appears ONLY in the deadline switch and has no dispatch case — counting it as dispatched is "+
			"what let an advertised tool answer \"unknown tool\" with every parity test green: %v", got)
}

// TestTools_DestructiveSetIsSmallAndNamed is the tripwire, not a
// completeness check: widening the set of verbs a client is told are
// safe must be a deliberate edit to a test that says why, never a side
// effect of adding a tool. It is fleetapi's
// TestRoutes_GuestSurfaceIsExactlyStateAndEvents, for effects.
func TestTools_DestructiveSetIsSmallAndNamed(t *testing.T) {
	wantRead := []string{"fleet_doctor", "fleet_savings", "fleet_status", "fleet_usage", "render_front"}
	wantDestructive := []string{"drain_cell", "probe_model", "suspend_cell", "unload_model"}

	var gotRead, gotDestructive []string
	for _, d := range (&Server{}).toolDefs() {
		switch d.Effect {
		case effectRead:
			gotRead = append(gotRead, d.Name)
		case effectDestructive:
			gotDestructive = append(gotDestructive, d.Name)
		}
	}
	sort.Strings(gotRead)
	sort.Strings(gotDestructive)
	require.Equal(t, wantRead, gotRead,
		"the read-only set changed. readOnlyHint is what a client uses to decide a tool is safe to "+
			"call unattended, so adding one is a real decision — update this test and say why.")
	require.Equal(t, wantDestructive, gotDestructive,
		"the destructive set changed. Dropping a tool from it tells every client that a verb which "+
			"can truncate a generation or take a box off the fleet is merely an update.")
}
