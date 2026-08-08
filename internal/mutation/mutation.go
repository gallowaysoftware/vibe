// Package mutation is the mechanical form of the review step this plan
// has been doing by hand for twenty phases (fleet-control C20).
//
// Every adversarial-review addendum under docs/design/fleet-control-plan/
// carries a table headed "mutation | red": the reviewer broke a
// production predicate, watched a NAMED test go red, and restored the
// line. That is the only technique in this project that has reliably
// distinguished a guard that works from a guard that is merely present —
// and it found 39+ real defects in code that was already green in CI,
// four of them blockers.
//
// It is also entirely manual, which means it happens when somebody funds
// a review pass and never afterwards. A guard whose test was
// mutation-verified in C11 and quietly stopped covering it in C14 is
// indistinguishable, from CI's point of view, from one that still works.
//
// So the table becomes data. Each entry names a production line, the
// edit that breaks it, and the tests that MUST go red. The runner applies
// each mutation to a copy of the tree, runs those tests, and asserts they
// fail. Two failures of the registry itself are as important as the
// findings:
//
//   - the mutation applies and NO named test fails: the guard is
//     UNPROTECTED. That is the finding.
//   - the Find pattern no longer matches: the entry is STALE. A refactor
//     must not be able to silently retire coverage, which is the same
//     stale-exemption rule C16's fix introduced and internal/astscan
//     generalises.
//
// A third, quieter one: a mutation that does not COMPILE proves nothing,
// so a build failure is a registry error rather than a catch. "The test
// suite went red" is not the claim; "this named assertion went red" is.
//
// Runtime is why this lives in its own CI job (like the llama-swap
// conformance matrix) and behind VIBE_MUTATION_TEST: every entry is a
// package recompile. See the phase doc for the measured number.
package mutation

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/gallowaysoftware/vibe/internal/astscan"
)

// Mutation is one guard and the edit that disarms it.
type Mutation struct {
	// Name is the entry's identity in output and in the phase docs.
	Name string
	// File is repo-relative.
	File string
	// Find must appear EXACTLY once in File. Exactly once rather than at
	// least once: an ambiguous pattern would mutate whichever copy came
	// first, and the entry would keep passing while covering something
	// else.
	Find string
	// Replace is what Find becomes. It must still compile — see the
	// package comment.
	Replace string
	// Pkg is the package to test, as a `go test` pattern
	// (./internal/vibe/fleetapi/).
	Pkg string
	// MustFail names the tests that must go red. Each one is checked
	// individually: a named test that does not RUN is as much a failure
	// as one that passes, because a renamed test is the commonest way an
	// entry stops covering anything.
	MustFail []string
	// Why records what the guard protects. It is the sentence the next
	// agent reads when this entry fails, so it says what breaks in the
	// FLEET, not what the code does.
	Why string
}

// Registry is the table. Entries are seeded from the mutation records in
// the phase docs' adversarial-review addenda — the load-bearing ones, not
// all of them: an entry costs a package rebuild, and a registry nobody
// will wait for is a registry that gets deleted.
var Registry = []Mutation{
	// ── class 1: absent evidence read as a healthy value ──────────────
	{
		Name: "inflight/unknown-operation-reads-as-a-reported-zero",
		File: "internal/vibe/fleetapi/watcher.go",
		Find: "\tdefault:\n\t\ts.disarmInFlightLocked(cell, wrap.Operation)\n\t\treturn\n\t}",
		// Falls through to the "we understood this frame" path, so the
		// previous set's length is published as a KNOWN count.
		Replace:  "\tdefault:\n\t\t_ = wrap.Operation\n\t}",
		Pkg:      "./internal/vibe/fleetapi/",
		MustFail: []string{"TestInFlightUnknownShapeIsNotIdle"},
		Why: "an inflight frame shape this build cannot fold must UN-report the count. v240+ tags " +
			"frames with an operation and sends deltas; folding one as a full list reported zero " +
			"in flight on a busy cell, which disarmed drain, suspend, probe and both warm loops at once.",
	},
	{
		Name:     "warm-target/in-flight-request-no-longer-blocks-the-restore",
		File:     "internal/vibe/fleetapi/warmtarget.go",
		Find:     "if n, reported := s.InFlight(t.Cell).Observed(); reported && n > 0 {",
		Replace:  "if n, reported := s.InFlight(t.Cell).Observed(); false && reported && n > 0 {",
		Pkg:      "./internal/vibe/fleetapi/",
		MustFail: []string{"TestWarmTarget_InFlightRequestBlocksRestore"},
		Why: "a request in flight IS activity. The per-model stamp only moves on frames, and a " +
			"generation longer than restore_after_idle produces none between its start and its " +
			"end — so without this rung the warm policy evicts the operator's model mid-stream.",
	},
	{
		Name:     "warm-target/fleetd-uptime-becomes-the-idle-clock",
		File:     "internal/vibe/fleetapi/warmtarget.go",
		Find:     "\treturn s.inFlight[cell].IsKnown() || s.cellUp[cell]",
		Replace:  "\treturn true",
		Pkg:      "./internal/vibe/fleetapi/",
		MustFail: []string{"TestWarmTarget_NoActivityEvidence"},
		Why: "a resident with no recorded activity is evidence of idleness only where fleetd is " +
			"WATCHING the cell. Without the channel check, fleetd's own uptime becomes the clock " +
			"the warm rule forbids, and every resident swap is evicted on the first tick past the window.",
	},
	{
		Name: "drain-wait/evidence-loss-reported-as-quiescence",
		File: "internal/vibe/daemon/cell_drain.go",
		Find: "\t\tn, reported := d.fleet.InFlight(cell).Observed()\n\t\tswitch {\n\t\tcase reported:",
		// The pre-C20 spelling, in effect: read the count, drop the bit,
		// let an unreported read take the reported branch as a zero. The
		// `_ = reported` keeps it compiling — a mutation that fails to
		// build proves nothing about the guard.
		Replace:  "\t\tn, reported := d.fleet.InFlight(cell).Observed()\n\t\t_ = reported\n\t\tswitch {\n\t\tcase true:",
		Pkg:      "./internal/vibe/daemon/",
		MustFail: []string{"TestCellDrainWait_DoesNotClaimQuiescenceWhenTheEvidenceStops", "TestCellDrainWait_RidesOutAReconnectWithTheRequestStillRunning"},
		Why: "the in-flight count can go UNREPORTED mid-wait when the cell's events stream drops. " +
			"Reading that as zero tells an operator who asked for quiescence that the cell went " +
			"quiet, when what went quiet was fleetd's evidence — and the stop that follows cancels " +
			"whatever was running. Both halves are pinned: the report must not read as quiescence, " +
			"and a gap shorter than the grace must not end the wait at all (llama-swap re-seeds a " +
			"fresh events connection inside ~200 ms).",
	},

	// ── class 3: a guard that lives in one of N call paths ────────────
	{
		Name:     "c15/streamCell drops the llama-swap credential",
		File:     "internal/vibe/fleetapi/watcher.go",
		Find:     "\tif err := s.AuthorizeSwap(req, c.Name); err != nil {\n\t\treturn false\n\t}",
		Replace:  "\tif req == nil {\n\t\treturn false\n\t}",
		Pkg:      "./internal/vibe/fleetapi/",
		MustFail: []string{"TestEveryLlamaSwapRequestIsAuthorized"},
		Why: "the AST scan is the only thing that sees a NEW producer built without the credential " +
			"— it caught C16's /api/version reader at merge time. If it stops failing for a " +
			"genuinely unauthorized builder it is decoration.",
	},
	{
		Name:     "c15/fleetmcp's unload builder drops the credential",
		File:     "internal/vibe/fleetmcp/fleetmcp.go",
		Find:     "\tif err := s.fleet.AuthorizeSwap(req, cell); err != nil {\n\t\treturn \"\", fmt.Errorf(\"unload %s on %s: %v\", model, cell, credentialDetail(err))\n\t}",
		Replace:  "\tif req == nil {\n\t\treturn \"\", nil\n\t}",
		Pkg:      "./internal/vibe/fleetmcp/",
		MustFail: []string{"TestEveryLlamaSwapRequestIsAuthorized"},
		Why: "the credential rule has a TWIN, and fleetmcp is the half that holds the operator's verbs " +
			"(warm_model, unload_model). The registry covered only fleetapi's copy, so the phase doc's " +
			"claim that one entry proves both was one rename away from being false in the package where " +
			"a 401 is an agent's verb failing silently.",
	},
	{
		Name:     "c4/the warm class guard leaves the restore",
		File:     "internal/vibe/fleetapi/warmtarget.go",
		Find:     "\tif refused := s.warmClassRefusal(t.Model); refused != \"\" {",
		Replace:  "\tif refused := \"\"; refused != \"\" {",
		Pkg:      "./internal/vibe/fleetapi/",
		MustFail: []string{"TestEveryWarmProducerConsultsTheClassGuard", "TestWarmTarget_RestoreRefusesAnEmbedClassModelAtFireTime"},
		Why: "model_classes is the declaration that an id must not receive a chat completion, and " +
			"until the 2026-08-05 live gate only warm_model honoured it. The structural rule is " +
			"what makes the next producer's omission visible; the behavioural test is what proves " +
			"the rung does something.",
	},

	// ── class 2 / the route table ─────────────────────────────────────
	{
		Name:     "c12/a route added without an access decision",
		File:     "internal/vibe/fleetapi/routes.go",
		Find:     "Path: \"/api/fleet/doctor\", Access: AccessTokenOnly",
		Replace:  "Path: \"/api/fleet/doctor\"",
		Pkg:      "./internal/vibe/fleetapi/",
		MustFail: []string{"TestRoutes_EveryRouteDeclaresAnAccessLevel"},
		Why: "Access has no safe zero value on purpose: 'the next agent forgot' must not be spelled " +
			"the same way as 'the next agent decided'. This plan added routes in eight of twelve phases.",
	},
	{
		Name:     "c12/the guest surface silently widens to the ledger",
		File:     "internal/vibe/fleetapi/routes.go",
		Find:     "Path: \"/api/fleet/usage\", Access: AccessTokenOnly",
		Replace:  "Path: \"/api/fleet/usage\", Access: AccessGuest",
		Pkg:      "./internal/vibe/fleetapi/",
		MustFail: []string{"TestRoutes_GuestSurfaceIsExactlyStateAndEvents"},
		Why: "a guest sees what the fleet is doing NOW. Tokens per cell per day is a record of when " +
			"this house works and when it was away.",
	},

	// ── class 4: a catalog that advertises what it cannot serve ───────
	{
		Name: "catalog/discovery falls back through to the upstream's own shape",
		File: "internal/vibe/proxy/proxy.go",
		Find: "\tif r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, \"/v1/models\") {\n" +
			"\t\tp.serveModels(w, r, def, rw)\n\t\treturn\n\t}",
		Replace: "\tif false && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, \"/v1/models\") {\n" +
			"\t\tp.serveModels(w, r, def, rw)\n\t\treturn\n\t}",
		Pkg: "./internal/vibe/proxy/",
		MustFail: []string{
			"TestProxy_OllamaShapedUpstreamIsServedAsOpenAI",
			"TestProxy_UnrecognisedCatalogShapeIsNotForwarded",
			"TestOllamaShapedCell_CatalogIDIsRoutable",
		},
		Why: "this is the novodoo defect exactly: with no routes and no rewrite the proxy forwarded " +
			"whatever shape its upstream emitted, so an Ollama-shaped backend made the cell an " +
			"Ollama-shaped peer. Every consumer in this repo reads data[] — vamp then substitutes " +
			"the literal id \"vibe\" — so the cell advertises a model, a client pins the id, and the " +
			"completion 404s while the cell serves fine.",
	},
	{
		Name:     "catalog/an unreadable shape becomes an empty catalog",
		File:     "internal/vibe/modelcat/modelcat.go",
		Find:     "\tif !haveData && !c.hasOllama {\n\t\treturn nil, ErrShape\n\t}",
		Replace:  "\tif false && !haveData && !c.hasOllama {\n\t\treturn nil, ErrShape\n\t}",
		Pkg:      "./internal/vibe/modelcat/",
		MustFail: []string{"TestParse_UnrecognisedShapeIsAnErrorNotAnEmptyCatalog"},
		Why: "a shape nobody recognised must not become a valid-looking empty catalog. \"This cell " +
			"serves nothing\" is a CLAIM, and a parser that failed has no standing to make it — the " +
			"quiet version of that claim is indistinguishable from a genuinely idle cell, which is " +
			"how an unroutable peer stayed in the fleet's catalog undetected.",
	},

	// ── the completeness tables ───────────────────────────────────────
	{
		Name:     "c19/a fleet state file falls outside the mirror",
		File:     "internal/vibe/fleetmirror/mirror.go",
		Find:     "producer: \"LeasesFile\"",
		Replace:  "producer: \"LeasesFileTypo\"",
		Pkg:      "./internal/vibe/fleetmirror/",
		MustFail: []string{"TestMirrorCoversEveryFleetStateFile"},
		Why: "the mirror's whole value is completeness, and the way it stops being complete is a " +
			"later phase adding a state file and nobody noticing — which is how C7a's ledger, C9's " +
			"notify scope and C11's holds each arrived.",
	},

	// ── C20's own scans, each proven to catch ─────────────────────────
	{
		Name:     "c20/an observed.Value read with the known bit discarded",
		File:     "internal/vibe/fleetapi/activity.go",
		Find:     "\tcount, reported := s.inFlight[cell].Observed()",
		Replace:  "\tcount, _ := s.inFlight[cell].Observed()\n\treported := true",
		Pkg:      "./internal/vibe/observed/",
		MustFail: []string{"TestObservedIsNeverReadWithADiscardedKnownBit"},
		Why: "`n, _ := x.Observed()` is the one hole the type leaves open. It is legal Go and " +
			"occasionally correct; it is also the exact line six of this fleet's defects were spelled as.",
	},
	{
		Name:    "c20/a new (value, known-bit) field pair",
		File:    "internal/vibe/fleetapi/fleetapi.go",
		Find:    "\tinFlight map[string]observed.Value[int]",
		Replace: "\tinFlight map[string]observed.Value[int]\n\tresidentGB float64\n\tresidentGBKnown bool",
		Pkg:     "./internal/vibe/observed/",
		// The scan runs over the whole module, so a pair introduced
		// anywhere is caught from this one package's tests.
		MustFail: []string{"TestNoNewValueAndKnownBitFieldPair"},
		Why: "two fields that must agree is the shape observed.Value replaces. Every way of losing " +
			"the second one — a map miss, a dropped return, a delete — yields a confident zero.",
	},
	{
		Name:     "c20/a new (measurement, bool) return",
		File:     "internal/vibe/fleetapi/activity.go",
		Find:     "// activityFor derives one cell's activity block.",
		Replace:  "func (s *Server) residentGB(cell string) (float64, bool) { return 0, s.cellUp[cell] }\n\n// activityFor derives one cell's activity block.",
		Pkg:      "./internal/vibe/observed/",
		MustFail: []string{"TestNoNewMeasurementAndBoolReturn"},
		Why: "a measurement plus a droppable bool, where the measurement's zero is a plausible " +
			"value, is the combination that disarmed eight busy guards on the v247 wire.",
	},
	{
		Name:     "c20/an unguarded cd in a shell rig",
		File:     "scripts/fleetlab/gate-c19-drill.sh",
		Find:     "cd -- \"$(dirname -- \"${BASH_SOURCE[0]}\")\" || exit 1",
		Replace:  "cd -- \"$(dirname -- \"${BASH_SOURCE[0]}\")\"",
		Pkg:      "./internal/shelllint/",
		MustFail: []string{"TestScriptsAreSafe"},
		Why: "gate-c13-parity.sh ran a bare `cd` under `set -uo pipefail` with no -e and then " +
			"git init / git config / git add -A / git commit. With a wrong FLEETLAB_DIR the cd " +
			"fails, the shell stays in the operator's CWD, and the rig commits their working tree.",
	},
	{
		Name:     "c21/a git write that does not name its repository",
		File:     "scripts/fleetlab/gate-c13-parity.sh",
		Find:     "git -C \"$DEFS\" init -q .",
		Replace:  "git init -q .",
		Pkg:      "./internal/shelllint/",
		MustFail: []string{"TestScriptsAreSafe"},
		Why: "the OTHER half of the C17 blocker. The cd rule catches the chdir that fails; this is " +
			"what a failed chdir then lets loose — `git init`, `git config user.*`, `git add -A`, " +
			"`git commit` in whatever repository the shell is standing in, which in the C17 " +
			"reproduction was the operator's own. gate-c13-parity.sh was rewritten to use `git -C` " +
			"everywhere BECAUSE of that incident, and nothing until now stopped the next rig " +
			"dropping it again.",
	},

	// ── the doctor's read-only promise ────────────────────────────────
	{
		Name:     "c13/a mutating verb reaches the doctor",
		File:     "internal/vibe/fleetapi/doctor.go",
		Find:     "func (s *Server) checkIntentHygiene(",
		Replace:  "func (s *Server) c20Scratch() { s.QueueCommand(\"c\", AnnounceCommand{}) }\n\nfunc (s *Server) checkIntentHygiene(",
		Pkg:      "./internal/vibe/fleetapi/",
		MustFail: []string{"TestDoctor_ReachesNoMutatingVerb"},
		Why: "`vibe fleet doctor` exists to be run mid-incident. Its read-only promise is pinned " +
			"structurally as well as behaviourally because the behavioural half only sees the " +
			"files a given run happened to touch.",
	},

	// ── the day-bucket rule ───────────────────────────────────────────
	{
		Name: "c7a/a day bucket computed by truncation",
		File: "internal/vibe/fleetapi/usage.go",
		Find: "\t\"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg\"\n)",
		// The forbidden literal is assembled here rather than written out,
		// for the same reason c7a_test.go assembles its needles: this file
		// is inside the tree that test walks, and a registry entry must not
		// be able to fail the rule it exists to check.
		Replace: "\t\"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg\"\n)\n\n" +
			"// c20 scratch\nvar _ = time.Now().Truncate(24 *" + " time.Hour)",
		Pkg:      "./internal/vibe/fleetapi/",
		MustFail: []string{"TestNoTruncateBasedDayBucketing"},
		Why: "Truncate rounds against absolute time and lands on UTC midnight regardless of the " +
			"value's Location — with no error, no type mismatch and no way to notice from the output.",
	},

	// ── the credential never leaks ────────────────────────────────────
	{
		Name:     "c9/the webhook URL stops being scrubbed",
		File:     "internal/vibe/fleetnotify/webhook.go",
		Find:     "func Redact(raw string) string {",
		Replace:  "func Redact(raw string) string {\n\tif raw != \"\" {\n\t\treturn raw\n\t}",
		Pkg:      "./internal/vibe/fleetnotify/",
		MustFail: []string{"TestRedact_KeepsSchemeAndHostAndDropsTheTopicQueryAndUserinfo"},
		Why: "an ntfy topic URL is bearer-equivalent. The sink unwraps *url.Error AND scrubs, and " +
			"both guards are pinned individually — neither may be deleted because 'the other one covers it'.",
	},
}

// ── tree copying and patching ────────────────────────────────────────

// RepoRoot walks up from dir until it finds go.mod.
func RepoRoot(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(abs, "go.mod")); err == nil {
			return abs, nil
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", errors.New("no go.mod above " + dir)
		}
		abs = parent
	}
}

// skipDirs are excluded from the working copy: version control, cached
// upstream binaries, agent scratch space and build output are all
// irrelevant to a mutation and dominate the copy time.
//
// .claude is not merely scratch. This repo runs one git WORKTREE per
// parallel agent under .claude/worktrees/, so copying it means copying a
// complete second (and ninth) checkout of this module into every worker's
// tree — and then the module-wide scans in internal/vibe/observed and
// internal/shelllint, which several registry entries name as their
// MustFail test, run over all of them. astscan.ForeignDir below is the
// general form; this entry is the cheap one that stops the walk before it
// stats nine thousand files.
var skipDirs = map[string]bool{".git": true, ".upstream": true, ".claude": true, "__pycache__": true, "node_modules": true}

// CopyTree makes a working copy of the repo. Cheap enough to do per
// worker (~8 MB of source) and the only way two mutations can be applied
// concurrently without one seeing the other's edit.
func CopyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] || astscan.ForeignDir(src, path) {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		// A git WORKTREE's .git is a file pointing at the main repo, not a
		// directory, so the skip list above never sees it.
		if !d.Type().IsRegular() || skipDirs[d.Name()] {
			return nil
		}
		return copyFile(path, filepath.Join(dst, rel))
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Occurrences counts Find in the file, which is what the staleness guard
// asserts is exactly 1.
func (m Mutation) Occurrences(root string) (int, error) {
	b, err := os.ReadFile(filepath.Join(root, m.File))
	if err != nil {
		return 0, err
	}
	return strings.Count(string(b), m.Find), nil
}

// Apply writes the mutated file and returns the original bytes so the
// caller can restore it. It refuses on anything other than exactly one
// match: a mutation applied to the wrong copy of an ambiguous pattern
// would keep passing while covering something else entirely.
func (m Mutation) Apply(root string) (original []byte, err error) {
	path := filepath.Join(root, m.File)
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if n := strings.Count(string(b), m.Find); n != 1 {
		return nil, fmt.Errorf("%s: Find matches %d times in %s, want exactly 1 — the entry is STALE", m.Name, n, m.File)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	mutated := strings.Replace(string(b), m.Find, m.Replace, 1)
	if err := os.WriteFile(path, []byte(mutated), info.Mode().Perm()); err != nil {
		return nil, err
	}
	return b, nil
}

// Restore puts the original bytes back, preserving the mode.
//
// The mode matters: one registry entry mutates an executable shell rig,
// and restoring it 0o644 would leave the worker's tree subtly different
// from the one the baseline ran against — the sort of drift that makes a
// later entry in the same worker fail for a reason nobody can find.
func (m Mutation) Restore(root string, original []byte) error {
	path := filepath.Join(root, m.File)
	perm := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		perm = info.Mode().Perm()
	}
	return os.WriteFile(path, original, perm)
}
