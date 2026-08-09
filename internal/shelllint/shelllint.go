// Package shelllint is the mechanical form of one real blocker
// (fleet-control C20, class 8; extended in C21).
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
// fast under time pressure. Four rules, all cheap, all with written
// exemptions:
//
//  1. a `cd` whose failure is not handled, in a script without `set -e`;
//  2. `rm -rf` on a path segment an empty variable would erase, which
//     makes the target `/`, the current directory, or — the C21
//     addition — the target's own PARENT;
//  3. a `pkill`/`killall` that cannot be scoped to this rig's own
//     processes, and is therefore entitled to kill a sibling lab's
//     (futures item 15 is the same hazard, from the port side);
//  4. a git WRITE verb that does not name the repository it writes to.
//     Rule 1 catches the bad `cd`; this is what makes a bad `cd`
//     catastrophic rather than merely wrong, and it is the other half of
//     the C17 blocker.
//
// No external linter: shellcheck is a binary this repo does not vendor
// and CI would have to install, and these rules are the ones this
// project's own history produced.
//
// C21 also widened the walk from scripts/ to the whole module. install.sh
// is the script this project asks strangers to pipe into sh, and it had
// never been linted; internal/install_test.sh had never been linted
// either. A linter whose root excludes the most-executed shell script in
// the repository is not a coverage story, it is a coincidence.
//
// The walk was widened; the SELECTION was not. Files were picked by the
// `.sh` extension, and git requires a hook to be named exactly `pre-push`
// — so scripts/hooks/pre-push, the one script an author edits casually
// because it is "just the hook", was the only shell script in the tree
// with no rule over it at all. Selection is now extension OR shebang, and
// the shebang half is deliberately narrow: only an EXTENSIONLESS file is
// sniffed, and only a shell interpreter counts, so marionette.py and every
// other named-by-extension file is decided without being opened.
package shelllint

import (
	"bufio"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gallowaysoftware/vibe/internal/astscan"
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
//
// Known coarseness: two findings of the SAME rule on the same line share a
// key, so one exemption covers both. Reachable only by writing two
// hazardous commands of one kind on one line (`rm -rf "$A"; rm -rf "$B"`);
// recorded rather than fixed because a per-occurrence index would make
// every existing key depend on the order of matches within a line.
func (f Finding) Key() string { return fmt.Sprintf("%s:%d:%s", f.File, f.Line, f.Rule) }

const (
	RuleUnguardedCd  = "unguarded-cd"
	RuleRmRfVar      = "rm-rf-bare-var"
	RuleBroadKill    = "unscoped-kill"
	RuleGitWriteHere = "git-write-no-repo"
)

// Stats is what the lint EXAMINED, not what it found. Every rule needs its
// own denominator: the file count says the walk ran, and says nothing
// about whether a rule's pattern still matches anything. A repo that
// stopped using `pkill` would retire rule 3 in silence, which is the exact
// failure mode this package exists to prevent in shell scripts.
type Stats struct {
	Files int
	// Cd counts cd commands in command position, guarded or not.
	Cd int
	// RmRecursive counts rm invocations carrying a recursive flag.
	RmRecursive int
	// Kill counts pkill/killall invocations.
	Kill int
	// GitWrite counts git invocations whose subcommand writes.
	GitWrite int
}

var (
	setLine = regexp.MustCompile(`^\s*set\s+[-+a-zA-Z\s]*`)
	// Command position: line start — INDENTED or not, because a rig's
	// commands live inside functions and an anchor of `^` alone saw only
	// the top-level ones — or after ; && || | ( ) { }, or after one of the
	// keywords that introduces a command. `then cd "$LAB"` used to be
	// invisible to this rule for want of the keyword alternative.
	cmdPos  = `(?:^\s*|[;&|(){}]\s*|\b(?:then|do|else)\s+)`
	cdLine  = regexp.MustCompile(cmdPos + `cd\s`)
	gitLine = regexp.MustCompile(cmdPos + `git\s`)
	rmCmd   = regexp.MustCompile(`(^|[;&|(){}'"]|\s)rm\s`)
	// `[^;\n]*` rather than `[^\n]*` so a SECOND kill on the same line is
	// its own match. `pkill -f "$LAB/x"; pkill -f llama-swap` was read as
	// one invocation — the scoped one — and the unscoped half, which on
	// this box reaches the production llama-swap on :9000, was never
	// examined at all.
	killLine = regexp.MustCompile(`\b(pkill|killall)\b([^;\n]*)`)
	// pkill's full-command-line match: `-f`, a bundle carrying it
	// (`pkill -9f`), or the long form `--full`.
	killFullMatch = regexp.MustCompile(`(^|\s)(--full\b|-[0-9a-zA-Z]*f)`)
	// ${VAR:?...} aborts on empty and ${VAR:-...} substitutes, so neither
	// can collapse to nothing.
	//
	// `:+` used to be in this class and is NOT a guard: `${VAR:+word}`
	// expands to the EMPTY string exactly when VAR is empty, which is the
	// hazard rather than the fix.
	guardedExpansion = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*:[?-]`)
	// Positional and special parameters are expansions too, and they are
	// the ones a rig helper reaches for: `clean() { rm -rf "$1"; }` called
	// with no argument is `rm -rf ""`, and `"$@"` empty is the same.
	varExpansion = regexp.MustCompile(`^\$(?:[A-Za-z_][A-Za-z0-9_]*|\{[A-Za-z_][A-Za-z0-9_]*\}|[0-9@*])`)
)

// gitWriteVerbs is an allow-list rather than a deny-list: an unknown
// subcommand is assumed to be a read. Over-reporting is the safe direction
// for rules 1-3, but not for this one — the point is that every entry here
// can leave a permanent change in whichever repository the shell happens
// to be standing in, and a list padded with `log` and `status` would be
// exempted into uselessness on its first run.
//
// `clone` is deliberately absent: its destination is an explicit argument
// and it creates the repository it writes to.
var gitWriteVerbs = map[string]bool{
	"init": true, "add": true, "commit": true, "config": true,
	"checkout": true, "switch": true, "restore": true, "reset": true,
	"clean": true, "rm": true, "mv": true, "apply": true, "am": true,
	"cherry-pick": true, "rebase": true, "merge": true, "revert": true,
	"stash": true, "tag": true, "branch": true, "push": true,
	"fetch": true, "pull": true, "remote": true, "submodule": true,
	"worktree": true, "gc": true, "prune": true, "update-ref": true,
	"symbolic-ref": true, "sparse-checkout": true, "filter-branch": true,
	"notes": true, "replace": true,
}

// Lint scans every shell script under root, which is expected to be a
// module root. Findings and exemption keys are keyed on the path RELATIVE
// to root, so they read the way a reviewer types a path and do not embed
// the name of whichever checkout directory the tests happen to run in.
func Lint(root string) ([]Finding, Stats, error) {
	var out []Finding
	var stats Stats
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
		if !IsShellScript(path, d.Name()) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		stats.Files++
		findings, err := lintPath(filepath.ToSlash(rel), path, &stats)
		if err != nil {
			return err
		}
		out = append(out, findings...)
		return nil
	})
	return out, stats, err
}

// skipDir keeps the walk inside this module's own tree. Without it, a
// checkout of this repo parked under .claude/worktrees/ — which is how
// every parallel agent works here — contributes a second copy of every
// rig, under keys no exemption can name, and the lint fails locally while
// passing in CI. See astscan.ForeignDir.
func skipDir(root, path, name string) bool {
	if path == root {
		return false
	}
	if name == "node_modules" {
		return true
	}
	if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
		return true
	}
	return astscan.ForeignDir(root, path)
}

// shebangPeek bounds what an extensionless file costs to classify. A
// shebang is the FIRST line by definition, so there is nothing to find
// past it, and a fixed prefix means a stray binary in an untracked build
// directory is two syscalls rather than a whole read.
const shebangPeek = 256

// shellInterpreters is the allow-list the shebang half is decided on. It is
// an allow-list for the same reason gitWriteVerbs is: an unrecognised
// interpreter must not be linted with rules about `cd`, `rm -rf` and
// `pkill` that are written for a POSIX shell.
var shellInterpreters = map[string]bool{
	"sh": true, "bash": true, "dash": true, "ksh": true, "zsh": true, "ash": true,
}

// IsShellScript reports whether path is one of the scripts these rules are
// about: named `.sh`, or extensionless with a shell shebang.
//
// The second half exists for exactly one shape, and it is the shape most
// likely to be edited without thinking: git will only run a hook that is
// named for the hook, so scripts/hooks/pre-push cannot be called
// pre-push.sh, and an extension-keyed lint therefore covered every rig in
// scripts/fleetlab and not the file that runs on every push.
//
// Extensionless is the narrowing that keeps this from becoming "sniff
// everything". A file whose name carries an extension is classified by
// that extension and never opened, so marionette.py, the .go sources and
// the fixtures stay out on their names alone; only Dockerfile, LICENSE and
// their kind are read, and neither has a shebang.
func IsShellScript(path, name string) bool {
	if strings.HasSuffix(name, ".sh") {
		return true
	}
	if strings.Contains(name, ".") {
		return false
	}
	return hasShellShebang(path)
}

// hasShellShebang reads the first line of path and reports whether it names
// a shell. An unreadable file is not a shell script: the walk has already
// decided this entry exists, so a failure here is a permissions or race
// problem, and reporting it as "lint me" would fail the whole run on a
// file the lint has no rules for anyway.
func hasShellShebang(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, shebangPeek)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return false
	}
	line := string(buf[:n])
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = line[:i]
	}
	if !strings.HasPrefix(line, "#!") {
		return false
	}
	fields := strings.Fields(line[2:])
	// `#!/usr/bin/env bash` and `#!/usr/bin/env -S bash -e` both name their
	// interpreter one or more words along, which is how every script in this
	// repo is written.
	for len(fields) > 0 && (filepath.Base(fields[0]) == "env" || strings.HasPrefix(fields[0], "-")) {
		fields = fields[1:]
	}
	if len(fields) == 0 {
		return false
	}
	return shellInterpreters[filepath.Base(fields[0])]
}

// lintPath opens and lints one file. The open/close pair is a function
// rather than a `defer` inside the walk callback: deferring there holds
// every descriptor until the whole walk finishes.
func lintPath(name, path string, stats *Stats) ([]Finding, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return lintFile(name, f, stats), nil
}

func lintFile(name string, r io.Reader, stats *Stats) []Finding {
	if stats == nil {
		stats = &Stats{}
	}
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
		if locs := cdLine.FindAllStringIndex(line, -1); locs != nil {
			stats.Cd += len(locs)
			if !errexit && !cdHandled(line) {
				out = append(out, Finding{name, n, RuleUnguardedCd, raw,
					"a cd whose failure nothing handles, in a script with no `set -e`: the shell keeps " +
						"running in the operator's own directory. Use `cd … || exit 1`, `( cd … && … )`, or `set -e`."})
			}
		}
		// EVERY rm on the line, not the first. `rm -rf "${A:?}"; rm -rf
		// "$B"` used to be judged entirely on its careful half.
		for _, loc := range rmCmd.FindAllStringIndex(line, -1) {
			arg, recursive := rmTarget(line[loc[1]:])
			if !recursive {
				continue
			}
			stats.RmRecursive++
			if why := unsafeRmTarget(arg); why != "" {
				out = append(out, Finding{name, n, RuleRmRfVar, raw, why})
			}
		}
		for _, m := range killLine.FindAllStringSubmatch(line, -1) {
			stats.Kill++
			if why := unscopedKill(m[1], m[2]); why != "" {
				out = append(out, Finding{name, n, RuleBroadKill, raw, why})
			}
		}
		for _, loc := range gitLine.FindAllStringIndex(line, -1) {
			verb, pinned := gitInvocation(line[loc[1]:])
			if !gitWriteVerbs[verb] {
				continue
			}
			stats.GitWrite++
			if pinned {
				continue
			}
			out = append(out, Finding{name, n, RuleGitWriteHere, raw,
				"`git " + verb + "` with no -C/--git-dir writes to whichever repository the shell is " +
					"standing in. That is the C17 blocker's second half: a failed `cd` upstream of this " +
					"line does not stop it, it re-points it at the operator's own checkout, where the rig " +
					"rewrote the git identity and committed the working tree. Use `git -C \"$DIR\" …`."})
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
//
// Both halves of the `;` matter, and the review pass found the second one
// after the self-review had fixed the first. `cd "$X"; git init && git
// add -A` chains off `git init`, not off the cd — that is the C17 blocker
// verbatim. But so is `cd "$X" && git init; git add -A`: the `&&` stops
// `git init`, and then the `;` starts a fresh command that runs in the
// operator's directory anyway. So an `&&` guards only what is left of the
// next `;` — if anything follows the cd's own command on the line, the
// cd's failure is still unhandled. `|| …` is different: it handles the
// failure (typically by exiting) rather than merely skipping one command.
func cdHandled(line string) bool {
	i := strings.Index(line, "cd ")
	if i < 0 {
		return false
	}
	seg := line[i:]
	j := strings.IndexAny(seg, ";\n")
	if j >= 0 {
		seg = seg[:j]
	}
	if strings.Contains(seg, "||") {
		return true
	}
	// An && chain that is the last thing on the line: nothing runs after
	// the short-circuit, so the wrong directory reaches nothing.
	return j < 0 && strings.Contains(seg, "&&")
}

// rmTarget walks rm's arguments: every leading -flag is consumed (the
// recursive bit may live in any of them, and `rm -r -f X` is as dangerous
// as `rm -rf X`), and the first non-flag word is the target. Quotes are
// kept — "$LAB" and $LAB are the same hazard, "${LAB:?}" is not.
//
// `--` is CONSUMED rather than returned as the target. It used to be
// returned, which made `rm -rf -- "$LAB"` scan clean — the end-of-options
// marker is the more careful spelling and this repo already uses it
// (`cd -- "$(dirname …)"`), so the rule was silent on exactly the line an
// author who was being careful would write.
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
		if strings.HasPrefix(word, "-") {
			if word != "--" && strings.ContainsAny(word, "rR") {
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

// unsafeRmTarget reports why an `rm -rf` target is unsafe, or "".
//
// Two positions matter, and C20 shipped only the first:
//
//   - the target BEGINS with an unguarded expansion. Empty, the target
//     becomes "/…" (the filesystem root) or "" (the current directory).
//     This is the C20 rule, with one hole closed: it used to accept any
//     target containing a guarded expansion ANYWHERE, so
//     `rm -rf "$LAB/${SUB:?}"` — unguarded exactly where it matters —
//     scanned clean.
//   - the final path SEGMENT is entirely an unguarded expansion. Empty,
//     the target collapses to its own parent: `rm -rf "${LAB:?}/$CELL"`
//     with no CELL set deletes the whole lab rather than one cell, and
//     the `:?` on the front is what makes it read as careful.
//
// A command substitution counts as an unguarded expansion in either
// position: `$(…)` that prints nothing is empty in exactly the same way.
func unsafeRmTarget(arg string) string {
	t := strings.Trim(arg, `"'`)
	if tok := leadingExpansion(t); tok != "" && !guardedExpansion.MatchString(tok) {
		return "rm -rf on a target that BEGINS with a bare variable expansion: `set -u` catches UNSET " +
			"but not EMPTY, and an empty expansion here deletes the current directory or /. " +
			"Use ${VAR:?} so an empty value aborts."
	}
	segs := pathSegments(t)
	if n := len(segs); n > 1 && segs[n-1] == "" {
		segs = segs[:n-1] // a trailing slash
	}
	if n := len(segs); n > 1 && collapsesToNothing(segs[n-1]) {
		return "rm -rf whose LAST path segment is a bare variable expansion: empty, the target collapses " +
			"to its own parent — `rm -rf \"${LAB:?}/$CELL\"` with no CELL deletes the whole lab, and the " +
			"guard on the front is what makes it read as careful. Use ${VAR:?} on every segment."
	}
	return ""
}

// leadingExpansion returns the expansion token at the start of s, or "".
func leadingExpansion(s string) string {
	if !strings.HasPrefix(s, "$") {
		return ""
	}
	if strings.HasPrefix(s, "${") {
		if end := matching(s, '{', '}'); end > 0 {
			return s[:end]
		}
		return s
	}
	if strings.HasPrefix(s, "$(") {
		if end := matching(s, '(', ')'); end > 0 {
			return s[:end]
		}
		return s
	}
	return varExpansion.FindString(s)
}

// matching returns the index just past the closer that balances the opener
// at s[1], or -1.
func matching(s string, open, close byte) int {
	depth := 0
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}

// pathSegments splits on '/' without cutting inside an expansion:
// `${LAB:-/tmp/none}` carries a slash that is not a path separator.
func pathSegments(s string) []string {
	var out []string
	start, depth := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '{', '(':
			depth++
		case '}', ')':
			if depth > 0 {
				depth--
			}
		case '/':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// collapsesToNothing reports whether a path segment is made ENTIRELY of
// unguarded expansions, so an empty value erases the whole segment.
// `lab-$ID` does not qualify: empty, it is still "lab-".
func collapsesToNothing(seg string) bool {
	rest, n := seg, 0
	for rest != "" {
		tok := leadingExpansion(rest)
		if tok == "" {
			return false // literal text survives an empty expansion
		}
		if guardedExpansion.MatchString(tok) {
			return false
		}
		rest = rest[len(tok):]
		n++
	}
	return n > 0
}

// unscopedKill reports why a kill cannot be confined to this rig, or "".
//
// Three separate ways to be unscoped, and C20 pinned only the third:
//
//   - `killall` matches by process NAME. There is no argument that can
//     narrow it to one lab, so it is always entitled to a sibling's
//     processes and to the production llama-swap on :9000.
//   - `pkill` WITHOUT -f matches by process name too. `pkill "$RIG"` has
//     a variable in it and is exactly as unscoped as `pkill llama-server`.
//   - `pkill -f` with no variable in its own arguments is a pattern that
//     cannot name this rig's paths or ports.
func unscopedKill(verb, args string) string {
	own := killArgs(args)
	switch {
	case verb == "killall":
		return "killall matches by process NAME, which cannot be narrowed to one rig: it is entitled to a " +
			"sibling lab's processes and to the production llama-swap on :9000. Use `pkill -f` with a " +
			"pattern anchored on this rig's own path or port base."
	case !killFullMatch.MatchString(own):
		return "pkill without -f matches by process NAME, so every llama-server on the box is in scope " +
			"however specific the pattern looks. Use `pkill -f` and anchor the pattern on this rig's own " +
			"path or port base."
	case !strings.Contains(own, "$"):
		return "a kill pattern with no variable in it cannot be scoped to this rig: it matches a " +
			"sibling lab's processes, and on this box a production llama-swap and vibe daemon. " +
			"Anchor it on the rig's own path or port base."
	}
	return ""
}

// killArgs narrows a kill line to the verb's own command, so a variable
// in a NEIGHBOURING command cannot be read as having scoped the pattern.
// `|` alone is deliberately not a separator: it is legal inside a pkill
// -f regex, and cutting there would over-report on a real alternation.
func killArgs(rest string) string {
	for _, sep := range []string{";", "&&", "||"} {
		if i := strings.Index(rest, sep); i >= 0 {
			rest = rest[:i]
		}
	}
	return rest
}

// gitInvocation reads git's global options and returns its subcommand plus
// whether the repository was named explicitly. `git -C "$DIR" add -A` is
// pinned; `git add -A` writes wherever the shell is standing.
func gitInvocation(rest string) (verb string, pinned bool) {
	fields := strings.Fields(rest)
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		switch {
		case f == "-C":
			pinned = true
			i++ // the directory
		case f == "-c":
			i++ // the config assignment
		case strings.HasPrefix(f, "--git-dir") || strings.HasPrefix(f, "--work-tree"):
			pinned = true
		case strings.HasPrefix(f, "-"):
			// another global option (--no-pager, -P, --exec-path=…)
		default:
			return f, pinned
		}
	}
	return "", pinned
}

// stripComment removes a whole-line comment and a TRAILING one.
//
// The trailing half is not cosmetic. The kill rule asks whether the
// pattern carries a variable, and a comment sits to the right of
// everything — so `pkill -f llama-swap  # scoped to $LAB` scanned clean,
// which on this box is the production llama-swap on :9000. A `#` opens a
// comment only at line start or after whitespace, and only outside
// quotes: `${LAB#prefix}`, `$#` and `"a # b"` are not comments.
func stripComment(line string) string {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "#") {
		return ""
	}
	var sq, dq bool
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '\\':
			if !sq {
				i++
			}
		case '\'':
			if !dq {
				sq = !sq
			}
		case '"':
			if !sq {
				dq = !dq
			}
		case '#':
			if !sq && !dq && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t') {
				return line[:i]
			}
		}
	}
	return line
}
