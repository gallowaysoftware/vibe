package observed

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The guards ON the three scans, in both directions.
//
// C20 gave each scan a floor and a written-reason exemption table, and its
// own review then found that five of its ten findings were checks that
// scanned a real violation clean. The lesson it drew — a check that does
// not catch is worse than no check, because it manufactures confidence —
// applies to the floors as much as to the rules: a floor nobody has
// watched fail is a number, not a guard. So each one is planted against
// here: the violation must go RED naming the thing, and the scan must FAIL
// when its own target set is empty.

// ── the walk stops at this module's boundary ────────────────────────────

// TestScanWalkStaysInsideThisModule is the fixture form of a bug that was
// live in the tree: the walk descended into the git worktrees this repo
// keeps under .claude/worktrees/, so the scans examined a second (and
// third, and ninth) copy of the module. It failed for anyone with a
// worktree and passed in CI, where a fresh clone has none.
func TestScanWalkStaysInsideThisModule(t *testing.T) {
	// One violating function, copied into four places. Only the one that
	// belongs to this module may be scanned.
	const violation = "package a\n\nfunc Elapsed() (int, bool) { return 0, false }\n"
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/m\n")
	write(".git/HEAD", "ref: refs/heads/main\n")
	write("internal/a/a.go", violation)
	// A parallel agent's worktree: .git is a FILE, so a walk that skips
	// only directories named .git goes straight in.
	write(".claude/worktrees/agent-a1/.git", "gitdir: /elsewhere/.git/worktrees/agent-a1\n")
	write(".claude/worktrees/agent-a1/internal/a/a.go", violation)
	// A nested checkout with an innocuous name.
	write("tmp/checkout/.git/HEAD", "ref: refs/heads/main\n")
	write("tmp/checkout/internal/a/a.go", violation)
	// A nested module, and a testdata fixture: neither is this module's
	// code and neither follows its conventions.
	write("contrib/other/go.mod", "module example.com/other\n")
	write("contrib/other/a.go", violation)
	write("internal/a/testdata/fixture.go", violation)

	files, fset, err := moduleGoFiles(root)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	got := sortedKeys(files)
	if len(got) != 1 || got[0] != "internal/a/a.go" {
		t.Fatalf("walked %v, want exactly [internal/a/a.go] — a foreign tree is being scanned as if it "+
			"were this module's source", got)
	}
	// The denominator, which is the half that damages the floors: five
	// copies of one function must count as one.
	findings, funcs := findMeasurementReturns(files, fset)
	if funcs != 1 || len(findings) != 1 {
		t.Fatalf("examined %d functions and found %d violations, want 1 and 1: foreign trees inflate the "+
			"inertness floor until it can no longer notice this module's own code going away", funcs, len(findings))
	}
	if !strings.Contains(findings[0].Key, "internal/a/a.go") {
		t.Fatalf("finding %q is not keyed on a repo-relative path an exemption could name", findings[0].Key)
	}
}

// TestModuleWalkReachesTheRealModule pins the live walk from the other
// side: the fixture above proves the exclusions work, and this proves they
// did not exclude everything. Together they are the both-directions pair.
func TestModuleWalkReachesTheRealModule(t *testing.T) {
	files, _ := moduleFiles(t)
	for _, want := range []string{
		"internal/vibe/observed/observed.go",
		"internal/vibe/fleetapi/fleetapi.go",
		"internal/astscan/astscan.go",
		"internal/vamp/timing.go",
		"cmd/vibe/main.go",
	} {
		if _, ok := files[want]; !ok {
			t.Errorf("%s was not walked: a package this scan is supposed to cover is invisible to it", want)
		}
	}
	t.Logf("walked %d source files", len(files))
}

// ── the floors and the exemption tables are load-bearing ────────────────

// TestScanGuardsAreLoadBearing drops each scan's target set to nothing and
// requires the scan to FAIL. C20 wrote these floors; nobody had watched
// one fire. The same test proves the stale-exemption rule fires, which is
// the other half of what makes an exemption table a mechanism rather than
// a comment.
func TestScanGuardsAreLoadBearing(t *testing.T) {
	for _, s := range []scan{discardedKnownBitScan, evidencePairScan, measurementReturnScan} {
		t.Run(s.Name, func(t *testing.T) {
			if s.Floor <= 0 {
				t.Fatal("no floor: this scan passes over an empty tree, which is what a rename looks like")
			}
			// Direction 1: nothing examined. The exemption table is emptied
			// first so the floor is the only thing that can speak — with the
			// live table in place, every entry would also read as stale over
			// an empty tree and the assertion would pass for the wrong reason.
			empty := s
			empty.Exempt = nil
			problems := empty.audit(nil, 0)
			if len(problems) == 0 {
				t.Fatal("a scan that examined NOTHING reported success: the floor is decorative")
			}
			if !strings.Contains(strings.Join(problems, "\n"), "INERT") {
				t.Fatalf("problems = %v, want the inertness finding", problems)
			}
			// One under the floor still fails; one over it does not. A floor
			// that is off by a whole tree is not a floor.
			if got := empty.audit(nil, s.Floor-1); len(got) != 1 {
				t.Fatalf("examined = floor-1 gave %v, want exactly the inertness finding", got)
			}
			if got := empty.audit(nil, s.Floor); len(got) != 0 {
				t.Fatalf("examined = floor gave %v, want silence — the rule must be satisfiable", got)
			}

			// Direction 2: an exemption that matches nothing.
			stale := s
			stale.Exempt = map[string]string{"internal/vibe/gone/gone.go:deleted": "renamed two phases ago"}
			got := stale.audit(nil, s.Floor)
			if len(got) != 1 || !strings.Contains(got[0], "STALE") {
				t.Fatalf("problems = %v, want exactly the stale-exemption finding", got)
			}
			// …and an exemption that DOES match suppresses exactly its own
			// key, not its neighbours.
			live := s
			live.Exempt = map[string]string{"a.go:x": "declared"}
			got = live.audit([]finding{{Key: "a.go:x", Detail: "x"}, {Key: "a.go:y", Detail: "y"}}, s.Floor)
			if len(got) != 1 || got[0] != "y" {
				t.Fatalf("problems = %v, want only the unexempted finding: an exemption is per key", got)
			}
		})
	}
}

// TestEveryLiveExemptionIsReachable is the stale-exemption rule applied to
// the tables as they actually stand. audit() folds it in, but only where a
// scan's other guards are also green — this asserts it directly, so a
// reason that has stopped describing anything is named on its own.
func TestEveryLiveExemptionIsReachable(t *testing.T) {
	files, fset := moduleFiles(t)
	f1, _ := findDiscardedKnownBits(files, fset)
	f2, _ := findEvidencePairs(files, fset)
	f3, _ := findMeasurementReturns(files, fset)
	for _, tc := range []struct {
		s scan
		f []finding
	}{
		{discardedKnownBitScan, f1},
		{evidencePairScan, f2},
		{measurementReturnScan, f3},
	} {
		seen := map[string]bool{}
		for _, f := range tc.f {
			seen[f.Key] = true
		}
		for key, reason := range tc.s.Exempt {
			if !seen[key] {
				t.Errorf("%s: exemption %q (%q) matches no site in the module: it is STALE", tc.s.Name, key, reason)
			}
			if strings.TrimSpace(reason) == "" {
				t.Errorf("%s: exemption %q has no written reason, which is the whole mechanism", tc.s.Name, key)
			}
		}
	}
}

// ── each rule catches a planted violation ───────────────────────────────

func parseSources(t *testing.T, src map[string]string) (map[string]*ast.File, *token.FileSet) {
	t.Helper()
	fset := token.NewFileSet()
	out := map[string]*ast.File{}
	for name, body := range src {
		f, err := parser.ParseFile(fset, name, body, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		out[name] = f
	}
	return out, fset
}

func TestDiscardedKnownBitScanCatchesAPlantedRead(t *testing.T) {
	files, fset := parseSources(t, map[string]string{
		"internal/vibe/x/x.go": `package x

func bad(s *S, cell string) int {
	n, _ := s.inFlight[cell].Observed()
	return n
}

func good(s *S, cell string) int {
	n, reported := s.inFlight[cell].Observed()
	if !reported {
		return -1
	}
	return n
}

func alsoGood(s *S, cell string) bool { return s.inFlight[cell].IsKnown() }

func notThisMethod(s *S) (int, bool) { return s.Other() }
`,
	})
	findings, reads := findDiscardedKnownBits(files, fset)
	if reads != 2 {
		t.Fatalf("counted %d two-value Observed() reads, want 2 (the denominator the floor is built on)", reads)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly the discarded bit in bad()", findings)
	}
	if findings[0].Key != "internal/vibe/x/x.go:4" {
		t.Fatalf("key = %q, want the file and LINE — a file-scoped key would let one exemption cover every "+
			"future dropped bit in the same file", findings[0].Key)
	}
	if !strings.Contains(findings[0].Detail, "discards the known bit") {
		t.Fatalf("detail = %q, want it to name what is wrong", findings[0].Detail)
	}
}

func TestEvidencePairScanCatchesAPlantedPair(t *testing.T) {
	files, fset := parseSources(t, map[string]string{
		"internal/vibe/x/x.go": `package x

type S struct {
	residentGB      float64
	residentGBKnown bool

	leases    map[string]Lease
	leasesSet bool

	name  string
	other int
}

type NotAPair struct {
	Count    int
	CountFor string
}
`,
	})
	findings, structs := findEvidencePairs(files, fset)
	if structs != 2 {
		t.Fatalf("counted %d structs, want 2", structs)
	}
	keys := map[string]bool{}
	for _, f := range findings {
		keys[f.Key] = true
	}
	for _, want := range []string{"internal/vibe/x/x.go:residentGB", "internal/vibe/x/x.go:leases"} {
		if !keys[want] {
			t.Errorf("missed %s: the (value, known-bit) pair scanned clean", want)
		}
	}
	if len(findings) != 2 {
		t.Fatalf("findings = %v, want exactly the two pairs (a string beside an int is not one)", findings)
	}
}

func TestMeasurementReturnScanCatchesAPlantedReturn(t *testing.T) {
	files, fset := parseSources(t, map[string]string{
		"internal/vibe/x/x.go": `package x

import "time"

func residentGB(cell string) (float64, bool) { return 0, false }

func lastSeen(cell string) (time.Time, bool) { return time.Time{}, false }

func lease(cell string) (Lease, bool) { return Lease{}, false }

func plainCount(cell string) int { return 0 }

func withError(cell string) (int, error) { return 0, nil }
`,
	})
	findings, funcs := findMeasurementReturns(files, fset)
	if funcs != 5 {
		t.Fatalf("counted %d functions with results, want 5 (the denominator the floor is built on)", funcs)
	}
	keys := map[string]bool{}
	for _, f := range findings {
		keys[f.Key] = true
	}
	for _, want := range []string{"internal/vibe/x/x.go:residentGB", "internal/vibe/x/x.go:lastSeen"} {
		if !keys[want] {
			t.Errorf("missed %s: a measurement plus a droppable bool scanned clean", want)
		}
	}
	// A comma-ok lookup over a record type is deliberately NOT a finding:
	// nobody mistakes a zero Lease for a real one.
	if keys["internal/vibe/x/x.go:lease"] {
		t.Error("flagged a (record, bool) comma-ok lookup: the rule would drown the module in false positives")
	}
	if len(findings) != 2 {
		t.Fatalf("findings = %v, want exactly the two measurements", findings)
	}
}
