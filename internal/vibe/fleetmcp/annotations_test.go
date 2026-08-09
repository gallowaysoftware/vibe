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
// agent. The two destructive verbs are checked the other way: they must
// keep warning in prose, because the flag is a hint and the sentence is
// what an operator is told.
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
		if d.Effect == effectRead {
			continue
		}
		// The converse guard, stated positively for the two verbs whose
		// truncation risk is the whole reason drain_cell's description
		// exists.
		if d.Name == "drain_cell" {
			require.Contains(t, d.Description, "CANCELS in-flight streams",
				"drain_cell stopped saying what its SIGTERM does to a generation in progress")
		}
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
			// Both switches in callTool key on `name`; the timeout one is a
			// subset of the dispatch one, so folding them together cannot
			// hide a missing case.
			id, ok := sw.Tag.(*ast.Ident)
			if !ok || id.Name != "name" {
				return true
			}
			for _, stmt := range sw.Body.List {
				cc, ok := stmt.(*ast.CaseClause)
				if !ok {
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
