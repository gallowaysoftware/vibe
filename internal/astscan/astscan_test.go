package astscan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleDir = "testdata/sample"

func baseRule() Rule {
	return Rule{
		Name:         "sample",
		Dir:          sampleDir,
		Trigger:      []string{"send"},
		Require:      []string{"guard"},
		Exempt:       map[string]string{"ExemptOnPurpose": "the fixture's declared exemption"},
		MinProducers: 3,
		Because:      "because the fixture says so",
	}
}

// TestRuleFindsTheUnguardedProducer is the CATCHES half: a rule that
// cannot fail is decoration.
func TestRuleFindsTheUnguardedProducer(t *testing.T) {
	r := baseRule()
	res, err := r.Check()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 1 || res.Findings[0].Func != "Unguarded" {
		t.Fatalf("findings = %v, want exactly Unguarded", res.Findings)
	}
	if err := r.Err(res); err == nil || !strings.Contains(err.Error(), "Unguarded") {
		t.Fatalf("Err = %v, want a failure naming Unguarded", err)
	}
	if !strings.Contains(r.Err(res).Error(), r.Because) {
		t.Error("the failure does not carry Because: the next agent gets a rule name and no reason")
	}
	// Guarded passed and Bystander was never a candidate: a scan that
	// counts every function in the package has no usable floor.
	for _, p := range res.Producers {
		if strings.HasSuffix(p, ":Bystander") {
			t.Error("a function that neither triggers nor guards was counted as a producer")
		}
	}
	if len(res.Producers) != 3 {
		t.Fatalf("producers = %v, want the three that call send", res.Producers)
	}
}

// TestExemptionSuppressesExactlyOneFunction: an exemption is per
// identifier, not a family. C16's review found the opposite spelling —
// an exemption that spread to a neighbouring function — by mutation.
func TestExemptionSuppressesExactlyOneFunction(t *testing.T) {
	r := baseRule()
	res, err := r.Check()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range res.Findings {
		if f.Func == "ExemptOnPurpose" {
			t.Fatal("the declared exemption did not suppress its own function")
		}
	}
	r.Exempt = map[string]string{"Guarded": "wrong function on purpose"}
	res, err = r.Check()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range res.Findings {
		names[f.Func] = true
	}
	if !names["Unguarded"] || !names["ExemptOnPurpose"] {
		t.Fatalf("exempting Guarded suppressed something else: %v", res.Findings)
	}
}

// TestInertScanIsAnError is the NOT-INERT half. A rule whose trigger no
// longer matches anything passes silently, which is how a refactor
// retires a guard nobody notices is gone.
func TestInertScanIsAnError(t *testing.T) {
	r := baseRule()
	r.Trigger = []string{"aVerbThatWasRenamedTwoPhasesAgo"}
	res, err := r.Check()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 0 {
		t.Fatalf("a trigger that matches nothing produced findings: %v", res.Findings)
	}
	err = r.Err(res)
	if err == nil {
		t.Fatal("a scan that examined zero producers reported success")
	}
	if !strings.Contains(err.Error(), "INERT") {
		t.Fatalf("Err = %v, want it to say the scan is inert", err)
	}
}

// TestStaleExemptionIsAnError: C16's stale-exemption idea, generalised.
// An exemption that matches nothing is either a rename nobody re-pointed
// or a guard that quietly stopped applying — both are holes.
func TestStaleExemptionIsAnError(t *testing.T) {
	r := baseRule()
	r.Exempt = map[string]string{
		"ExemptOnPurpose": "still real",
		"DeletedLastYear": "no longer exists",
	}
	res, err := r.Check()
	if err != nil {
		t.Fatal(err)
	}
	if got := res.UnusedExemptions; len(got) != 1 || got[0] != "DeletedLastYear" {
		t.Fatalf("UnusedExemptions = %v, want [DeletedLastYear]", got)
	}
	err = r.Err(res)
	if err == nil || !strings.Contains(err.Error(), "STALE") {
		t.Fatalf("Err = %v, want a stale-exemption failure", err)
	}
}

// TestUnreadableDirIsAnError: a scan that cannot run must fail, never
// pass over nothing. The whole class of "the check was green because it
// never executed" lives here.
func TestUnreadableDirIsAnError(t *testing.T) {
	r := baseRule()
	r.Dir = "testdata/there-is-no-such-package"
	if _, err := r.Check(); err == nil {
		t.Fatal("a missing directory scanned clean")
	}
}

// TestForeignDirStopsAtANestedCheckout is the fixture form of the bug that
// made every module-wide scan in this repo fail locally and pass in CI.
// Both directions: the module's own directories must NOT be foreign, or
// the walk stops at the first subdirectory and every scan goes inert.
func TestForeignDirStopsAtANestedCheckout(t *testing.T) {
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
	// The module itself: a go.mod and a real .git directory at the root.
	write("go.mod", "module example.com/m\n")
	if err := os.MkdirAll(filepath.Join(root, ".git", "refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	write("internal/pkg/pkg.go", "package pkg\n")
	// A git WORKTREE parked inside the tree: .git is a FILE. This is the
	// shape that was actually there — nine of them, one per agent.
	write(".claude/worktrees/agent-a1/.git", "gitdir: /home/x/src/m/.git/worktrees/agent-a1\n")
	write(".claude/worktrees/agent-a1/internal/pkg/pkg.go", "package pkg\n")
	// A nested clone somewhere with no telltale name at all.
	if err := os.MkdirAll(filepath.Join(root, "tmp", "checkout", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A nested MODULE, which is somebody else's source even without a .git.
	write("contrib/vendored/go.mod", "module example.com/other\n")

	for _, rel := range []string{".claude/worktrees/agent-a1", "tmp/checkout", "contrib/vendored"} {
		if !ForeignDir(root, filepath.Join(root, rel)) {
			t.Errorf("%s: not reported foreign — the walk descends into a second copy of the module, "+
				"which inflates every inertness floor and flags files no exemption key can name", rel)
		}
	}
	for _, rel := range []string{".", "internal", "internal/pkg", ".claude", ".claude/worktrees", "contrib"} {
		if ForeignDir(root, filepath.Join(root, rel)) {
			t.Errorf("%s: reported foreign — this module's own tree is being skipped, and a scan that "+
				"walks nothing passes forever", rel)
		}
	}
	// The root is reached as the walk's own first argument, in whatever
	// spelling the caller used. It has both markers and must never be
	// foreign, or the walk stops before it starts.
	for _, spelling := range []string{root, root + "/", filepath.Join(root, "internal", "..")} {
		if ForeignDir(root, spelling) {
			t.Errorf("the walk root spelled %q was reported foreign: the scan would examine zero files", spelling)
		}
	}
}

// TestCleanRuleReportsNoError closes the loop: with the fixture's one
// defect exempted, the rule is silent. A check nobody can satisfy gets
// deleted by the next agent.
func TestCleanRuleReportsNoError(t *testing.T) {
	r := baseRule()
	r.Exempt = map[string]string{
		"ExemptOnPurpose": "the fixture's declared exemption",
		"Unguarded":       "the fixture's defect, exempted here to prove the rule can be satisfied",
	}
	res, err := r.Check()
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Err(res); err != nil {
		t.Fatalf("a fully-exempted rule still failed: %v", err)
	}
}
