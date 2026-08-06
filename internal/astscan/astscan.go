// Package astscan is the reusable half of this repo's structural tests
// (fleet-control C20).
//
// Three of the defect classes this plan's adversarial reviews keep
// finding are "a guard that lives in one of N call paths": C15's
// llama-swap credential was in 1 of 7 producers, C4's warm class guard in
// 1 of 4, C16's version reader missed C15's authorizer entirely, and
// C14's structural suspend refusals held in two of three. Every one of
// those is invisible to a unit test — each producer has tests, and each
// producer's tests pass — and visible in one line of AST.
//
// C15 wrote that scan by hand for one verb. This package is the same scan
// with the verb as data, so a future phase declares "every function that
// does X must also call Y" in a few lines instead of forty, and gets the
// two guards that make such a scan trustworthy for free:
//
//   - MinProducers: a scan that matches nothing PASSES, which is how a
//     rename silently retires the rule. Findings are useless without a
//     floor on what was actually examined.
//   - Exempt: an exemption is a named function and a written reason, and
//     an exemption that stops matching anything is itself an error — the
//     stale-exemption idea C16's fix used, generalised.
//
// Deliberately syntactic (go/ast, not go/types): resolving types needs
// either the deprecated ParseDir-plus-Check dance or golang.org/x/tools,
// and this repo adds no dependency. A syntactic scan over-reports rather
// than under-reports, which is the safe direction for a guard.
//
// Two limits, both inherited from C15's hand-rolled version and both
// worth knowing before relying on a rule:
//
//   - The unit is a FuncDecl. A trigger inside a package-level `var f =
//     func() {…}` is invisible. Nothing in this module is written that
//     way, and MinProducers is what would make the omission visible if a
//     producer moved there.
//   - Matching is on the final identifier of a call, so `s.AuthorizeSwap`
//     and `other.AuthorizeSwap` are the same to this scan. That is the
//     over-reporting direction for Trigger and the under-reporting one
//     for Require; the behavioural test beside each rule is what covers
//     the difference.
package astscan

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Rule declares "every function in Dir that TRIGGERS must also REQUIRE".
type Rule struct {
	// Name identifies the rule in failure messages.
	Name string
	// Dir is the package directory to scan, relative to the test's own
	// directory. Non-test .go files only: a rule is about production code.
	Dir string
	// Trigger names make a function a producer. A name matches either a
	// bare call (`warmFn(...)`) or a selector call (`http.NewRequest(...)`,
	// `s.warmViaFront(...)`) — the selector's final identifier is what is
	// compared, because the receiver is exactly what varies between call
	// sites.
	Trigger []string
	// Require names the guard. A producer satisfies the rule by calling
	// ANY of them (some guards have a wrapper and a direct form).
	Require []string
	// Exempt names functions allowed to trigger without requiring, each
	// with the reason it is allowed to. An exemption that matches no
	// producer is an error: it has gone stale, and a stale exemption is a
	// hole nobody is looking at.
	Exempt map[string]string
	// MinProducers is the inertness floor. Fewer producers than this and
	// the rule failed to find the code it is about.
	MinProducers int
	// Because is appended to every finding: the sentence a future agent
	// reads when the scan fails on their new code.
	Because string
}

// Finding is one producer that triggers without requiring.
type Finding struct {
	File string
	Line int
	Func string
	Msg  string
}

func (f Finding) String() string { return fmt.Sprintf("%s:%d: %s: %s", f.File, f.Line, f.Func, f.Msg) }

// Result is what one Check examined as well as what it found, so a caller
// can assert on the denominator.
type Result struct {
	Producers []string
	Findings  []Finding
	// UnusedExemptions are declared exemptions that matched no producer.
	UnusedExemptions []string
}

// Check runs the rule. The error return is for the scan failing to run at
// all (an unreadable directory, an unparseable file) — which must be a
// test failure and never a silent pass.
func (r Rule) Check() (Result, error) {
	var res Result
	entries, err := os.ReadDir(r.Dir)
	if err != nil {
		return res, fmt.Errorf("astscan %s: read %s: %w", r.Name, r.Dir, err)
	}
	trigger := set(r.Trigger)
	require := set(r.Require)
	fset := token.NewFileSet()
	exemptUsed := map[string]bool{}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		path := filepath.Join(r.Dir, name)
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return res, fmt.Errorf("astscan %s: parse %s: %w", r.Name, path, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			triggers, requires := false, false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					if trigger[fun.Name] {
						triggers = true
					}
					if require[fun.Name] {
						requires = true
					}
				case *ast.SelectorExpr:
					if trigger[fun.Sel.Name] {
						triggers = true
					}
					if require[fun.Sel.Name] {
						requires = true
					}
				}
				return true
			})
			if !triggers {
				continue
			}
			res.Producers = append(res.Producers, name+":"+fn.Name.Name)
			if _, exempt := r.Exempt[fn.Name.Name]; exempt {
				exemptUsed[fn.Name.Name] = true
				continue
			}
			if requires {
				continue
			}
			res.Findings = append(res.Findings, Finding{
				File: path,
				Line: fset.Position(fn.Pos()).Line,
				Func: fn.Name.Name,
				Msg: fmt.Sprintf("calls %s but never %s. %s",
					or(r.Trigger), or(r.Require), r.Because),
			})
		}
	}
	for name := range r.Exempt {
		if !exemptUsed[name] {
			res.UnusedExemptions = append(res.UnusedExemptions, name)
		}
	}
	sort.Strings(res.UnusedExemptions)
	return res, nil
}

// Err reports everything wrong with a completed Check as one error, or
// nil. It folds in the inertness floor and the stale exemptions, so a
// caller cannot enforce the findings and forget the guards on the scan
// itself — the mistake this package exists to stop repeating.
func (r Rule) Err(res Result) error {
	var msgs []string
	for _, f := range res.Findings {
		msgs = append(msgs, f.String())
	}
	if len(res.Producers) < r.MinProducers {
		msgs = append(msgs, fmt.Sprintf(
			"found %d producer(s) in %s, expected at least %d: this scan is INERT — the code it guards was renamed, moved or deleted, and it would now pass over anything (producers: %s)",
			len(res.Producers), r.Dir, r.MinProducers, strings.Join(res.Producers, ", ")))
	}
	for _, name := range res.UnusedExemptions {
		msgs = append(msgs, fmt.Sprintf(
			"exemption %q matches no producer in %s: it is STALE. Either the function was renamed (re-point it) or it no longer triggers (delete it) — a stale exemption is an unwatched hole",
			name, r.Dir))
	}
	if len(msgs) == 0 {
		return nil
	}
	return fmt.Errorf("%s:\n  %s", r.Name, strings.Join(msgs, "\n  "))
}

func set(names []string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

func or(names []string) string {
	if len(names) == 1 {
		return names[0]
	}
	return "any of " + strings.Join(names, "/")
}
