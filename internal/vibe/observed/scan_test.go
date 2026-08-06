package observed

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
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

// moduleRoot is internal/, reached from this package's own directory. The
// scans walk it so a package added by a future phase is covered without
// anybody remembering to add it to a list.
const moduleRoot = "../../.."

// scanFiles yields every non-test .go file under the module, excluding
// testdata and generated protobuf output.
func scanFiles(t *testing.T) (map[string]*ast.File, *token.FileSet) {
	t.Helper()
	fset := token.NewFileSet()
	out := map[string]*ast.File{}
	err := filepath.WalkDir(moduleRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "testdata", "node_modules", ".upstream":
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
		rel, rerr := filepath.Rel(moduleRoot, path)
		if rerr != nil {
			return rerr
		}
		out[filepath.ToSlash(rel)] = f
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", moduleRoot, err)
	}
	if len(out) < 100 {
		t.Fatalf("scanned %d source files — the walk is not reaching the module and every scan below would pass over nothing", len(out))
	}
	return out, fset
}

// ── scan 1: nobody drops the known bit ──────────────────────────────────

// discardedKnownBit is the one hole the type deliberately leaves open:
// `n, _ := x.Observed()`. It is legal Go and occasionally correct, and it
// is also exactly the line that produced C4's idle floor and the v247
// disarm. Leaving it possible but visible is the trade; this is the
// visibility.
var discardedKnownBitExempt = map[string]string{}

func TestObservedIsNeverReadWithADiscardedKnownBit(t *testing.T) {
	files, fset := scanFiles(t)
	reads, dropped := 0, 0
	used := map[string]bool{}
	for path, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
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
			dropped++
			// Keyed on the LINE. The identifier is always "_", so a
			// file-scoped key would let one exemption cover every future
			// dropped bit in the same file.
			key := fmt.Sprintf("%s:%d", path, fset.Position(id.Pos()).Line)
			if _, exempt := discardedKnownBitExempt[key]; exempt {
				used[key] = true
				return true
			}
			t.Errorf("%s:%d: reads an observed.Value and discards the known bit. "+
				"That zero is not a measurement — it is the absence of one, and this fleet has shipped "+
				"six defects spelled exactly this way. Use IsKnown() if only the evidence matters, or "+
				"OrElse(<value>) to write down what absence means here.", path, fset.Position(id.Pos()).Line)
			return true
		})
	}
	// Inertness, both directions: the migration put real .Observed() reads
	// in the tree, and a rename or a rollback that removed them would make
	// this scan pass over nothing.
	if reads < 8 {
		t.Fatalf("found %d two-value Observed() reads across the module, expected the migrated call sites (%d dropped) — this scan is inert", reads, dropped)
	}
	for key := range discardedKnownBitExempt {
		if !used[key] {
			t.Errorf("exemption %q matches no read: it is STALE (the line moved, or the read is gone). Re-point it or delete it.", key)
		}
	}
}

// ── scan 2: no new (value, bool) field pair ─────────────────────────────

// pairSuffixes are the ways this repo has spelled "and here is whether
// the value beside me means anything".
var pairSuffixes = []string{"Seen", "Known", "Reported", "OK", "Ok", "Valid", "Set", "Present", "Measured", "Available"}

// evidencePairExempt is the written-reason table. An entry is a claim
// that the pair is NOT an evidence carrier; it is not a way to postpone
// the migration.
var evidencePairExempt = map[string]string{
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
	"internal/vibe/fleetapi/savings.go:Power": "accumulator + did-anything-contribute; the one ungated read (dayNet.net) is C7b's declared err-large payback term, disclosed by powerGapNote",
	"internal/vibe/fleetapi/savings.go:power": "same pair, the unexported window aggregate; every read of this one IS gated on powerKnown",
}

func TestNoNewValueAndKnownBitFieldPair(t *testing.T) {
	files, fset := scanFiles(t)
	structs, used := 0, map[string]bool{}
	for path, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			st, ok := n.(*ast.StructType)
			if !ok || st.Fields == nil {
				return true
			}
			structs++
			types := map[string]ast.Expr{}
			pos := map[string]token.Pos{}
			for _, fl := range st.Fields.List {
				for _, id := range fl.Names {
					types[id.Name] = fl.Type
					pos[id.Name] = id.Pos()
				}
			}
			for name := range types {
				for _, sfx := range pairSuffixes {
					partner, ok := types[name+sfx]
					if !ok || !boolish(partner) {
						continue
					}
					key := path + ":" + name
					if _, exempt := evidencePairExempt[key]; exempt {
						used[key] = true
						continue
					}
					t.Errorf("%s:%d: %s is paired with %s%s. That is the (value, known) shape "+
						"observed.Value exists to replace: two fields that must agree, where every way of "+
						"losing the second one — a map miss, a dropped return, a delete — yields a "+
						"confident zero. Use observed.Value[T], or add %s to evidencePairExempt with the "+
						"reason it is not an evidence carrier.",
						path, fset.Position(pos[name]).Line, name, name, sfx, key)
				}
			}
			return true
		})
	}
	if structs < 100 {
		t.Fatalf("examined %d struct types — the scan is inert", structs)
	}
	t.Logf("examined %d struct types", structs)
	for key := range evidencePairExempt {
		if !used[key] {
			t.Errorf("exemption %q matches no field pair: it is STALE. Re-point it or delete it — "+
				"an exemption nobody can reach is a hole nobody is watching.", key)
		}
	}
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

var measurementReturnExempt = map[string]string{
	// A pure arithmetic helper over a slice the caller owns: "false" means
	// the slice was empty, which the caller checked before calling. No
	// evidence crosses a boundary here.
	"internal/vibe/modelprobe/modelprobe.go:median": "arithmetic over a caller-owned slice; the bool is 'the input was empty'",
	// A binary search over the vendored price table: the bool is "no
	// snapshot on or before this day", and every caller renders the
	// unpriced reason rather than a rate.
	"internal/vibe/prices/prices.go:resolveIndex": "table lookup; absence is rendered as an unpriced reason, never as a rate of 0",
	// A cron evaluator: the bool is "this spec never fires within 8
	// years", a property of the SPEC, not an observation of the fleet.
	"internal/vibe/fleetapi/warmsched.go:nextFire": "a property of the cron spec, not an observation; the false branch disables the entry loudly",
	// C7b's power term, same pair as evidencePairExempt above.
	"internal/vibe/fleetapi/savings.go:cellPowerCost": "the declared-power accumulator's reader; see evidencePairExempt",
	// vamp, not fleet: three string/duration parsers whose bool means "the
	// input did not contain one", checked by the caller on the next line.
	// Listed rather than migrated because vamp has no evidence axis — no
	// guard anywhere reads these as a claim about a machine.
	"internal/vamp/ffmpeg_executor.go:trailingInt":      "parses a trailing integer out of a filename; false means there was none",
	"internal/vamp/webhook_executor.go:parseRetryAfter": "parses an HTTP Retry-After header; false means the header was absent or unparseable",
	"internal/vamp/timing.go:overheadPercent":           "arithmetic over a caller-owned report; false means the denominator was zero",
}

func TestNoNewMeasurementAndBoolReturn(t *testing.T) {
	files, fset := scanFiles(t)
	funcs, used := 0, map[string]bool{}
	for path, f := range files {
		for _, d := range f.Decls {
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
			if _, exempt := measurementReturnExempt[key]; exempt {
				used[key] = true
				continue
			}
			t.Errorf("%s:%d: %s returns a measurement plus a bool. Return observed.Value[T] instead — "+
				"the bool is droppable and the value's zero is a plausible measurement, which is the "+
				"combination that disarmed eight busy guards on the v247 wire. If this is a comma-ok "+
				"lookup rather than evidence, add %s to measurementReturnExempt with the reason.",
				path, fset.Position(fn.Pos()).Line, fn.Name.Name, key)
		}
	}
	if funcs < 500 {
		t.Fatalf("examined %d functions with results — the scan is inert", funcs)
	}
	t.Logf("examined %d functions with results", funcs)
	for key := range measurementReturnExempt {
		if !used[key] {
			t.Errorf("exemption %q matches no function: it is STALE. Re-point it or delete it.", key)
		}
	}
}

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
