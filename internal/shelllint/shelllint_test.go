package shelllint

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// moduleRoot is this module's own root, from this package's directory.
// C20 pointed the lint at scripts/ alone, which left install.sh — the
// script this project asks strangers to pipe into sh — outside every rule
// it has.
const moduleRoot = "../.."

// exempt is the written-reason table, keyed file:line:rule. A key moves
// with the line, on purpose: an exemption should not outlive the line it
// exempts, and re-adding it is one edit with the reason in front of you.
var exempt = map[string]string{
	// gate-c15's rig spawns llama-server processes through llama-swap, so
	// their argv carries llama-swap's own config path and not the rig's
	// $LAB — the port range is the only handle the sweep has. It is a
	// standalone range (5960-5979, spelled 59[67][0-9]) that no other rig
	// uses, but it is still a literal.
	//
	// C23 gave lab.sh the FLEETLAB_PORT_BASE knob and re-anchored ITS
	// sweep on a derived window; this rig is a separate fleet with its own
	// C15LAB_DIR and its own fixed ports, and was left alone. So the
	// hazard here is unchanged and narrower than it was: two concurrent
	// runs of gate-c15-warm-auth.sh on one box still collide. Recorded so
	// the next reader finds it rather than rediscovering it.
	"scripts/fleetlab/gate-c15-warm-auth.sh:66:unscoped-kill": "llama-server argv carries no rig path; anchored on this rig's private 5960-5979 range instead",
	"scripts/fleetlab/gate-c15-warm-auth.sh:67:unscoped-kill": "the rig's own host-probe helper, named for the gate (c15-hostprobe)",
	// The two temp-directory traps the widened walk brought in. Both are
	// `rm -rf "$VAR"` on a bare expansion, and in both the line ABOVE is
	// what makes the expansion non-empty — verified by reading, not
	// assumed:
	//
	//   install.sh:177   [ -d "$tmpdir" ] || fatal "could not create temp directory"
	//   install_test.sh  TMPROOT=$(mktemp -d …) under `set -eu`, which
	//                    aborts the script if mktemp fails.
	//
	// Neither is mine to edit in this phase and both would be strictly
	// better as ${tmpdir:?} / ${TMPROOT:?} — a one-line follow-up, at
	// which point these two entries go stale and this table says so.
	"install.sh:178:rm-rf-bare-var":              "the line above is `[ -d \"$tmpdir\" ] || fatal`, so an empty tmpdir aborts the installer before this trap is armed",
	"internal/install_test.sh:26:rm-rf-bare-var": "TMPROOT is assigned from `mktemp -d` on the line above under `set -eu`, which aborts if it fails",
}

// floors are the inertness assertions, one per rule. The file count says
// the walk ran; it says nothing about whether a rule's pattern still
// matches anything, and a repo that stopped writing `pkill` would retire
// the kill rule in silence. Each is well under the live count so ordinary
// churn does not trip it, and well over zero so a rule going dark does.
var floors = Stats{Files: 20, Cd: 8, RmRecursive: 5, Kill: 6, GitWrite: 6}

// TestScriptsAreSafe is the shell half of this phase's harness. The one
// blocker in this plan's history that was not a Go defect was a bare `cd`
// in a rig (C17 A1); nothing mechanical would have caught it.
func TestScriptsAreSafe(t *testing.T) {
	findings, stats, err := Lint(moduleRoot)
	if err != nil {
		t.Fatalf("lint %s: %v", moduleRoot, err)
	}
	t.Logf("linted %d .sh files: %d cd, %d recursive rm, %d kill, %d git write", stats.Files, stats.Cd, stats.RmRecursive, stats.Kill, stats.GitWrite)
	for _, c := range []struct {
		what       string
		got, floor int
	}{
		{".sh files", stats.Files, floors.Files},
		{"cd commands", stats.Cd, floors.Cd},
		{"recursive rm commands", stats.RmRecursive, floors.RmRecursive},
		{"pkill/killall commands", stats.Kill, floors.Kill},
		{"git write commands", stats.GitWrite, floors.GitWrite},
	} {
		if c.got < c.floor {
			t.Errorf("examined %d %s, expected at least %d: this rule is INERT — the shape it is about "+
				"is no longer in the tree, and it would now pass over anything", c.got, c.what, c.floor)
		}
		// The other way a floor stops being a guard: not by being wrong, but
		// by drifting so far below the live count that nothing can trip it.
		// Same rule and same reasoning as internal/vibe/observed's
		// floorSlack — a quarter is arbitrary, being checked is not.
		if c.floor*4 < c.got {
			t.Errorf("the floor for %s is %d against %d examined, so %d%% of them could vanish before it "+
				"complained. That is a number, not a guard — raise it.", c.what, c.floor, c.got, 100-100*c.floor/c.got)
		}
	}
	used := map[string]bool{}
	var live []string
	for _, f := range findings {
		if _, ok := exempt[f.Key()]; ok {
			used[f.Key()] = true
			continue
		}
		live = append(live, f.String())
	}
	if len(live) > 0 {
		sort.Strings(live)
		t.Errorf("%d unsafe line(s):\n  %s", len(live), strings.Join(live, "\n  "))
	}
	for key := range exempt {
		if !used[key] {
			t.Errorf("exemption %q matches no finding: it is STALE (the line moved, or the hazard is "+
				"gone). Re-point it or delete it.", key)
		}
	}
}

// TestLintReachesTheScriptsItClaimsTo is the other half of the file floor:
// a count can be met by twenty copies of the same rig. These are the
// scripts whose absence would mean the walk lost a whole subtree.
func TestLintReachesTheScriptsItClaimsTo(t *testing.T) {
	_, stats, err := Lint(moduleRoot)
	if err != nil {
		t.Fatalf("lint: %v", err)
	}
	seen := map[string]bool{}
	err = filepath.WalkDir(moduleRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir(moduleRoot, path, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".sh") {
			rel, _ := filepath.Rel(moduleRoot, path)
			seen[filepath.ToSlash(rel)] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"install.sh",
		"internal/install_test.sh",
		"scripts/fleetlab/lab.sh",
		"scripts/fleetlab/gate-c13-parity.sh",
		"scripts/smoke/llama-swap/run-smoke.sh",
		"scripts/upgrade/ritual.sh",
	} {
		if !seen[want] {
			t.Errorf("%s is not reached by the lint: a whole subtree of shell is unguarded", want)
		}
	}
	if len(seen) != stats.Files {
		t.Errorf("walked %d .sh files but Lint counted %d", len(seen), stats.Files)
	}
}

// TestLintStopsAtANestedCheckout: this repo keeps a git worktree per
// parallel agent under .claude/worktrees/, each a full second copy of
// scripts/. A walk that descends into them reports every rig twice, under
// keys no exemption can name, and fails on every developer machine while
// passing in CI. Same bug, same fix, as the Go scans in
// internal/vibe/observed.
func TestLintStopsAtANestedCheckout(t *testing.T) {
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
	const rig = "set -uo pipefail\nrm -rf \"$LAB\"\n"
	write("go.mod", "module example.com/m\n")
	write(".git/HEAD", "ref: refs/heads/main\n")
	write("scripts/rig.sh", rig)
	write(".claude/worktrees/agent-a1/.git", "gitdir: /elsewhere\n")
	write(".claude/worktrees/agent-a1/scripts/rig.sh", rig)
	write("tmp/checkout/.git/HEAD", "ref: refs/heads/main\n")
	write("tmp/checkout/scripts/rig.sh", rig)

	findings, stats, err := Lint(root)
	if err != nil {
		t.Fatalf("lint: %v", err)
	}
	if stats.Files != 1 || len(findings) != 1 {
		t.Fatalf("linted %d files and found %d hazards, want 1 and 1: a foreign checkout is being linted "+
			"as if it were this module's own scripts (findings %v)", stats.Files, len(findings), findings)
	}
	if findings[0].File != "scripts/rig.sh" {
		t.Fatalf("finding keyed %q, want a path relative to the module root", findings[0].File)
	}
}

// ── the rules themselves, proven to catch and proven not to over-fire ──

func lintText(t *testing.T, body string) []Finding {
	t.Helper()
	return lintFile("x.sh", strings.NewReader(body), &Stats{})
}

// countRule is for the lines that trip TWO rules — the C17 blocker is a
// bad `cd` AND the git write it lets loose — where asserting on the total
// would say nothing about either.
func countRule(findings []Finding, rule string) int {
	n := 0
	for _, f := range findings {
		if f.Rule == rule {
			n++
		}
	}
	return n
}

func TestUnguardedCd(t *testing.T) {
	// The C17 blocker, reduced.
	got := lintText(t, "#!/usr/bin/env bash\nset -uo pipefail\ncd \"$LAB/etc/vibe/backends\"\ngit add -A\n")
	if len(got) != 2 || got[0].Rule != RuleUnguardedCd || got[1].Rule != RuleGitWriteHere {
		t.Fatalf("findings = %v, want the unguarded cd AND the git write it enables", got)
	}
	for name, body := range map[string]string{
		"guarded by ||":     "set -uo pipefail\ncd \"$LAB\" || exit 1\n",
		"subshell and &&":   "set -uo pipefail\n( cd \"$REPO\" && go build ./... )\n",
		"errexit is on":     "set -euo pipefail\ncd \"$LAB\"\n",
		"errexit long form": "set -o errexit\nset -u\ncd \"$LAB\"\n",
		"not a cd at all":   "set -uo pipefail\necho \"cd is mentioned here\"\n",
		"a comment":         "set -uo pipefail\n# cd \"$LAB\"\n",
	} {
		if got := lintText(t, body); len(got) != 0 {
			t.Errorf("%s: false positive %v", name, got)
		}
	}
	// set -e AFTER the cd does not retroactively guard it.
	if got := lintText(t, "cd \"$LAB\"\nset -e\n"); len(got) != 1 {
		t.Errorf("a cd before `set -e` was treated as guarded: %v", got)
	}
	// An && that comes BEFORE the cd chains off something else. This is
	// the C17 blocker verbatim, and it now trips both halves of it.
	got = lintText(t, "set -uo pipefail\ncd \"$LAB\"; git init && git add -A\n")
	if countRule(got, RuleUnguardedCd) != 1 {
		t.Errorf("an && to the RIGHT of the cd was read as guarding it: %v", got)
	}
	if countRule(got, RuleGitWriteHere) != 2 {
		t.Errorf("the two unpinned git writes the failed cd lets loose were not both flagged: %v", got)
	}
	// The MIRROR image, which the self-review's REV-4 left open: the &&
	// short-circuits `git init`, and then the `;` starts a fresh command
	// that runs in the operator's directory regardless. An && guards only
	// what is left of the next `;`.
	got = lintText(t, "set -uo pipefail\ncd \"$LAB\" && git init; git add -A\n")
	if countRule(got, RuleUnguardedCd) != 1 {
		t.Errorf("an && chain followed by `; more` was read as guarding the whole line: %v", got)
	}
	// …and the same shape with the trailing statement removed is still
	// guarded, or the rule is unsatisfiable by the idiom it recommends.
	if got := lintText(t, "set -uo pipefail\n( cd \"$REPO\" && go build ./... ) || die \"build failed\"\n"); len(got) != 0 {
		t.Errorf("the repo's own guarded-subshell idiom now over-fires: %v", got)
	}
	// A trailing comment neither guards a cd nor hides one.
	if got := lintText(t, "set -uo pipefail\ncd \"$LAB\"  # || exit 1 would be nice\n"); len(got) != 1 {
		t.Errorf("a `||` inside a COMMENT was read as guarding the cd: %v", got)
	}
	// C21: a cd in command position after a KEYWORD. `then cd "$LAB"` was
	// invisible — the rule only knew about line starts and punctuation, so
	// the single commonest way to write a conditional chdir scanned clean.
	for name, body := range map[string]string{
		"after then": "set -uo pipefail\nif [ -n \"$LAB\" ]; then cd \"$LAB\"; fi\n",
		"after do":   "set -uo pipefail\nfor d in a b; do cd \"$d\"; done\n",
		"after else": "set -uo pipefail\nif false; then :; else cd \"$LAB\"; fi\n",
		// Indented inside a function: the shape every rig helper has.
		"indented in a function": "set -uo pipefail\nsetup() {\n  cd \"$LAB\"\n}\n",
	} {
		got := lintText(t, body)
		if len(got) != 1 || got[0].Rule != RuleUnguardedCd {
			t.Errorf("%s: findings = %v, want one unguarded-cd", name, got)
		}
	}
}

func TestRmRfBareVar(t *testing.T) {
	for name, body := range map[string]string{
		"double quoted": "set -uo pipefail\nrm -rf \"$LAB\"\n",
		"unquoted":      "set -uo pipefail\nrm -rf $LAB\n",
		"braced":        "set -uo pipefail\nrm -rf \"${LAB}/state\"\n",
		"flags apart":   "set -uo pipefail\nrm -r -f \"$LAB\"\n",
		// `--` is the CAREFUL spelling and this repo already uses it
		// (`cd -- "$(dirname …)"`), so it must not be the one the rule is
		// silent on. rmTarget used to return the marker itself as rm's
		// target, which starts with `-` rather than `$`.
		"end-of-options marker":         "set -uo pipefail\nrm -rf -- \"$LAB\"\n",
		"end-of-options, flags apart":   "set -uo pipefail\nrm -r -f -- $LAB\n",
		"end-of-options, braced target": "set -uo pipefail\nrm -rf -- \"${LAB}/state\"\n",
		// C21. The guard used to be satisfied by a `:?` ANYWHERE in the
		// target, so the careful-looking half carried the careless half.
		"guarded head, bare tail":    "set -uo pipefail\nrm -rf \"${LAB:?}/$CELL\"\n",
		"guarded head, bare middle":  "set -uo pipefail\nrm -rf \"$SUB/${LAB:?}\"\n",
		"two expansions, no literal": "set -uo pipefail\nrm -rf \"${A}${B}\"\n",
		"command substitution":       "set -uo pipefail\nrm -rf \"$(cell_dir)\"\n",
		"absolute then bare segment": "set -uo pipefail\nrm -rf \"/srv/labs/$LAB\"\n",
		// ${VAR:+word} expands to EMPTY when VAR is empty: it is the
		// hazard, not the guard, and it used to be in the guarded class.
		"colon-plus is not a guard": "set -uo pipefail\nrm -rf \"${LAB:+$LAB}\"\n",
		// A rig helper's argument. Called with nothing, `$1` is empty.
		"a positional parameter": "set -uo pipefail\nclean() {\n  rm -rf \"$1\"\n}\n",
		"all the arguments":      "set -uo pipefail\nrm -rf \"$@\"\n",
	} {
		got := lintText(t, body)
		if len(got) != 1 || got[0].Rule != RuleRmRfVar {
			t.Errorf("%s: findings = %v, want one rm-rf-bare-var", name, got)
		}
	}
	for name, body := range map[string]string{
		"colon-question guard":   "set -uo pipefail\nrm -rf \"${SB_STATE:?}/vibe\"\n",
		"colon-dash default":     "set -uo pipefail\nrm -rf \"${LAB:-/tmp/none}\"\n",
		"a literal path":         "set -uo pipefail\nrm -rf /tmp/fixed/path\n",
		"literal then var":       "set -uo pipefail\nrm -rf /tmp/lab-\"$ID\"\n",
		"not recursive":          "set -uo pipefail\nrm -f \"$LAB\"\n",
		"guarded after --":       "set -uo pipefail\nrm -rf -- \"${LAB:?}\"\n",
		"not recursive after --": "set -uo pipefail\nrm -f -- \"$LAB\"\n",
		"every segment guarded":  "set -uo pipefail\nrm -rf \"${LAB:?}/${CELL:?}\"\n",
		"trailing slash":         "set -uo pipefail\nrm -rf \"${LAB:?}/state/\"\n",
	} {
		if got := lintText(t, body); len(got) != 0 {
			t.Errorf("%s: false positive %v", name, got)
		}
	}
	// EVERY rm on the line, not the first: the careful half used to carry
	// the careless one.
	if got := lintText(t, "set -uo pipefail\nrm -rf \"${A:?}\"; rm -rf \"$B\"\n"); len(got) != 1 {
		t.Errorf("a second rm on the same line was never examined: %v", got)
	}
	for name, body := range map[string]string{
		"both guarded": "set -uo pipefail\nrm -rf \"${A:?}\"; rm -rf \"${B:?}\"\n",
	} {
		if got := lintText(t, body); len(got) != 0 {
			t.Errorf("%s: false positive %v", name, got)
		}
	}
}

func TestUnscopedKill(t *testing.T) {
	for name, body := range map[string]string{
		"bare": "set -uo pipefail\npkill -f llama-swap\n",
		// The `$` has to be in the KILL's own arguments. Both of these
		// scanned clean: the rule searched everything to the right of the
		// verb, so a variable in a neighbouring command — or in a comment
		// claiming the pattern is scoped — read as scoping it. On this box
		// that is the production llama-swap on :9000.
		"a $ in a trailing comment": "set -uo pipefail\npkill -f llama-swap  # scoped to $LAB, honest\n",
		"a $ in the next command":   "set -uo pipefail\npkill -f llama-swap || echo \"$?\"\n",
		"a $ after a semicolon":     "set -uo pipefail\npkill -f llama-swap; rm -rf \"${WORK:?}\"\n",
		// C21: matching by process NAME cannot be scoped at all, however
		// many variables the pattern carries. Both of these passed.
		// A second kill on the same line used to be invisible: the pattern
		// ran to end-of-line, so the whole line was one (scoped) match.
		"a second kill after a semicolon": "set -uo pipefail\npkill -f \"$LAB/bin/vibe\"; pkill -f llama-swap\n",
		// Indented, inside a function — where a rig's sweep() actually
		// lives.
		"indented in a function":  "set -uo pipefail\nsweep() {\n  pkill -f llama-swap\n}\n",
		"pkill without -f":        "set -uo pipefail\npkill \"$RIG_PROC\"\n",
		"killall with a variable": "set -uo pipefail\nkillall \"$LAB_PROC\"\n",
		"killall with a literal":  "set -uo pipefail\nkillall llama-server\n",
		"pkill -9 without -f":     "set -uo pipefail\npkill -9 \"$RIG_PROC\"\n",
	} {
		got := lintText(t, body)
		if len(got) != 1 || got[0].Rule != RuleBroadKill {
			t.Errorf("%s: findings = %v, want one unscoped-kill", name, got)
		}
	}
	for name, body := range map[string]string{
		"anchored on the lab path": "set -uo pipefail\npkill -f \"llama-swap -config $LAB/\"\n",
		"anchored on a port base":  "set -uo pipefail\npkill -f \"llama-server .*--port $((BASE+1))\"\n",
		"kill by pid":              "set -uo pipefail\nkill -TERM \"$pid\"\n",
		"flag bundle carries f":    "set -uo pipefail\npkill -9f \"$LAB/bin/vibe\"\n",
	} {
		if got := lintText(t, body); len(got) != 0 {
			t.Errorf("%s: false positive %v", name, got)
		}
	}
}

// TestGitWriteNeedsARepo is the C17 blocker's second half. Rule 1 catches
// the `cd` that fails; this catches what a failed `cd` then lets loose.
// The reproduction in C17 was a rig that rewrote the operator's git
// identity and committed their working tree as "fleetlab defs".
func TestGitWriteNeedsARepo(t *testing.T) {
	for name, body := range map[string]string{
		"init":               "set -euo pipefail\ngit init -q .\n",
		"add":                "set -euo pipefail\ngit add -A\n",
		"commit":             "set -euo pipefail\ngit commit -qm \"fleetlab defs\"\n",
		"the identity write": "set -euo pipefail\ngit config user.email lab@fleetlab\n",
		"reset --hard":       "set -euo pipefail\ngit reset -q --hard HEAD~1\n",
		"after a semicolon":  "set -euo pipefail\ntrue; git checkout -q .\n",
		"after then":         "set -euo pipefail\nif true; then git clean -fd; fi\n",
		// Indented inside a function, which is where a rig's setup step
		// lives. An anchor of `^` alone saw only top-level commands.
		"indented in a function": "set -euo pipefail\nseed() {\n  git add -A\n}\n",
	} {
		got := lintText(t, body)
		if len(got) != 1 || got[0].Rule != RuleGitWriteHere {
			t.Errorf("%s: findings = %v, want one git-write-no-repo", name, got)
		}
	}
	for name, body := range map[string]string{
		"pinned with -C":        "set -euo pipefail\ngit -C \"$DEFS\" add -A\n",
		"pinned with --git-dir": "set -euo pipefail\ngit --git-dir=\"$D/.git\" commit -qm x\n",
		"a read verb":           "set -euo pipefail\ngit log --oneline -1\n",
		"status":                "set -euo pipefail\ngit status --porcelain\n",
		"clone names its dest":  "set -euo pipefail\ngit clone -q \"$DEFS\" \"$DST\"\n",
		// `git` inside a string is not a command. The repo's own
		// gate-c13-parity.sh prints "make the shared backends dir a real
		// git checkout", and a rule that read that as `git checkout` would
		// have shipped a false positive on its first run.
		"git inside a message": "set -euo pipefail\nhr \"0. make the dir a real git checkout\"\n",
		"pinned inside $()":    "set -euo pipefail\nX=$(git -C \"$D\" rev-parse HEAD)\n",
		"two pinned on a line": "set -euo pipefail\ngit -C \"$D\" config user.name lab; git -C \"$D\" config user.email l@a\n",
	} {
		if got := lintText(t, body); len(got) != 0 {
			t.Errorf("%s: false positive %v", name, got)
		}
	}
	// The counter is what makes the rule non-inert, so it has to count the
	// pinned ones too — otherwise a tree where every git call is correct
	// looks identical to a tree with no git calls at all.
	var st Stats
	lintFile("x.sh", strings.NewReader("git -C \"$D\" add -A\ngit -C \"$D\" commit -qm x\ngit log\n"), &st)
	if st.GitWrite != 2 {
		t.Errorf("GitWrite = %d, want 2: the denominator must count correct calls, or a clean tree and an "+
			"empty one are indistinguishable", st.GitWrite)
	}
}

// TestStatsCountWhatWasExamined pins the other denominators the same way.
func TestStatsCountWhatWasExamined(t *testing.T) {
	var st Stats
	lintFile("x.sh", strings.NewReader(
		"set -euo pipefail\n"+
			"cd \"$A\"\n"+
			"( cd \"$B\" && ls )\n"+
			"rm -rf \"${C:?}\"\n"+
			"rm -f \"$D\"\n"+
			"pkill -f \"$E/bin\"\n"+
			"git -C \"$F\" add -A\n"), &st)
	want := Stats{Cd: 2, RmRecursive: 1, Kill: 1, GitWrite: 1}
	if st != want {
		t.Fatalf("stats = %+v, want %+v — a floor built on a miscount is not a floor", st, want)
	}
}
