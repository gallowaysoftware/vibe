package shelllint

import (
	"sort"
	"strings"
	"testing"
)

// scriptsRoot is the repo's scripts/ tree, from this package's directory.
const scriptsRoot = "../../scripts"

// exempt is the written-reason table, keyed file:line:rule. A key moves
// with the line, on purpose: an exemption should not outlive the line it
// exempts, and re-adding it is one edit with the reason in front of you.
var exempt = map[string]string{
	// gate-c15's rig spawns llama-server processes through llama-swap, so
	// their argv carries llama-swap's own config path and not the rig's
	// $LAB — the port range is the only handle the sweep has. It is a
	// standalone range (5960-5979) that no other rig uses, but it is
	// still a literal, and futures item 15 (FLEETLAB_PORT_BASE) is the
	// real fix. Recorded here so the next reader finds the hazard rather
	// than rediscovering it.
	"scripts/fleetlab/gate-c15-warm-auth.sh:66:unscoped-kill": "llama-server argv carries no rig path; anchored on this rig's private 596x port range instead",
	"scripts/fleetlab/gate-c15-warm-auth.sh:67:unscoped-kill": "the rig's own host-probe helper, named for the gate (c15-hostprobe)",
}

// TestScriptsAreSafe is the shell half of this phase's harness. The one
// blocker in this plan's history that was not a Go defect was a bare `cd`
// in a rig (C17 A1); nothing mechanical would have caught it.
func TestScriptsAreSafe(t *testing.T) {
	findings, files, err := Lint(scriptsRoot)
	if err != nil {
		t.Fatalf("lint %s: %v", scriptsRoot, err)
	}
	// Inertness: the walk must actually be reaching the rigs.
	if files < 15 {
		t.Fatalf("linted %d .sh files under %s — the scan is inert", files, scriptsRoot)
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
		t.Errorf("%d unsafe line(s) in scripts/:\n  %s", len(live), strings.Join(live, "\n  "))
	}
	for key := range exempt {
		if !used[key] {
			t.Errorf("exemption %q matches no finding: it is STALE (the line moved, or the hazard is "+
				"gone). Re-point it or delete it.", key)
		}
	}
}

// ── the rules themselves, proven to catch and proven not to over-fire ──

func lintText(t *testing.T, body string) []Finding {
	t.Helper()
	return lintFile("x.sh", strings.NewReader(body))
}

func TestUnguardedCd(t *testing.T) {
	// The C17 blocker, reduced.
	got := lintText(t, "#!/usr/bin/env bash\nset -uo pipefail\ncd \"$LAB/etc/vibe/backends\"\ngit add -A\n")
	if len(got) != 1 || got[0].Rule != RuleUnguardedCd {
		t.Fatalf("findings = %v, want one unguarded-cd", got)
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
	// the C17 blocker with one more statement on the line.
	if got := lintText(t, "set -uo pipefail\ncd \"$LAB\"; git init && git add -A\n"); len(got) != 1 {
		t.Errorf("an && to the RIGHT of the cd was read as guarding it: %v", got)
	}
	// The MIRROR image, which the self-review's REV-4 left open: the &&
	// short-circuits `git init`, and then the `;` starts a fresh command
	// that runs in the operator's directory regardless. An && guards only
	// what is left of the next `;`.
	if got := lintText(t, "set -uo pipefail\ncd \"$LAB\" && git init; git add -A\n"); len(got) != 1 {
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
	} {
		if got := lintText(t, body); len(got) != 0 {
			t.Errorf("%s: false positive %v", name, got)
		}
	}
}
