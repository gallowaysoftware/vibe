package observed

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/astscan"
)

// fleet-control C20, class 1: absent evidence read as a healthy value.
//
// The Value type removes the shape from the code it reaches. These three
// scans are what stop the shape being re-introduced beside it, and they
// run over the WHOLE module rather than one package, because every
// occurrence so far was in a different one.
//
// All three carry the same two guards the rest of this harness insists
// on: an explicit exemption table where each entry is a written reason,
// and a floor on how much was examined — a scan that quietly matches
// nothing is the failure mode of every structural test in this repo.
//
// C21 added the third: the floors and the stale-exemption rule are
// themselves tested (TestScanGuardsAreLoadBearing). A floor nobody has
// watched fail is a number, not a guard.

// moduleRoot is this module's own root, reached from this package's own
// directory. The scans walk it so a package added by a future phase is
// covered without anybody remembering to add it to a list.
const moduleRoot = "../../.."

// ── the walk ────────────────────────────────────────────────────────────

// moduleGoFiles parses every non-test .go file belonging to THIS module.
//
// "Belonging to this module" is the whole difficulty. The first cut walked
// everything under moduleRoot, and this repo keeps git worktrees inside
// the tree (.claude/worktrees/*, one per parallel agent) — each a complete
// second copy of the same source. The scans then examined 2080 files and
// 17223 functions instead of 208 and 1721, reported an exempted function
// once per worktree under a path no exemption key could match, and failed
// on every developer machine while passing in CI, where a fresh clone has
// no worktrees. The inflated denominator was the quieter half of the
// damage: an inertness floor that a foreign tree can satisfy has stopped
// being able to notice that this module's own code went away.
//
// So two exclusions, and both are properties rather than names:
//
//   - astscan.ForeignDir: a nested checkout (a .git file or directory) or
//     a nested module (its own go.mod).
//   - the go tool's own rule: directories beginning with "." or "_", plus
//     testdata, are not part of a build. Following the toolchain here means
//     the scan sees what `go build ./...` sees, and a future phase parking
//     something under a new dot-directory does not silently join the scan.
func moduleGoFiles(root string) (map[string]*ast.File, *token.FileSet, error) {
	fset := token.NewFileSet()
	out := map[string]*ast.File{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir(root, path, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		// Generated code follows protoc's conventions, not this repo's.
		if strings.HasSuffix(name, ".pb.go") || strings.Contains(path, "vibev1connect") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		// Keys are repo-relative so the exemption tables read the way a
		// reviewer types a path, not the way this test's directory sees it.
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		out[filepath.ToSlash(rel)] = f
		return nil
	})
	return out, fset, err
}

func skipDir(root, path, name string) bool {
	if path == root {
		return false
	}
	if name == "testdata" || name == "node_modules" {
		return true
	}
	if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
		return true
	}
	return astscan.ForeignDir(root, path)
}

// minModuleFiles is the walk's own inertness floor. The module has ~208
// non-test, non-generated source files; a walk that reaches half of them
// is broken, and every scan below would pass over the missing half in
// silence. It is asserted in one place because all three scans share the
// walk.
const minModuleFiles = 150

func moduleFiles(t *testing.T) (map[string]*ast.File, *token.FileSet) {
	t.Helper()
	files, fset, err := moduleGoFiles(moduleRoot)
	if err != nil {
		t.Fatalf("walk %s: %v", moduleRoot, err)
	}
	if len(files) < minModuleFiles {
		t.Fatalf("scanned %d source files, expected at least %d — the walk is not reaching the module "+
			"and every scan below would pass over nothing", len(files), minModuleFiles)
	}
	// The mirror image, and the one that actually bit: a walk that reaches
	// TOO MUCH is not a stricter scan, it is a broken one. Every key must
	// be a path inside this module.
	for path := range files {
		if strings.HasPrefix(path, "..") || strings.HasPrefix(path, ".") || filepath.IsAbs(path) {
			t.Fatalf("scanned %q, which is outside this module's own source: the exemption tables are "+
				"keyed on repo-relative paths and can never name it", path)
		}
	}
	return files, fset
}

// ── the shared guards on a scan ─────────────────────────────────────────

// finding is one flagged site: the key an exemption would use, and the
// sentence the next agent reads.
type finding struct {
	Key    string
	Detail string
}

// scan carries the two guards every structural rule in this repo needs:
// a written-reason exemption table where an UNUSED entry is an error, and
// a floor on how much was examined. Both are folded into one audit so a
// caller cannot enforce the findings and forget the guards on the scan
// itself — the mistake this harness exists to stop repeating.
type scan struct {
	Name   string
	Exempt map[string]string
	// Floor and Unit are the inertness assertion. A scan that matches
	// nothing PASSES, which is how a rename silently retires a rule.
	Floor int
	Unit  string
}

func (s scan) audit(findings []finding, examined int) []string {
	var live []string
	used := map[string]bool{}
	for _, f := range findings {
		if _, ok := s.Exempt[f.Key]; ok {
			used[f.Key] = true
			continue
		}
		live = append(live, f.Detail)
	}
	sort.Strings(live)
	problems := live
	if examined < s.Floor {
		problems = append(problems, fmt.Sprintf(
			"examined %d %s, expected at least %d: this scan is INERT — the code it is about was "+
				"renamed, moved or deleted, and it would now pass over anything",
			examined, s.Unit, s.Floor))
	}
	var stale []string
	for key := range s.Exempt {
		if !used[key] {
			stale = append(stale, fmt.Sprintf(
				"exemption %q matches nothing: it is STALE (the line moved, or the site is gone). "+
					"Re-point it or delete it — an exemption nobody can reach is a hole nobody is watching.", key))
		}
	}
	sort.Strings(stale)
	return append(problems, stale...)
}

func report(t *testing.T, s scan, problems []string) {
	t.Helper()
	if len(problems) == 0 {
		return
	}
	t.Errorf("%s: %d problem(s):\n  %s", s.Name, len(problems), strings.Join(problems, "\n  "))
}

// floorSlack is how far a floor may sit below the live denominator before
// it stops being a guard.
//
// A floor that can never fire is the same failure as no floor, and it
// arrives by drift rather than by decision: the module grows, the number
// does not, and one day it is satisfied by a tenth of the tree. The
// nested-worktree walk made that concrete — with nine foreign copies in
// the denominator, a floor of 500 was met by 17223 functions, so this
// module's own 1721 could have gone to ZERO with the floor still green.
// A quarter is arbitrary; being checked at all is not.
const floorSlack = 4

func checkFloorStillGuards(t *testing.T, s scan, examined int) {
	t.Helper()
	if examined > 0 && s.Floor*floorSlack < examined {
		t.Errorf("%s: the floor is %d against %d %s actually examined, so %d%% of what this scan covers "+
			"could vanish before it complained. That is a number, not a guard — raise it.",
			s.Name, s.Floor, examined, s.Unit, 100-100*s.Floor/examined)
	}
}

// ── scan 1: nobody drops the known bit ──────────────────────────────────

// discardedKnownBit is the one hole the type deliberately leaves open:
// `n, _ := x.Observed()`. It is legal Go and occasionally correct, and it
// is also exactly the line that produced C4's idle floor and the v247
// disarm. Leaving it possible but visible is the trade; this is the
// visibility.
var discardedKnownBitScan = scan{
	Name:   "no observed.Value read with a discarded known bit",
	Exempt: map[string]string{},
	// The migration put real two-value .Observed() reads in the tree. A
	// rename or a rollback that removed them would make this scan pass
	// over nothing.
	Floor: 8,
	Unit:  "two-value Observed() reads",
}

func TestObservedIsNeverReadWithADiscardedKnownBit(t *testing.T) {
	files, fset := moduleFiles(t)
	findings, reads := findDiscardedKnownBits(files, fset)
	t.Logf("examined %d two-value Observed() reads", reads)
	checkFloorStillGuards(t, discardedKnownBitScan, reads)
	report(t, discardedKnownBitScan, discardedKnownBitScan.audit(findings, reads))
}

func findDiscardedKnownBits(files map[string]*ast.File, fset *token.FileSet) (findings []finding, reads int) {
	for _, path := range sortedKeys(files) {
		ast.Inspect(files[path], func(n ast.Node) bool {
			var lhs []ast.Expr
			var rhs []ast.Expr
			switch v := n.(type) {
			case *ast.AssignStmt:
				lhs, rhs = v.Lhs, v.Rhs
			case *ast.ValueSpec:
				for _, name := range v.Names {
					lhs = append(lhs, name)
				}
				rhs = v.Values
			default:
				return true
			}
			if len(rhs) != 1 || len(lhs) != 2 {
				return true
			}
			call, ok := rhs[0].(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Observed" || len(call.Args) != 0 {
				return true
			}
			reads++
			id, ok := lhs[1].(*ast.Ident)
			if !ok || id.Name != "_" {
				return true
			}
			// Keyed on the LINE. The identifier is always "_", so a
			// file-scoped key would let one exemption cover every future
			// dropped bit in the same file.
			line := fset.Position(id.Pos()).Line
			findings = append(findings, finding{
				Key: fmt.Sprintf("%s:%d", path, line),
				Detail: fmt.Sprintf("%s:%d: reads an observed.Value and discards the known bit. "+
					"That zero is not a measurement — it is the absence of one, and this fleet has shipped "+
					"six defects spelled exactly this way. Use IsKnown() if only the evidence matters, or "+
					"OrElse(<value>) to write down what absence means here.", path, line),
			})
			return true
		})
	}
	return findings, reads
}

// ── scan 2: no new (value, bool) field pair ─────────────────────────────

// pairSuffixes are the ways this repo has spelled "and here is whether
// the value beside me means anything".
var pairSuffixes = []string{"Seen", "Known", "Reported", "OK", "Ok", "Valid", "Set", "Present", "Measured", "Available"}

// evidencePairScan's table is written-reason. An entry is a claim that the
// pair is NOT an evidence carrier; it is not a way to postpone the
// migration.
var evidencePairScan = scan{
	Name: "no new (value, known-bit) field pair",
	Exempt: map[string]string{
		// C7b's power term is an ACCUMULATOR plus "did anything contribute",
		// not a measurement plus "was it measured": every write is
		// `power += cost; powerKnown = true` at one site. observed.Value would
		// spell each += as Known(OrElse(0)+cost), which is strictly worse code
		// for no change in what is representable.
		//
		// The reads are NOT uniformly gated on the bool, and the first draft
		// of this table said they were — `dayNet.net()` is `Gross - Power`
		// with no reference to PowerKnown, so the payback series bills an
		// undeclared cell's electricity as zero. That is C7b's deliberate
		// "the power term is the one place this screen errs LARGE", stated on
		// the page by `powerGapNote`, not a dropped bit. Written down here
		// because an exemption's REASON is the only thing that tells the next
		// agent whether to re-examine it: re-examine if the accumulation ever
		// moves away from its guard, or if the err-large disclosure goes away.
		//
		// Re-read against savings.go at C21: still true. dayNet.net() is
		// `d.Gross - d.Power` with no PowerKnown reference, and powerGapNote
		// is still built from powerMissing and rendered on the page.
		"internal/vibe/fleetapi/savings.go:Power": "accumulator + did-anything-contribute; the one ungated read (dayNet.net) is C7b's declared err-large payback term, disclosed by powerGapNote",
		// Re-read at C21: every read of agg.power (the row's Power, the row's
		// Net, both totals) sits under an `agg.powerKnown` guard.
		"internal/vibe/fleetapi/savings.go:power": "same pair, the unexported window aggregate; every read of this one IS gated on powerKnown",
	},
	Floor: 400,
	Unit:  "struct types",
}

func TestNoNewValueAndKnownBitFieldPair(t *testing.T) {
	files, fset := moduleFiles(t)
	findings, structs := findEvidencePairs(files, fset)
	t.Logf("examined %d struct types", structs)
	checkFloorStillGuards(t, evidencePairScan, structs)
	report(t, evidencePairScan, evidencePairScan.audit(findings, structs))
}

func findEvidencePairs(files map[string]*ast.File, fset *token.FileSet) (findings []finding, structs int) {
	for _, path := range sortedKeys(files) {
		ast.Inspect(files[path], func(n ast.Node) bool {
			st, ok := n.(*ast.StructType)
			if !ok || st.Fields == nil {
				return true
			}
			structs++
			types := map[string]ast.Expr{}
			pos := map[string]token.Pos{}
			var order []string
			for _, fl := range st.Fields.List {
				for _, id := range fl.Names {
					types[id.Name] = fl.Type
					pos[id.Name] = id.Pos()
					order = append(order, id.Name)
				}
			}
			for _, name := range order {
				for _, sfx := range pairSuffixes {
					partner, ok := types[name+sfx]
					if !ok || !boolish(partner) {
						continue
					}
					line := fset.Position(pos[name]).Line
					findings = append(findings, finding{
						Key: path + ":" + name,
						Detail: fmt.Sprintf("%s:%d: %s is paired with %s%s. That is the (value, known) shape "+
							"observed.Value exists to replace: two fields that must agree, where every way of "+
							"losing the second one — a map miss, a dropped return, a delete — yields a "+
							"confident zero. Use observed.Value[T], or add %s:%s to the exemption table with the "+
							"reason it is not an evidence carrier.",
							path, line, name, name, sfx, path, name),
					})
				}
			}
			return true
		})
	}
	return findings, structs
}

// ── scan 3: no new (measurement, bool) return ───────────────────────────

// numericResult is the first-result shape that makes a (T, bool) return a
// MEASUREMENT rather than a comma-ok lookup. A map lookup returning
// (Lease, bool) is fine — the record either exists or it does not, and no
// caller can mistake a zero Lease for a real one. A count or an instant
// is different: its zero is a perfectly plausible value.
var numericResult = map[string]bool{
	"int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true,
	"float32": true, "float64": true, "Duration": true, "Time": true,
}

var measurementReturnScan = scan{
	Name: "no new (measurement, bool) return",
	Exempt: map[string]string{
		// A pure arithmetic helper over a slice the caller owns: "false" means
		// the slice was empty, which the caller checked before calling. No
		// evidence crosses a boundary here.
		"internal/vibe/modelprobe/modelprobe.go:median": "arithmetic over a caller-owned slice; the bool is 'the input was empty'",
		// A linear scan over the vendored price table. The int is a slice
		// INDEX, not a measurement: it is -1 when there is no table at all,
		// and resolveTo checks `idx < 0` before reading anything.
		//
		// The reason written here at C20 was FALSE, in the phase that made
		// written reasons the mechanism, and it was false in the direction
		// that matters — it claimed the bool meant "no snapshot on or before
		// this day", rendered as an unpriced reason. It does not. The bool is
		// `beforeBase` at both call sites: TRUE means the day predates the
		// base snapshot and is priced AT the base anyway, with the fact
		// disclosed on the page via Resolved.BeforeBase. FALSE is the
		// ordinary, healthy path. An exemption whose reason describes the
		// opposite of the code is worse than no exemption: the next agent
		// re-reads the reason instead of the function.
		"internal/vibe/prices/prices.go:resolveIndex": "the int is a slice index (-1 for 'no table', checked by resolveTo), and the bool is beforeBase — a disclosure flag both callers propagate to Resolved.BeforeBase, not an evidence bit",
		// A cron evaluator: the bool is "this spec never fires within 8
		// years", a property of the SPEC, not an observation of the fleet.
		// Re-read at C21: all five call sites (two in warmsched, three in
		// sleepsched) take the false branch to a disabled state with an
		// operator-visible note.
		"internal/vibe/fleetapi/warmsched.go:nextFire": "a property of the cron spec, not an observation; the false branch disables the entry loudly",
		// C7b's power term, same pair as the evidence-pair table above.
		"internal/vibe/fleetapi/savings.go:cellPowerCost": "the declared-power accumulator's reader; see evidencePairScan",
		// vamp, not fleet: three string/duration parsers whose bool means "the
		// input did not contain one", checked by the caller on the next line.
		// Listed rather than migrated because vamp has no evidence axis — no
		// guard anywhere reads these as a claim about a machine.
		"internal/vamp/ffmpeg_executor.go:trailingInt":      "parses a trailing integer out of a filename; false means there was none",
		"internal/vamp/webhook_executor.go:parseRetryAfter": "parses an HTTP Retry-After header; false means the header was absent or unparseable",
		"internal/vamp/timing.go:overheadPercent":           "arithmetic over a caller-owned report; false means the denominator was zero",
	},
	Floor: 1200,
	Unit:  "functions with results",
}

func TestNoNewMeasurementAndBoolReturn(t *testing.T) {
	files, fset := moduleFiles(t)
	findings, funcs := findMeasurementReturns(files, fset)
	t.Logf("examined %d functions with results", funcs)
	checkFloorStillGuards(t, measurementReturnScan, funcs)
	report(t, measurementReturnScan, measurementReturnScan.audit(findings, funcs))
}

func findMeasurementReturns(files map[string]*ast.File, fset *token.FileSet) (findings []finding, funcs int) {
	for _, path := range sortedKeys(files) {
		for _, d := range files[path].Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Type.Results == nil {
				continue
			}
			funcs++
			var flat []ast.Expr
			for _, r := range fn.Type.Results.List {
				n := max(len(r.Names), 1)
				for range n {
					flat = append(flat, r.Type)
				}
			}
			if len(flat) != 2 || !numericIdent(flat[0]) {
				continue
			}
			if id, ok := flat[1].(*ast.Ident); !ok || id.Name != "bool" {
				continue
			}
			key := path + ":" + fn.Name.Name
			findings = append(findings, finding{
				Key: key,
				Detail: fmt.Sprintf("%s:%d: %s returns a measurement plus a bool. Return observed.Value[T] instead — "+
					"the bool is droppable and the value's zero is a plausible measurement, which is the "+
					"combination that disarmed eight busy guards on the v247 wire. If this is a comma-ok "+
					"lookup rather than evidence, add %s to the exemption table with the reason.",
					path, fset.Position(fn.Pos()).Line, fn.Name.Name, key),
			})
		}
	}
	return findings, funcs
}

// ── a fourth scan, considered and DECLINED (C21) ────────────────────────
//
// The shape: `probeTCP` (internal/vibe/fleetapi/presence.go) returns a
// bare bool. ECONNREFUSED — the machine is up, nothing is listening — an
// i/o timeout, and fleetd's OWN snapshot budget expiring all arrive at the
// operator as the same word, so "fleetd was too busy to look" renders as
// "your machine is switched off". That is this plan's class 1 exactly, and
// it is structurally INVISIBLE to scan 3 above: a lone `bool` return does
// not match the (T, bool) shape the scan hunts.
//
// A rule was drafted and measured over the module before being declined.
// The numbers, so the next agent who has this idea does not have to
// re-derive them:
//
//   - 104 functions return exactly one bool.
//   - 22 of those handle an error they were not GIVEN — the tightest
//     syntactic approximation of "produced a distinction and destroyed
//     it" that go/ast can express. (Excluding the ones that TAKE an error
//     is what removes isRetryable, isAddrInUse, IsNotFound and friends:
//     a classifier of somebody else's error destroys nothing.)
//   - of those 22, about 5 are the real class — probeTCP, probeTCPDirect,
//     probeURL, probeServiceURL, IsAlive. The other 17 are ordinary
//     predicates whose fail-safe answer is genuinely "no": fileExists,
//     isHTTPS, promptYesNo, inside, ForeignDir, two proxy handlers, and
//     so on.
//
// Roughly 23% precision. Seventeen exemptions written in one sitting, most
// of them saying "this is an ordinary predicate", is how an exemption
// table stops being read — and the tables above are 2 and 7 entries
// because each one had to be argued.
//
// The deeper reason, which no amount of tightening fixes: scan 3 works
// because the SHAPE is the smell. A numeric zero beside a droppable bit is
// dangerous wherever it appears, regardless of what the caller does. A
// bare bool has no such property — it is also the shape of every correct
// predicate in Go. What separates probeTCP from fileExists is not its
// signature but what happens downstream: one becomes a sentence an
// operator reads about their hardware, the other becomes an if. That is
// dataflow, and dataflow needs go/types, which this package deliberately
// does not have (see internal/astscan's header: no golang.org/x/tools).
//
// The narrow version — "a bare bool returned by a function that dials" —
// was drafted too, and declined for a different reason: it would have
// shipped with all four of its live matches exempted, three of them in
// packages this phase does not touch and one of them mid-fix in a sibling
// branch. A rule with no satisfied instance and a full exemption table is
// decoration that manufactures confidence, which is the precise failure
// C20's own review found five times over.
//
// What would change the answer: a named reachability result in fleetapi
// (refused / timed out / not attempted / unreachable) would turn this into
// an astscan.Rule — "every probe producer must return it" — which is a
// shape this harness already enforces well, on the guard side rather than
// the type side.

func numericIdent(t ast.Expr) bool {
	switch v := t.(type) {
	case *ast.Ident:
		return numericResult[v.Name]
	case *ast.SelectorExpr:
		return numericResult[v.Sel.Name]
	}
	return false
}

func boolish(t ast.Expr) bool {
	switch v := t.(type) {
	case *ast.Ident:
		return v.Name == "bool"
	case *ast.MapType:
		return boolish(v.Value)
	case *ast.ArrayType:
		return boolish(v.Elt)
	case *ast.StarExpr:
		return boolish(v.X)
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
