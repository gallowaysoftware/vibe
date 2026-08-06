package mutation

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// The mutation harness (fleet-control C20). Two tests, deliberately
// split by cost:
//
//   - TestMutationRegistryIsCurrent runs in the ordinary suite. It
//     compiles nothing and takes milliseconds, and it is what catches the
//     STALE half: a refactor that moves a guarded line, or a rename that
//     detaches an entry from the test it names. A registry that has
//     quietly stopped pointing at anything is worse than no registry,
//     because the phase docs cite it.
//   - TestMutationsAreCaught runs only under VIBE_MUTATION_TEST=1, in its
//     own CI job beside the conformance matrix. It is the expensive half:
//     one package rebuild per entry.
//
// Env-gated rather than build-tagged, following internal/swaptest's
// LLAMA_SWAP_BIN precedent and AGENTS.md's rule: this module has zero
// //go:build lines, and a tagged-out file is skipped by vet and the
// linter and rots uncompiled.

const gateEnv = "VIBE_MUTATION_TEST"

// ── the cheap half ───────────────────────────────────────────────────

func TestMutationRegistryIsCurrent(t *testing.T) {
	root := repoRoot(t)
	if len(Registry) < 10 {
		t.Fatalf("registry holds %d entries — too few to be the thing the phase docs cite", len(Registry))
	}
	names := map[string]bool{}
	for _, m := range Registry {
		t.Run(m.Name, func(t *testing.T) {
			if names[m.Name] {
				t.Fatal("duplicate entry name: output and the phase doc table would be ambiguous")
			}
			names[m.Name] = true
			if m.Why == "" {
				t.Error("no Why: an entry that fails must say what breaks in the FLEET, not leave the next agent reading a diff")
			}
			if len(m.MustFail) == 0 {
				t.Fatal("no MustFail: an entry with nothing to assert is a mutation that always 'passes'")
			}
			n, err := m.Occurrences(root)
			if err != nil {
				t.Fatalf("%s: %v — the entry points at a file that is not there", m.File, err)
			}
			if n != 1 {
				t.Fatalf("Find matches %d times in %s, want exactly 1. This entry is STALE: the "+
					"production line it guards was moved, edited or duplicated, and the mutation would "+
					"either not apply or apply to the wrong copy. Re-point it — do NOT delete it to make "+
					"this green, because that silently retires the coverage the phase doc claims.", n, m.File)
			}
			if m.Replace == m.Find {
				t.Error("Replace equals Find: the mutation is a no-op and every named test would pass")
			}
			// A named test that no longer exists cannot go red. Checked
			// here, cheaply, rather than only in the expensive job — a
			// renamed test is the commonest way an entry detaches.
			for _, name := range m.MustFail {
				if !testExists(t, filepath.Join(root, strings.TrimPrefix(m.Pkg, "./")), name) {
					t.Errorf("MustFail names %s, which is not declared in %s. A test that does not exist "+
						"cannot fail, so this entry asserts nothing.", name, m.Pkg)
				}
			}
		})
	}
}

var testDecl = regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]*)\(`)

func testExists(t *testing.T, dir, name string) bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, m := range testDecl.FindAllStringSubmatch(string(b), -1) {
			if m[1] == name {
				return true
			}
		}
	}
	return false
}

// ── the expensive half ───────────────────────────────────────────────

func TestMutationsAreCaught(t *testing.T) {
	if os.Getenv(gateEnv) != "1" {
		t.Skipf("set %s=1 to run the mutation harness (its own CI job; ~one package rebuild per entry)", gateEnv)
	}
	root := repoRoot(t)
	start := time.Now()

	// Baseline first, on ONE copy: every named test must currently PASS.
	// Without it, "the mutation made it fail" is unfalsifiable — a test
	// that was already red, or that never ran, would read as a catch.
	base := filepath.Join(t.TempDir(), "baseline")
	if err := CopyTree(root, base); err != nil {
		t.Fatalf("copy tree: %v", err)
	}
	byPkg := map[string]map[string]bool{}
	for _, m := range Registry {
		if byPkg[m.Pkg] == nil {
			byPkg[m.Pkg] = map[string]bool{}
		}
		for _, name := range m.MustFail {
			byPkg[m.Pkg][name] = true
		}
	}
	for pkg, tests := range byPkg {
		res, out, err := runTests(t, base, pkg, keys(tests))
		if err != nil && len(res) == 0 {
			t.Fatalf("baseline %s did not run: %v\n%s", pkg, err, out)
		}
		for name := range tests {
			switch got, ok := res[name]; {
			case !ok:
				t.Errorf("baseline: %s did not RUN in %s — every entry naming it asserts nothing", name, pkg)
			case !got:
				t.Errorf("baseline: %s is ALREADY FAILING in %s — a mutation cannot be shown to have caused that", name, pkg)
			}
		}
	}
	if t.Failed() {
		t.Fatal("baseline is not clean; the mutation results below would be meaningless")
	}
	t.Logf("baseline clean over %d package(s) in %s", len(byPkg), time.Since(start).Round(time.Millisecond))

	// Then the mutations, one worker per tree copy.
	workers := min(4, len(Registry))
	queue := make(chan Mutation)
	var wg sync.WaitGroup
	var mu sync.Mutex
	type outcome struct {
		name string
		err  error
		dur  time.Duration
	}
	var outcomes []outcome

	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			tree := filepath.Join(t.TempDir(), fmt.Sprintf("w%d", w))
			if err := CopyTree(root, tree); err != nil {
				mu.Lock()
				outcomes = append(outcomes, outcome{name: fmt.Sprintf("worker %d", w), err: err})
				mu.Unlock()
				return
			}
			for m := range queue {
				t0 := time.Now()
				err := runOne(t, tree, m)
				mu.Lock()
				outcomes = append(outcomes, outcome{m.Name, err, time.Since(t0)})
				mu.Unlock()
			}
		}(w)
	}
	for _, m := range Registry {
		queue <- m
	}
	close(queue)
	wg.Wait()

	caught := 0
	for _, o := range outcomes {
		if o.err != nil {
			t.Errorf("%s: %v", o.name, o.err)
			continue
		}
		caught++
		t.Logf("caught  %-58s %s", o.name, o.dur.Round(time.Millisecond))
	}
	t.Logf("%d/%d guards mutation-verified in %s", caught, len(Registry), time.Since(start).Round(time.Second))
}

// ── the runner's own guards ──────────────────────────────────────────

// TestRunnerRejectsAStaleFind: the cheap half of the staleness rule, at
// the Apply boundary rather than only in the registry audit. Apply must
// refuse an ambiguous or absent pattern rather than editing whichever
// copy came first.
func TestRunnerRejectsAStaleFind(t *testing.T) {
	root := repoRoot(t)
	tree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tree, "internal", "vibe", "fleetapi"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(tree, "internal", "vibe", "fleetapi", "x.go")
	if err := os.WriteFile(src, []byte("package fleetapi\n\nvar a = 1\nvar b = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = root
	for name, m := range map[string]Mutation{
		"absent":    {Name: "absent", File: "internal/vibe/fleetapi/x.go", Find: "var c = 1", Replace: "var c = 2"},
		"ambiguous": {Name: "ambiguous", File: "internal/vibe/fleetapi/x.go", Find: " = 1", Replace: " = 2"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := m.Apply(tree); err == nil {
				t.Fatal("Apply accepted a pattern that does not match exactly once")
			} else if !strings.Contains(err.Error(), "STALE") {
				t.Fatalf("err = %v, want it to name the entry as stale", err)
			}
		})
	}
	after, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != "package fleetapi\n\nvar a = 1\nvar b = 1\n" {
		t.Fatal("a refused Apply still wrote the file")
	}
}

// TestRunnerReportsAnUnprotectedGuard and
// TestRunnerRejectsANonCompilingMutation are the two ways this harness
// could lie, both exercised against the real tree. Without them the
// runner is a thing that has only ever been observed agreeing.
func TestRunnerReportsAnUnprotectedGuard(t *testing.T) {
	if os.Getenv(gateEnv) != "1" {
		t.Skipf("set %s=1 (this one compiles a package)", gateEnv)
	}
	tree := workTree(t)
	// An edit that compiles, changes nothing observable, and names a test
	// that therefore keeps passing: the exact shape of a registry entry
	// whose guard has quietly stopped being covered.
	inert := Mutation{
		Name:     "inert",
		File:     "internal/vibe/fleetapi/warmtarget.go",
		Find:     "// warmTimeout bounds one warm call.",
		Replace:  "// warmTimeout bounds one warm call (inert edit).",
		Pkg:      "./internal/vibe/fleetapi/",
		MustFail: []string{"TestWarmTarget_InFlightRequestBlocksRestore"},
		Why:      "the runner's own negative control",
	}
	err := runOne(t, tree, inert)
	if err == nil {
		t.Fatal("an inert mutation was reported as caught: this harness cannot tell a working guard from an unprotected one")
	}
	if !strings.Contains(err.Error(), "UNPROTECTED") {
		t.Fatalf("err = %v, want the unprotected-guard finding", err)
	}
}

func TestRunnerRejectsANonCompilingMutation(t *testing.T) {
	if os.Getenv(gateEnv) != "1" {
		t.Skipf("set %s=1 (this one compiles a package)", gateEnv)
	}
	tree := workTree(t)
	broken := Mutation{
		Name:     "does-not-compile",
		File:     "internal/vibe/fleetapi/warmtarget.go",
		Find:     "// warmTimeout bounds one warm call.",
		Replace:  "this is not go",
		Pkg:      "./internal/vibe/fleetapi/",
		MustFail: []string{"TestWarmTarget_InFlightRequestBlocksRestore"},
		Why:      "the runner's own negative control",
	}
	err := runOne(t, tree, broken)
	if err == nil {
		t.Fatal("a mutation that does not compile was counted as a caught guard — every entry could be satisfied by a syntax error")
	}
	if !strings.Contains(err.Error(), "does not compile") {
		t.Fatalf("err = %v, want the build-failure diagnosis", err)
	}
}

func workTree(t *testing.T) string {
	t.Helper()
	tree := filepath.Join(t.TempDir(), "tree")
	if err := CopyTree(repoRoot(t), tree); err != nil {
		t.Fatalf("copy tree: %v", err)
	}
	return tree
}

// runOne applies one mutation, runs its named tests, restores the file
// and judges the result.
func runOne(t *testing.T, tree string, m Mutation) error {
	t.Helper()
	original, err := m.Apply(tree)
	if err != nil {
		return err
	}
	defer func() {
		if rerr := m.Restore(tree, original); rerr != nil {
			t.Errorf("%s: restore failed, this worker's tree is now poisoned: %v", m.Name, rerr)
		}
	}()

	res, out, err := runTests(t, tree, m.Pkg, m.MustFail)
	if len(res) == 0 {
		// No per-test verdict at all: the mutated tree did not build, or
		// the package failed before any test ran. Either way this entry
		// proves nothing about the guard — a mutation must break the
		// GUARD, not the compiler.
		return fmt.Errorf("the mutated tree produced no test verdicts (%v). A mutation that does not "+
			"compile proves nothing about %s. Fix the Replace text.\n%s", err, m.Why, tail(out, 25))
	}
	var passed []string
	for _, name := range m.MustFail {
		got, ok := res[name]
		if !ok {
			return fmt.Errorf("%s did not RUN under the mutation — the entry names a test that no longer exists here", name)
		}
		if got {
			passed = append(passed, name)
		}
	}
	if len(passed) == len(m.MustFail) {
		return fmt.Errorf("mutation applied and every named test still PASSED (%s). This guard is "+
			"UNPROTECTED: %s", strings.Join(passed, ", "), m.Why)
	}
	return nil
}

var (
	runLine    = regexp.MustCompile(`^=== RUN\s+(Test[A-Za-z0-9_]*)`)
	verdictRe  = regexp.MustCompile(`^\s*--- (PASS|FAIL|SKIP): (Test[A-Za-z0-9_]*)`)
	testBinary = "go"
)

// runTests runs exactly the named tests in one package and returns a
// per-test pass/fail map built from `go test -v`'s own output.
//
// The map is deliberately not derived from the process exit status: `go
// test` exits non-zero for a build failure, a panic and an ordinary
// failure alike, and this harness has to tell "the guard fired" from "the
// mutation did not compile". A SKIP is recorded as neither — a skipped
// test cannot be evidence of anything.
func runTests(t *testing.T, tree, pkg string, names []string) (map[string]bool, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	pattern := "^(" + strings.Join(names, "|") + ")$"
	cmd := exec.CommandContext(ctx, testBinary, "test", "-count=1", "-timeout", "240s", "-v", "-run", pattern, pkg)
	cmd.Dir = tree
	raw, err := cmd.CombinedOutput()
	out := string(raw)

	res := map[string]bool{}
	ran := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		if m := runLine.FindStringSubmatch(line); m != nil {
			ran[m[1]] = true
			continue
		}
		if m := verdictRe.FindStringSubmatch(line); m != nil {
			switch m[1] {
			case "PASS":
				res[m[2]] = true
			case "FAIL":
				res[m[2]] = false
			}
		}
	}
	// A test that ran and produced no top-level verdict line has panicked
	// the package; count it as failed rather than missing, so a mutation
	// that makes a guard panic still reads as caught by that guard.
	for name := range ran {
		if _, ok := res[name]; !ok {
			res[name] = false
		}
	}
	return res, out, err
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := RepoRoot(".")
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	return root
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
