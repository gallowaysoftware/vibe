// Package shelllint is the mechanical form of one real blocker
// (fleet-control C20, class 8).
//
// `gate-c13-parity.sh` ran `cd "$LAB/etc/vibe/backends"` bare, under
// `set -uo pipefail` with **no** `-e`, and then `git init`,
// `git config user.email/name`, `git add -A` and `git commit`. With a
// wrong or absent FLEETLAB_DIR the `cd` fails, the shell stays in the
// operator's current directory, and those four commands run in the
// operator's own repository — which was reproduced in a scratch repo
// during C17: the rig rewrote the local git identity and committed the
// working tree as "fleetlab defs".
//
// The rigs under scripts/ are not incidental. They are where every live
// gate in this plan runs, they run beside a production llama-swap on
// :9000 and a production vibe daemon on :9001, and they are written
// fast under time pressure. Three rules, all cheap, all with written
// exemptions:
//
//  1. a `cd` whose failure is not handled, in a script without `set -e`;
//  2. `rm -rf` on a bare variable expansion, where an unset or EMPTY
//     variable makes the target `/` or the current directory;
//  3. a `pkill`/`killall` pattern with no variable in it, which cannot
//     be scoped to this rig's own processes and is entitled to kill a
//     sibling lab's (futures item 15 is the same hazard, from the port
//     side).
//
// No external linter: shellcheck is a binary this repo does not vendor
// and CI would have to install, and these three rules are the ones this
// project's own history produced.
package shelllint

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Finding is one flagged line.
type Finding struct {
	File string
	Line int
	Rule string
	Text string
	Why  string
}

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d [%s] %s\n      %s", f.File, f.Line, f.Rule, strings.TrimSpace(f.Text), f.Why)
}

// Key is the exemption key: file plus rule plus line text is too brittle,
// file plus rule too coarse. File + line number moves with any edit, which
// is deliberate — an exemption should not survive the line it exempts.
func (f Finding) Key() string { return fmt.Sprintf("%s:%d:%s", f.File, f.Line, f.Rule) }

const (
	RuleUnguardedCd = "unguarded-cd"
	RuleRmRfVar     = "rm-rf-bare-var"
	RuleBroadKill   = "unscoped-kill"
)

var (
	setLine = regexp.MustCompile(`^\s*set\s+[-+a-zA-Z\s]*`)
	// A cd at the start of a command position: line start, or after ; && || ( {.
	cdLine   = regexp.MustCompile(`(^|[;&|(){}]\s*)cd\s`)
	rmCmd    = regexp.MustCompile(`(^|[;&|(){}'"]|\s)rm\s`)
	killLine = regexp.MustCompile(`\b(pkill|killall)\b([^\n]*)`)
	// ${VAR:?...} and ${VAR:-...} both make an empty expansion safe: the
	// first aborts, the second substitutes.
	guardedExpansion = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*:[?+-]`)
	bareExpansion    = regexp.MustCompile(`\$\{?[A-Za-z_][A-Za-z0-9_]*\}?`)
)

// Lint scans every .sh file under root.
func Lint(root string) ([]Finding, int, error) {
	var out []Finding
	files := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".sh") {
			return nil
		}
		files++
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out = append(out, lintFile(filepath.ToSlash(filepath.Join(filepath.Base(root), rel)), f)...)
		return nil
	})
	return out, files, err
}

func lintFile(name string, r interface{ Read([]byte) (int, error) }) []Finding {
	var out []Finding
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	errexit := false
	n := 0
	for sc.Scan() {
		n++
		raw := sc.Text()
		line := stripComment(raw)
		if line == "" {
			continue
		}
		if m := setLine.FindString(line); m != "" && strings.Contains(m, "-") {
			// `set -e`, `set -euo pipefail`, `set -o errexit`.
			if hasErrExit(m) {
				errexit = true
			}
		}
		if !errexit && cdLine.MatchString(line) && !cdHandled(line) {
			out = append(out, Finding{name, n, RuleUnguardedCd, raw,
				"a cd whose failure nothing handles, in a script with no `set -e`: the shell keeps " +
					"running in the operator's own directory. Use `cd … || exit 1`, `( cd … && … )`, or `set -e`."})
		}
		if loc := rmCmd.FindStringIndex(line); loc != nil {
			if arg, recursive := rmTarget(line[loc[1]:]); recursive && needsEmptyGuard(arg) {
				out = append(out, Finding{name, n, RuleRmRfVar, raw,
					"rm -rf on a bare variable expansion: `set -u` catches UNSET but not EMPTY, and an " +
						"empty expansion here deletes the current directory or /. Use ${VAR:?} so an empty value aborts."})
			}
		}
		if m := killLine.FindStringSubmatch(line); m != nil {
			if !strings.Contains(m[2], "$") {
				out = append(out, Finding{name, n, RuleBroadKill, raw,
					"a kill pattern with no variable in it cannot be scoped to this rig: it matches a " +
						"sibling lab's processes, and on this box a production llama-swap and vibe daemon. " +
						"Anchor it on the rig's own path or port base."})
			}
		}
	}
	return out
}

func hasErrExit(setCmd string) bool {
	fields := strings.Fields(setCmd)
	for i, f := range fields {
		if f == "-o" && i+1 < len(fields) && fields[i+1] == "errexit" {
			return true
		}
		if strings.HasPrefix(f, "-") && !strings.HasPrefix(f, "--") && strings.Contains(f, "e") {
			// `-o` is not a flag bundle; `-euo` is.
			if f != "-o" {
				return true
			}
		}
	}
	return false
}

// cdHandled reports whether the line already deals with a failing cd:
// an explicit `|| …`, or a subshell that chains with `&&` (the
// `( cd "$X" && cmd )` idiom, where a failed cd stops the chain).
func cdHandled(line string) bool {
	i := strings.Index(line, "cd ")
	if i < 0 {
		return false
	}
	// The operator has to be in the cd's OWN command. `cd "$X"; git init
	// && git add -A` contains a `&&` after the cd and chains off `git
	// init` — which is the C17 blocker with one more statement on the
	// line, and the reading a whole-line Contains() gets wrong.
	seg := line[i:]
	if j := strings.IndexAny(seg, ";\n"); j >= 0 {
		seg = seg[:j]
	}
	return strings.Contains(seg, "||") || strings.Contains(seg, "&&")
}

// rmTarget walks rm's arguments: every leading -flag is consumed (the
// recursive bit may live in any of them, and `rm -r -f X` is as dangerous
// as `rm -rf X`), and the first non-flag word is the target. Quotes are
// kept — "$LAB" and $LAB are the same hazard, "${LAB:?}" is not.
func rmTarget(rest string) (target string, recursive bool) {
	for {
		rest = strings.TrimLeft(rest, " \t")
		if rest == "" {
			return "", recursive
		}
		end := strings.IndexAny(rest, " \t;&|")
		word := rest
		if end >= 0 {
			word = rest[:end]
		}
		if strings.HasPrefix(word, "-") && word != "--" {
			if strings.ContainsAny(word, "rR") {
				recursive = true
			}
			if end < 0 {
				return "", recursive
			}
			rest = rest[end:]
			continue
		}
		return word, recursive
	}
}

// needsEmptyGuard reports whether an rm -rf target begins with a variable
// expansion that an empty value would turn into "" or "/…".
func needsEmptyGuard(arg string) bool {
	trimmed := strings.Trim(arg, `"'`)
	if !strings.HasPrefix(trimmed, "$") {
		return false
	}
	if guardedExpansion.MatchString(trimmed) {
		return false
	}
	return bareExpansion.MatchString(trimmed)
}

func stripComment(line string) string {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "#") {
		return ""
	}
	return line
}
