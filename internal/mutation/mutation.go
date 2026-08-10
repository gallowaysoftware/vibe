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

	// ── a recorder that becomes an actuator (C24) ─────────────────────
	{
		Name:     "c24/a recorded stop is handed back to the cell as a command",
		File:     "internal/vibe/fleetapi/announce.go",
		Find:     "\tstopRecord := hasRequest && IsStopRecord(&req2)",
		Replace:  "\tstopRecord := false && hasRequest && IsStopRecord(&req2)",
		Pkg:      "./internal/vibe/fleetapi/",
		MustFail: []string{"TestC24StopRecordIsNeverHandedBackAsACommand", "TestC24StopRecordLosesToTheCellsOwnDrain"},
		Why: "a cell unit's ExecStopPost hook writes that it stopped. Treated as an ordinary intent " +
			"REQUEST, that record is handed to the announcing cell as desired_intent, and " +
			"fleetannounce.reconcile answers a drained one by RUNNING cell_cmds.drain — so the hook " +
			"stops the serving stack of a box that has just come back, on the first heartbeat, " +
			"through a path nothing in the hook or the unit file can see.",
	},
	{
		Name:     "c24/a unit's stop overwrites the declaration that knows why",
		File:     "internal/vibe/fleetapi/intent.go",
		Find:     "\t\t\tif cur, had := next[cell]; had && cur.State == \"drained\" && !IsStopRecord(&cur) {",
		Replace:  "\t\t\tif cur, had := next[cell]; false && had && cur.State == \"drained\" && !IsStopRecord(&cur) {",
		Pkg:      "./internal/vibe/fleetapi/",
		MustFail: []string{"TestC24StopRecordNeverOverwritesADeclaration"},
		Why: "C14's scheduled suspend records {drained, asleep per sleep_schedule, eta 07:15} and THEN " +
			"takes the box down through the same unit stop that fires the hook. A stop record that " +
			"overwrites it makes the fleet forget it put the box to sleep: the doctor calls the night " +
			"undeclared and the wake's clear-first ordering has a different record to clear than the " +
			"one it wrote. A human's --reason gaming is the same shape.",
	},

	// ── a new display state that stops paging (C27) ───────────────────
	{
		Name:     "c27/a stopped always_on cell no longer pages",
		File:     "internal/vibe/fleetapi/notify.go",
		Find:     "\tcase DisplayStopped, DisplayDrainedQ, DisplayOffAway, DisplayOffAwayQ:",
		Replace:  "\tcase DisplayDrainedQ, DisplayOffAway, DisplayOffAwayQ:",
		Pkg:      "./internal/vibe/fleetapi/",
		MustFail: []string{"TestC27StoppedAlarmsForAnAlwaysOnCell", "TestC24StopRecordDoesNotSilenceTheAlwaysOnAlarm"},
		Why: "absentAlarm's switch ends in `default: return \"\", false` — no alarm. A \"down\" state " +
			"missing from its case list therefore stops paging SILENTLY, and STOPPED is written by " +
			"the cell unit's own stop hook, which a crash fires exactly as `systemctl stop` does. " +
			"The heavy cell dies at 03:00, the box says \"I stopped\" on the way down, and the " +
			"notifier answers by going quiet for the one incident it exists for.",
	},

	{
		Name: "c27/the state list an agent reads loses two states",
		File: "internal/vibe/fleetmcp/fleetmcp.go",
		Find: "\t\t\t\t\"STOPPED / DRAINED? / OFF / OFF/AWAY / OFF/AWAY? / INCONSISTENT) with resident \" +",
		// The two deleted names are still SUBSTRINGS of the one that
		// remains, which is exactly how the old guard stayed green.
		Replace:  "\t\t\t\t\"STOPPED / DRAINED? / OFF/AWAY? / INCONSISTENT) with resident \" +",
		Pkg:      "./internal/vibe/fleetmcp/",
		MustFail: []string{"TestC27FleetStatusDescribesEveryDisplayState"},
		Why: "the tool description is the ONLY place an agent learns what a `display` value means, and " +
			"OFF ⊂ OFF/AWAY ⊂ OFF/AWAY?. The guard used to ask `strings.Contains(list, state)`, so " +
			"deleting OFF and OFF/AWAY from the list left both as substrings of the survivor and the " +
			"test stayed green — while an agent reading the list had never heard of either, and OFF is " +
			"the state a whole box being gone renders as. Matched on TOKENS now, both directions.",
	},

	// ── class 1 again, on the read surfaces ───────────────────────────
	{
		Name: "page/a dead fleetd keeps rendering green",
		File: "internal/vibe/fleetapi/fleet.html",
		Find: `  {"level":"offline","min_age_s":150,"cls":"lv-offline","neutralise":true,"head":"OFFLINE",`,
		// The banner still turns red; only the TABLE goes back to
		// asserting a health it has no evidence for.
		Replace:  `  {"level":"offline","min_age_s":150,"cls":"lv-offline","neutralise":false,"head":"OFFLINE",`,
		Pkg:      "./internal/vibe/fleetapi/",
		MustFail: []string{"TestFleetPage_OfflineStateFromHandlerToDOM"},
		Why: "this is the phone-in-the-hallway surface. Before the liveness ladder, refresh() threw, " +
			"every caller was an empty catch and render() ran only on success — so a dead fleetd left " +
			"the table frozen with every SERVING badge green, cued by a timestamp in 12px grey. A " +
			"badge's colour is a claim about a value OBSERVED now; once the observations stop " +
			"arriving, leaving it green is absent evidence read as a healthy value.",
	},
	{
		Name: "page/a later rule repaints a neutralised badge green",
		File: "internal/vibe/fleetapi/fleet.html",
		Find: "  body.notlive #cells { opacity: .8; }",
		// Later in the sheet and the same specificity, so it WINS —
		// while `body.notlive .badge`, the rule every previous guard
		// read, is left untouched and still says --gray.
		Replace:  "  body.notlive #cells { opacity: .8; }\n  body.notlive .b-serving { color: var(--green); }",
		Pkg:      "./internal/vibe/fleetapi/",
		MustFail: []string{"TestFleetPage_NeutralisedBadgesAreNotGreen"},
		Why: "the ladder can be perfect and the page still lie, because what a reader SEES is the " +
			"cascade's answer, not one rule's text. The guard this replaced grepped the neutralising " +
			"rule for '--green' and would pass this mutation unchanged; the current one resolves the " +
			"cascade and paints the badge, so the last-wins rule is what it reads.",
	},
	{
		Name:     "page/the tool-result panel moves back inside the table render() rebuilds",
		File:     "internal/vibe/fleetapi/fleet.html",
		Find:     "  <div id=\"toolresult\" hidden>",
		Replace:  "  <div id=\"cells\"><div id=\"toolresult\" hidden>",
		Pkg:      "./internal/vibe/fleetapi/",
		MustFail: []string{"TestFleetPage_ToolResultsSurviveTheNextRender"},
		Why: "drain_cell's reply — 'WARNING: the requested wait was SKIPPED … cancelled any streams' plus " +
			"the lease list — arrives AFTER the drain has happened and is two hundred characters. " +
			"flash() re-polls 1.5s later, so anything inside the element render() replaces is " +
			"destroyed before it is read. The containment is now computed from the served markup's " +
			"nesting rather than inferred from a word not appearing in a function body.",
	},
	{
		Name:     "savings/a CAD electricity bill is subtracted from a USD gross",
		File:     "internal/vibe/fleetapi/savings.go",
		Find:     "func (c CurrencyInfo) canNet() bool { return !c.Mixed }",
		Replace:  "func (c CurrencyInfo) canNet() bool { return true }",
		Pkg:      "./internal/vibe/fleetapi/",
		MustFail: []string{"TestSavings_MixedCurrencyRefusesToNet", "TestSavings_MixedCurrencyRemovesThePaybackBarAndSaysWhy"},
		Why: "every rate on the token side comes out of internal/vibe/prices normalized to USD; " +
			"electricity and capital are the household's own money. This fleet's owner is Canadian. " +
			"`net = gross − power` across the two is not an approximation — it is a different " +
			"quantity, and it renders as a perfectly plausible dollar figure with no symptom a reader " +
			"could ever notice. canNet is the single gate every subtraction and every payback ratio " +
			"in that file goes through.",
	},
	{
		Name: "ps/an unanswered ping is reported as a stopped daemon",
		File: "internal/vibe/cli/client.go",
		// The pre-fix behaviour exactly: EVERY ping failure is absence.
		// The blank assignments keep `errors` and `connect` used, because a
		// mutation that does not compile proves nothing.
		Find: "\tif errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {\n" +
			"\t\treturn false\n\t}\n\treturn connect.CodeOf(err) == connect.CodeUnavailable",
		Replace:  "\t_ = errors.Is\n\t_ = connect.CodeOf\n\treturn true",
		Pkg:      "./internal/vibe/cli/",
		MustFail: []string{"TestPSRefusesToCallASlowDaemonAStoppedOne", "TestDaemonAbsentSeparatesNoDaemonFromNoAnswer"},
		Why: "`vibe ps --json` is a document a script acts on, and its daemon_running field is a " +
			"CLAIM. Only a refused or absent socket supports it; a ping that spent its 500ms supports " +
			"nothing. Without this discrimination a daemon that is up with a model resident and merely " +
			"slow to answer renders as {\"daemon_running\": false, \"active\": null} at exit 0 — the " +
			"absent-evidence-as-a-definite-value class, on the one surface that is machine-readable. " +
			"The helper is pinned rather than the call site because `daemonAbsent` is the whole of the " +
			"discrimination and 'why not just treat every error the same?' reads as a simplification.",
	},
	// daemonAbsent has four callers and the entry above pins the helper.
	// These two pin the CALL SITES, which is a different failure: the
	// helper can be perfect and a command can still not ask it. It was
	// exactly that for as long as the helper existed — `ps` consulted it,
	// `env` and `shutdown` did not, and the sweep that added it to one
	// surface is the sweep a future agent will repeat.
	{
		Name: "env/an unanswered ping exports nothing and says nothing",
		File: "internal/vibe/cli/cmd_env.go",
		// The pre-fix line, restored exactly: any ping failure is silence
		// at exit 0.
		Find:     "\t\t\t\tif !daemonAbsent(err) {",
		Replace:  "\t\t\t\tif false {",
		Pkg:      "./internal/vibe/cli/",
		MustFail: []string{"TestEnvRefusesToCallASlowDaemonAnAbsentOne"},
		Why: "`eval \"$(vibe env)\"` is how a frontend is pointed at the local front, and printing " +
			"nothing is the CORRECT answer when no profile is active — which is why this failure is " +
			"invisible. A daemon that is up with a model resident and merely slow to answer produces " +
			"the identical empty stdout at exit 0, the frontend falls back to its built-in vendor " +
			"endpoint, and the operator is billed for tokens the local front was ready to serve. No " +
			"log line, no exit code, no symptom other than the invoice.",
	},
	{
		Name:     "shutdown/a busy daemon is reported as an absent one",
		File:     "internal/vibe/cli/cmd_shutdown.go",
		Find:     "\t\t\t\tif !daemonAbsent(err) {",
		Replace:  "\t\t\t\tif false {",
		Pkg:      "./internal/vibe/cli/",
		MustFail: []string{"TestShutdownRefusesToCallASlowDaemonAStoppedOne"},
		Why: "the command whose entire job is to talk to a daemon that is BY CONSTRUCTION busy — it " +
			"is holding a model and it is about to be asked to tear the stack down. Under this " +
			"mutation `vibe shutdown && <next step>` prints \"daemon not running\", exits 0 and runs " +
			"the next step against a live daemon with the GPU still occupied. Idempotence is the " +
			"right goal and it is preserved for genuine absence; what this pins is that the goal is " +
			"not allowed to invent the evidence.",
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
	{
		Name:     "c9/only the whole URL is scrubbed, not the parts that carry the bearer",
		File:     "internal/vibe/fleetnotify/webhook.go",
		Find:     "\tfor _, part := range scrubbableParts(u) {\n\t\tout = strings.ReplaceAll(out, part, redacted)\n\t}",
		Replace:  "\t_ = scrubbableParts(u)",
		Pkg:      "./internal/vibe/fleetnotify/",
		MustFail: []string{"TestScrubURL_RemovesACredentialThatIsNotInThePath", "TestWebhookSink_ErrorBodyIsScrubbedOfTheTopic"},
		Why: "the far side quotes a FRAGMENT of the request, never the string vibe sent — an echoed " +
			"r.RequestURI is path+query with no scheme and no host, so the whole-URL match cannot see " +
			"it. `?auth=<token>` is a real webhook shape and, when the path is a bare \"/\", the path " +
			"fallback does not fire either: the whole-URL match was the only guard and a quoted " +
			"fragment walked past it. Scrub feeds deliver.go's stats.LastError, which fleetapi " +
			"publishes on GET /api/fleet/state — an AccessGuest route — and in the fleet_status MCP " +
			"tool, so a credential that survives this loop is a credential on the guest surface.",
	},
	{
		Name:     "vamp/a webhook stage stops scrubbing its own URL out of an error",
		File:     "internal/vamp/webhook_executor.go",
		Find:     "\treturn &scrubbedError{msg: fleetnotify.ScrubURL(rawURL, msg), cause: err}",
		Replace:  "\treturn &scrubbedError{msg: msg, cause: err}",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestWebhookExecutor_ErrorTextQuotingTheURLIsScrubbed"},
		Why: "a Slack/Discord incoming-webhook URL carries its bearer in the PATH. A vamp stage error " +
			"becomes Executor.FailureSummary, and six executors hand that to templates as " +
			"{{ .failure_summary }} — so an unscrubbed transport error means the run's own " +
			"run_when: failure webhook POSTS the credential into the chat channel. Pinned " +
			"separately from the unwrap below: each covers a leak the other does not.",
	},
	{
		Name:     "vamp/a webhook stage stops unwrapping *url.Error",
		File:     "internal/vamp/webhook_executor.go",
		Find:     "\tvar ue *url.Error\n\tif errors.As(err, &ue) && ue.Err != nil {\n\t\tmsg = ue.Err.Error()\n\t}",
		Replace:  "\tvar ue *url.Error\n\t_ = ue",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestScrubURLError_DropsAURLTheScrubberCannotMatch"},
		Why: "*url.Error embeds a URL STRUCTURALLY, and it is not always the one the stage rendered: " +
			"http.Client follows redirects and names the hop it failed on. String scrubbing keyed on " +
			"the stage's own URL cannot match that. Dropping the wrapper is the half that does not " +
			"depend on the two spellings agreeing.",
	},
	{
		Name:     "vamp/the run log prints the webhook URL",
		File:     "internal/vamp/webhook_executor.go",
		Find:     "\t\tfmt.Fprintf(in.Log, \"webhook: %s %s\\n\", method, fleetnotify.Redact(urlStr))",
		Replace:  "\t\tfmt.Fprintf(in.Log, \"webhook: %s %s\\n\", method, urlStr)",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestWebhookExecutor_LogDoesNotCarryTheURL"},
		Why: "under --detach the run log is a FILE in the run dir, written every run and kept. A URL " +
			"printed there is the credential persisted at rest, which is how a leak survives the " +
			"session that caused it.",
	},
	{
		Name:     "vamp/a webhook stage stops scrubbing what the far side said",
		File:     "internal/vamp/webhook_executor.go",
		Find:     "\tdefer func() { retErr = scrubResponseError(retErr, urlStr, resp, renderedHeaders) }()",
		Replace:  "\t_ = scrubResponseError",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestWebhookExecutor_ErrorBodyQuotingTheURLIsScrubbed", "TestWebhookExecutor_AssertStatusMismatchBodyIsScrubbed", "TestWebhookExecutor_TransientErrorBodyIsScrubbed", "TestWebhookExecutor_EchoedCredentialHeaderIsScrubbed", "TestWebhookExecutor_FailureSummaryDoesNotPostTheCredential"},
		Why: "the two entries above scrub errors the TRANSPORT produced. This one scrubs the error " +
			"the SERVER produced, which is the shorter path to a chat channel: for Slack, Discord " +
			"and ntfy the URL PATH is the bearer, and a 404 page routinely quotes the request line " +
			"back — so the credential arrives inside far-side prose, where there is no *url.Error " +
			"to unwrap. It is a DEFERRED rewrite over the whole response phase rather than a scrub " +
			"at each return because that is how this leak survived being closed twice: the fix " +
			"reached the sites someone had in mind (the *url.Error paths) and not the non-2xx body " +
			"preview, the assert.status_code mismatch preview or the assert failure list. Deleting " +
			"the defer reopens every one of them at once, and the return added next year with it.",
	},
	{
		Name:     "vamp/a webhook stage stops scrubbing its URL out of the far side's prose",
		File:     "internal/vamp/webhook_executor.go",
		Find:     "\tmsg := fleetnotify.ScrubURL(reqURL, err.Error())",
		Replace:  "\tmsg := err.Error()",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestWebhookExecutor_BodyIsScrubbedWhenTheResponseCarriesNoRequest"},
		Why: "pinned separately from the defer above because the defer only decides WHEN the scrub " +
			"runs; this is the scrub. It has to be ScrubURL and not a whole-string match: what a " +
			"server echoes is r.RequestURI — path+query, no scheme, no host — so the string vibe " +
			"sent never appears in the body, and a `?auth=<token>` webhook with a bare \"/\" path " +
			"has no path part to fall back on either. The named test is the ONE that separates this " +
			"guard from the resp.Request guard below: for an *http.Client the two URLs are the same " +
			"string and either scrub covers the other, so this entry first went in naming six tests " +
			"and the harness correctly reported all six still green. httpDoer is an interface, a " +
			"hand-built response has a nil Request, and there this is the only guard left.",
	},
	{
		Name:     "vamp/a webhook stage stops scrubbing the URL it was redirected to",
		File:     "internal/vamp/webhook_executor.go",
		Find:     "\tif resp != nil && resp.Request != nil && resp.Request.URL != nil {\n\t\tmsg = fleetnotify.ScrubURL(resp.Request.URL.String(), msg)\n\t}",
		Replace:  "\t_ = resp",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestWebhookExecutor_RedirectTargetQuotedInABodyIsScrubbed"},
		Why: "http.Client follows redirects, so the body that comes back was written by a host the " +
			"stage never addressed, about a URL the stage never rendered — and a scrub keyed on the " +
			"stage's own string cannot match it. resp.Request is where the URL actually requested " +
			"lives. Same class as youtube_executor.go's resumable session URI: a URL handed out by " +
			"the far side that IS an authorisation, and it reaches the same place — " +
			"{{ .failure_summary }} in a run_when: failure webhook that posts it into a room.",
	},
	{
		Name:    "vamp/vamp render resolves a dependency output path outside the run dir",
		File:    "internal/vamp/render.go",
		Find:    "\tif err := ensureUnderRunDir(out); err != nil {\n\t\treturn \"\", err\n\t}\n\treturn out, nil",
		Replace: "\treturn out, nil",
		Pkg:     "./internal/vamp/",
		MustFail: []string{
			"TestRenderStageOutputPath_RefusesAnEscapeFromTheRunDir",
		},
		Why: "renderTemplate-for-output-paths has five callers and this was the fifth. The other " +
			"four apply the run-dir rule — Executor.renderOutputPath, dryRunState.renderOutputPath " +
			"and both diff.go sites, one of which carries a ten-line comment naming this exact " +
			"threat — and cmd_render.go joins THIS result onto the run dir, os.ReadFile's it, and " +
			"binds the bytes into the printed prompt, so `vamp render --run-dir … --input " +
			"name=../outside/secret.txt` exited 0 and printed the file. The reachable inputs are " +
			"the pipeline YAML and --input rather than a sampled LLM string, so this is not the " +
			"executor-grade sink; it is pinned anyway because without it `render` resolves a path " +
			"`run` and `dry-run` both refuse, which is the \"a plan that lies\" failure one command " +
			"over. The function measured 100% line coverage while asserting nothing about what it " +
			"should REFUSE — which is why the entry, not the coverage number, is the guard.",
	},
	{
		Name:    "vamp/a template read helper climbs out of the directory it was handed",
		File:    "internal/vamp/exec.go",
		Find:    "\tfor _, seg := range strings.Split(filepath.ToSlash(path), \"/\") {\n\t\tif seg == \"..\" {\n\t\t\treturn fmt.Errorf(\"%s %s: %w (name the directory directly)\", fn, path, errTemplatePathTraversal)\n\t\t}\n\t}\n\treturn nil",
		Replace: "\treturn nil",
		Pkg:     "./internal/vamp/",
		MustFail: []string{
			"TestReadHelpers_RefuseATraversalOutOfTheDirectoryTheyWereGiven",
		},
		Why: "the executor's WRITE path is confined and its READ path was not, on the same " +
			"executor and from the same untrusted source: readFile's documented job is to chain a " +
			"prior stage's output into the next prompt, and a prior stage's output is whatever the " +
			"model wrote. `{{ readFile (printf \"%s/../id_rsa\" .runDir) }}` returned a private key " +
			"into the rendered prompt while the identical escape through `output:` was refused one " +
			"function over. joinPath is in the same entry's blast radius because filepath.Join " +
			"RESOLVES \"..\" rather than rejecting it, so an unguarded composer hands readFile a " +
			"clean path pointing outside the run dir. Deliberately traversal-only: an ABSOLUTE " +
			"path stays legal because reading a user's on-disk corpus is what these helpers are " +
			"for, and a mutation that \"finishes the job\" into a run-dir confinement would break " +
			"every lesson pipeline.",
	},
	{
		Name:    "vamp/a nested lesson glob collapses to bare leaf names",
		File:    "internal/vamp/exec.go",
		Find:    "\t\tdirs = append(dirs, name)",
		Replace: "\t\tdirs = append(dirs, filepath.Base(m))",
		Pkg:     "./internal/vamp/",
		MustFail: []string{
			"TestEnumerateDirs_NestedGlobKeepsTheParentSegment",
		},
		Why: "filepath.Base is not relative to anything — it is the last segment — and " +
			"enumerateDirs promises \"relative directory names\". Over a module-organised " +
			"curriculum (root/*/Lesson_*) four real lessons came back as " +
			"[\"Lesson_1\",\"Lesson_2\",\"Lesson_1\",\"Lesson_2\"]: parent lost, leaf duplicated, " +
			"none of them resolving under the root. The image fan-out then returned [] with a nil " +
			"error and the foreach logged \"no items to run\" — a green pipeline that described " +
			"zero diagrams, which is the same failure the zero-match error in this function was " +
			"written to eliminate, arriving through a different door. The mutation is the exact " +
			"line the bug lived on, so a future \"simplify to Base\" edit is caught by the test " +
			"rather than by the next multi-module curriculum.",
	},
	{
		Name:    "vamp/a lesson name that resolves to nothing reads as a lesson with no diagrams",
		File:    "internal/vamp/exec.go",
		Find:    "\tinfo, statErr := os.Stat(filepath.Join(lessonRoot, lesson))\n\tif statErr != nil || !info.IsDir() {\n\t\treturn \"\", fmt.Errorf(\"%w: %q under %s\", errLessonNotFound, lesson, lessonRoot)\n\t}",
		Replace: "\t_ = lessonRoot",
		Pkg:     "./internal/vamp/",
		MustFail: []string{
			"TestLessonHelpers_RefuseALessonThatNamesNothing",
		},
		Why: "enumerateDirs argues at length that a glob matching nothing must be an error " +
			"because a stale root is indistinguishable from a curriculum with no lessons once the " +
			"result is []. The three helpers that CONSUME the array never made that argument, and " +
			"the array need not come from enumerateDirs at all — the documented binding is " +
			"`.stages.list_lessons.output`, i.e. a model's own JSON. [\"Lesson_9999\"] against a " +
			"real root returned a well-formed empty fan-out with a nil error. The guard lives in " +
			"lessonImageDir so all three helpers get it from one place, and it is deliberately the " +
			"LESSON directory it stats, not images/: a real lesson with no images/ is zero units " +
			"of work correctly expressed and must stay [] — " +
			"TestEnumerateImagePairs_NoImagesIsEmptyArrayNotNull pins that and is right.",
	},
	{
		Name:    "vamp/an empty lessons array reads as a curriculum with no diagrams",
		File:    "internal/vamp/exec.go",
		Find:    "\tif len(lessons) == 0 {\n\t\treturn \"\", fmt.Errorf(\"enumerateImagePairs: the lessons array is empty; a producer that found no lessons is a fault, not a curriculum with no diagrams\")\n\t}",
		Replace: "\tif len(lessons) < 0 {\n\t\treturn \"\", fmt.Errorf(\"enumerateImagePairs: the lessons array is empty; a producer that found no lessons is a fault, not a curriculum with no diagrams\")\n\t}",
		Pkg:     "./internal/vamp/",
		MustFail: []string{
			"TestLessonHelpers_RefuseALessonThatNamesNothing",
		},
		Why: "the sibling of the entry above, and a separate guard: a producer can fail by naming " +
			"lessons that are not there OR by naming none at all, and a model under JSON-mode " +
			"pressure emits `[]` readily. Both used to produce an empty fan-out with a nil error, " +
			"after which executeForeachStage logs \"foreach array empty, no items to run\" and " +
			"returns nil — the run is green and nothing happened. `< 0` rather than a deletion " +
			"because it is the shape a careless \"loosen this\" edit actually takes, and it keeps " +
			"the error string in the tree so the entry cannot pass by accident.",
	},
	{
		Name:    "vamp/the svg ground-truth sidecar invents a value",
		File:    "internal/vamp/exec.go",
		Find:    "\t\tcase xml.StartElement:\n\t\t\tif t.Name.Local == \"text\" {\n\t\t\t\tif inText == 0 {\n\t\t\t\t\tcur.Reset()\n\t\t\t\t}\n\t\t\t\tinText++\n\t\t\t} else if inText > 0 {\n\t\t\t\tcur.WriteByte(' ')\n\t\t\t}\n\t\tcase xml.EndElement:\n\t\t\tif t.Name.Local == \"text\" && inText > 0 {\n\t\t\t\tinText--\n\t\t\t\tif inText == 0 {\n\t\t\t\t\tflush()\n\t\t\t\t}\n\t\t\t} else if inText > 0 {\n\t\t\t\tcur.WriteByte(' ')\n\t\t\t}",
		Replace: "\t\tcase xml.StartElement:\n\t\t\tif t.Name.Local == \"text\" {\n\t\t\t\tif inText == 0 {\n\t\t\t\t\tcur.Reset()\n\t\t\t\t}\n\t\t\t\tinText++\n\t\t\t}\n\t\tcase xml.EndElement:\n\t\t\tif t.Name.Local == \"text\" && inText > 0 {\n\t\t\t\tinText--\n\t\t\t\tif inText == 0 {\n\t\t\t\t\tflush()\n\t\t\t\t}\n\t\t\t}",
		Pkg:     "./internal/vamp/",
		MustFail: []string{
			"TestExtractSVGText_AdjacentTspansAreSeparated",
		},
		Why: "this helper's whole stated purpose is to hand a vision model GROUND TRUTH for the " +
			"numbers it might mis-read off a 896x896 raster. Sibling <tspan>s — what Inkscape and " +
			"matplotlib emit for any wrapped or stacked label — were concatenated with nothing " +
			"between them, so 12 and 34 arrived as \"1234\" and Total/Revenue as \"TotalRevenue\": " +
			"a confident value that appears nowhere in the diagram, which is strictly worse than " +
			"omitting it, because the sidecar exists to CORRECT the model's reading. Both arms are " +
			"in one mutation because deleting either alone still leaves a separator and the test " +
			"would not move — the honest mutation is the \"simplify away the space writes\" edit a " +
			"future reader would actually make. The function was at 0.0% coverage, so the entry " +
			"lands with its first test.",
	},
	{
		Name:    "vamp/an unresolvable home dir relocates the whole state tree",
		File:    "internal/vamp/paths.go",
		Find:    "\tif home != \"\" && err == nil {\n\t\treturn home\n\t}\n\treturn fallbackHome(err)",
		Replace: "\t_ = err\n\treturn home",
		Pkg:     "./internal/vamp/",
		MustFail: []string{
			"TestPaths_AreAbsoluteWithNoResolvableHome",
		},
		Why: "`home, _ := os.UserHomeDir()` left home == \"\" when $HOME is unset, and " +
			"filepath.Join(\"\", \".config\", \"vamp\") is the RELATIVE path \".config/vamp\" — so " +
			"ConfigHome, StateHome, PipelinesDir, CapabilitiesFile and RunsDir all silently " +
			"re-rooted onto the process CWD with nothing logged and nothing erroring. The first " +
			"symptom is a capability lookup that reads \"this capability is not configured\" " +
			"rather than \"I cannot find my configuration\". A HOME-less unit is the fleet's " +
			"normal remote-exec shape (SSH + systemd), not an exotic one. The assertion is IsAbs " +
			"rather than an exact path, so the entry survives a future change of fallback " +
			"directory but not a return to a relative one.",
	},
	{
		Name:     "vamp/a webhook stage stops scrubbing the credential header the far side echoed",
		File:     "internal/vamp/webhook_executor.go",
		Find:     "\t\tif value := headers[name]; len(value) >= minScrubbableHeaderValue && credentialHeader(name) {\n\t\t\tmsg = fleetnotify.ScrubURL(value, msg)\n\t\t}",
		Replace:  "\t\t_ = headers[name]",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestWebhookExecutor_EchoedCredentialHeaderIsScrubbed"},
		Why: "the URL is not the only credential a webhook stage holds. The `env` template helper " +
			"exists so a bearer travels in an Authorization / X-Api-Key header instead of the " +
			"pipeline YAML, and gateways echo the header they rejected straight into their error " +
			"body. The same test pins the DELIBERATE narrowness beside it — an ordinary header " +
			"value must survive — because blanket-scrubbing everything we sent erases " +
			"\"application/json\" from a preview that says the server rejected exactly that, and a " +
			"preview of nothing but <redacted> is a leak traded for an unusable error.",
	},
	{
		Name:     "vamp/a youtube upload stops scrubbing its session URI out of an error",
		File:     "internal/vamp/youtube_executor.go",
		Find:     "\t\treturn \"\", fmt.Errorf(\"upload PUT: %w\", scrubURLError(uploadURL, err))",
		Replace:  "\t\treturn \"\", fmt.Errorf(\"upload PUT: %w\", err)",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestYouTubeExecutor_TransportErrorDoesNotCarryTheSessionURI", "TestYouTubeExecutor_SessionURIQuotedInAMessageIsScrubbed", "TestYouTubeExecutor_RedirectedSessionURIIsDropped"},
		Why: "a resumable-upload session URI is CREDENTIAL-EQUIVALENT: whoever holds it can append " +
			"to, complete or abort that upload without the access token that created it. This is " +
			"the same trap as the webhook URL two entries up, arriving by the same two routes — " +
			"*url.Error embeds the URL structurally, and a transport can quote it in its own prose " +
			"— and reaching the same place, {{ .failure_summary }} in a run_when: failure webhook " +
			"that posts it into a chat channel.",
	},
	{
		Name:     "vamp/the run log prints the youtube session URI",
		File:     "internal/vamp/youtube_executor.go",
		Find:     "\t\tfmt.Fprintf(in.Log, \"youtube: uploading video bytes to %s\\n\", fleetnotify.Redact(uploadURL))",
		Replace:  "\t\tfmt.Fprintf(in.Log, \"youtube: uploading video bytes to %s\\n\", uploadURL)",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestYouTubeExecutor_LogRecordsARedactedSessionURI"},
		Why: "same persistence argument as the webhook log line: under --detach this is a FILE " +
			"kept after the run. Redact rather than silence, because a foreach publishing twelve " +
			"episodes still has to tell its twelve sessions apart.",
	},

	{
		Name:     "vamp/every vamp subprocess loses its kill grace",
		File:     "internal/vamp/subprocess.go",
		Find:     "\tcmd.WaitDelay = subprocessKillGrace",
		Replace:  "\tcmd.WaitDelay = 0",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestPandocCommand_DoesNotPutTheChildInItsOwnProcessGroup"},
		Why: "a WaitDelay of ZERO is documented to mean 'wait indefinitely'. Cmd.Wait does not return " +
			"until the stdout/stderr PIPES close, and a killed ffmpeg's descendant wedged in " +
			"uninterruptible I/O never closes them — so a cancelled stage's deadline fires exactly " +
			"on time and the call still never comes back, which on the wire is indistinguishable " +
			"from the bound not working at all. The same test pins the DELIBERATE absence of " +
			"Setpgid beside it: that pairing was tried in vamp and rejected, because a process " +
			"group of its own takes the child out of the terminal's foreground group and ctrl-C " +
			"stops reaching a forty-minute render.",
	},

	// ── a container that outlives the run that started it (vamp) ──────
	//
	// `docker run` is the ONE subprocess in this package whose work does
	// not live in the child. subprocess.go's WaitDelay ends the client and
	// no process-group signal could ever reach the container, because
	// dockerd owns it — so the name and the Cancel hook are the entire
	// mechanism, and each half is pinned separately.
	{
		Name:     "vamp/a cancelled pandoc leaves its container running",
		File:     "internal/vamp/pandoc_executor.go",
		Find:     "\tcmd := command(ctx, binary, args...)\n\tif containerName == \"\" {\n\t\treturn cmd\n\t}",
		Replace:  "\tcmd := command(ctx, binary, args...)\n\tif true || containerName == \"\" {\n\t\treturn cmd\n\t}",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestNewPandocCommand_CancelKillsTheContainerNotJustTheClient"},
		Why: "killing `docker run` kills a CLIENT. The container keeps converting, keeps its bind " +
			"mount on the run dir, and with --rm nothing ever cleans up after it — so a cancelled " +
			"overnight pipeline leaves a process the operator cannot see from `ps` still writing " +
			"into a run they believe is over.",
	},
	{
		Name:     "vamp/the pandoc container stops being named",
		File:     "internal/vamp/pandoc_executor.go",
		Find:     "\t\tif containerName != \"\" {\n\t\t\t// BEFORE the image name",
		Replace:  "\t\tif false && containerName != \"\" {\n\t\t\t// BEFORE the image name",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestBuildPandocArgs_NamesTheContainerBeforeTheImage", "TestPandocExecutor_DockerFallbackIsNamed"},
		Why: "the name is the only handle that crosses the daemon boundary, and it is the only one " +
			"that survives THIS process: after a SIGKILL there is no Cancel hook and no defer, and " +
			"`docker ps --filter name=vamp-pandoc-` is all a human has left. An unnamed container " +
			"is an orphan nobody can find.",
	},
	{
		Name:     "vamp/two pandoc stages can collide on one container name",
		File:     "internal/vamp/pandoc_executor.go",
		Find:     "\t\tfmt.Sprintf(\"%d-%s\", itemIdx, hex.EncodeToString(tail[:]))",
		Replace:  "\t\tfmt.Sprintf(\"%d-%s\", itemIdx, hex.EncodeToString([]byte(\"fix\")))",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestPandocContainerName_IsUniquePerInvocationAndGreppable"},
		Why: "`docker run --name` FAILS on a name already in use. A retry whose predecessor is " +
			"still shutting down, or two foreach items landing in the same millisecond, would then " +
			"die on a conflict that has nothing to do with the document being converted.",
	},
	{
		Name:     "vamp/an already-exited container is reported as a failed reap",
		File:     "internal/vamp/pandoc_executor.go",
		Find:     "\tif err == nil || containerAlreadyGone(err) {\n\t\treturn nil\n\t}",
		Replace:  "\tif err == nil {\n\t\treturn nil\n\t}",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestReapDockerContainer_AlreadyExitedIsNotAFailure", "TestDockerCLIKiller_FoldsTheDaemonMessageIntoTheError"},
		Why: "`docker kill` RACES the container's own exit, and winning that race is the ordinary " +
			"shape of a cancelled stage. Reporting it puts an alarming line in the log of every " +
			"correctly-cancelled run, which is how an operator learns to stop reading the lines " +
			"that exist to be read.",
	},
	{
		Name:     "vamp/the container reap can hang teardown",
		File:     "internal/vamp/pandoc_executor.go",
		Find:     "\tctx, cancel := context.WithTimeout(context.Background(), dockerKillTimeout)",
		Replace:  "\tctx, cancel := context.WithCancel(context.Background())",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestReapDockerContainer_IsBounded"},
		Why: "class 4 again — a deadline that is present but never reached. The reap runs while the " +
			"caller is ALREADY tearing down, on a context deliberately detached from the cancelled " +
			"stage's, so an unreachable dockerd with no bound of its own turns a stage that timed " +
			"out exactly on schedule into a run that never returns.",
	},

	// ── a model's string reaching the filesystem (vamp) ───────────────
	{
		Name: "vamp/the run-dir containment rule stops containing",
		File: "internal/vamp/exec.go",
		// Keeps `cleaned` used so the mutation still compiles: a build
		// failure proves nothing about the guard.
		Find:     "\tcleaned := filepath.Clean(out)\n\tif filepath.IsAbs(cleaned) || cleaned == \"..\" || strings.HasPrefix(cleaned, \"..\"+string(filepath.Separator)) {",
		Replace:  "\tcleaned := filepath.Clean(out)\n\tif false && (filepath.IsAbs(cleaned) || cleaned == \"..\" || strings.HasPrefix(cleaned, \"..\"+string(filepath.Separator))) {",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestRenderOutputPathRejectsRunDirEscape", "TestDryRunRenderOutputPathRejectsRunDirEscape", "TestTryResumeStage_SurfacesRunDirEscape"},
		Why: "a foreach item map is parsed from a PRIOR STAGE'S LLM OUTPUT and its fields are " +
			"interpolated into the stage's output: template; the result reaches " +
			"filepath.Join(RunDir, path) at four sites with nothing cleaning it. Two are writes " +
			"(a sampled \"../../etc/cron.d/x\") and two are resume READS (a sampled \"/etc/passwd\" " +
			"loaded into a stage output, i.e. into the next prompt). Deleting this guard used to " +
			"leave all 39 packages green.",
	},

	// ── an empty artefact that outlives the run that made it (vamp) ───
	{
		Name:     "vamp/an ffmpeg stage stops checking that it produced anything",
		File:     "internal/vamp/ffmpeg_executor.go",
		Find:     "\tif info.Size() == 0 {\n\t\treturn fmt.Errorf(\"stage %s: %s produced 0-byte output at %s (likely an error swallowed by the exit code)\", stageID, what, path)\n\t}",
		Replace:  "\tif false && info.Size() == 0 {\n\t\treturn fmt.Errorf(\"stage %s: %s produced 0-byte output at %s\", stageID, what, path)\n\t}",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestFFmpegExecutor_ZeroByteOutputFailsTheStage", "TestRequireNonEmptyOutput"},
		Why: "ffmpeg exits 0 having written a 0-byte container when a filtergraph error never reaches " +
			"the exit status. The stage goes green, the run 'succeeds' with silence in it, and the " +
			"empty file is what gets cached — so the failure replays on every later run with the " +
			"same inputs until someone clears the cache by hand.",
	},
	{
		Name:     "vamp/a mix stage stops checking that it produced anything",
		File:     "internal/vamp/mix_executor.go",
		Find:     "\tif info.Size() == 0 {",
		Replace:  "\tif false && info.Size() == 0 {",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestMixExecutor_ZeroByteOutputFailsTheStage"},
		Why: "the same swallowed-filtergraph failure as the ffmpeg entry above, at the executor that " +
			"produces the FINISHED artefact — an m4b nobody plays until the episode ships. The " +
			"guard was added without a test of its own, which is the shape this registry exists " +
			"to make impossible.",
	},
	// ── a resumed stage that is not the stage that ran (vamp) ─────────
	{
		Name: "vamp/foreach resume classifies stage types by an allowlist again",
		File: "internal/vamp/exec.go",
		// The exact allowlist this line replaced: text and youtube of the
		// six content-bearing types.
		Find:     "\tbinary := producesFileOutput(stageTypeOrDefault(st))",
		Replace:  "\trt := stageTypeOrDefault(st)\n\tbinary := rt != StageTypeText && rt != StageTypeYouTube",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestTryResumeForeach_ClassifiesByOutputKindNotAnAllowlist"},
		Why: "the single-stage resume path was already fixed for this, with a comment ending " +
			"\"Classify by output kind, not an allowlist that forgot compact\" — and the foreach " +
			"path 60 lines later WAS that allowlist. A resumed foreach render/compact/webhook/" +
			"confirm item handed the next stage an absolute path where the prompt expects the " +
			"file's bytes: the model generates from memory, the run exits 0, and the host's " +
			"directory layout is in a model request.",
	},
	{
		Name:     "vamp/resume stops re-validating a foreach item's JSON",
		File:     "internal/vamp/exec.go",
		Find:     "\t\tif st.OutputFormat == \"json\" {\n\t\t\tif err := validateJSON(string(body)); err != nil {\n\t\t\t\tmissing = append(missing, i)\n\t\t\t\tcontinue\n\t\t\t}\n\t\t}",
		Replace:  "\t\tif false && st.OutputFormat == \"json\" {\n\t\t\tif err := validateJSON(string(body)); err != nil {\n\t\t\t\tmissing = append(missing, i)\n\t\t\t\tcontinue\n\t\t\t}\n\t\t}",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestTryResumeForeach_JSONRevalidationCoversEveryContentType"},
		Why: "size > 0 is the whole integrity check resume performs, and a run killed mid-write " +
			"leaves truncated JSON that passes it. The item then resumes as COMPLETE and every " +
			"downstream stage that parses it inherits the corruption. Deleting this branch used " +
			"to leave the package green.",
	},
	{
		Name:     "vamp/resume stops re-validating a single stage's JSON",
		File:     "internal/vamp/exec.go",
		Find:     "\tif st.OutputFormat == \"json\" {\n\t\tif err := validateJSON(string(body)); err != nil {",
		Replace:  "\tif false && st.OutputFormat == \"json\" {\n\t\tif err := validateJSON(string(body)); err != nil {",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestTryResumeStage_JSONRevalidation"},
		Why: "the non-foreach half of the same guard, and the one that feeds foreach: a truncated " +
			"titles.json resuming as complete is how a fan-out runs over half a list, or over " +
			"nothing, and reports success.",
	},
	{
		Name:     "vamp/an empty file resumes as a completed stage",
		File:     "internal/vamp/exec.go",
		Find:     "\tif info.Size() == 0 {\n\t\treturn nil, false, nil\n\t}",
		Replace:  "\tif false && info.Size() == 0 {\n\t\treturn nil, false, nil\n\t}",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestReadNonEmpty_ZeroByteIsNotAResult"},
		Why: "readNonEmpty exists to embody \"an empty file is not a result\" — it is NAMED for this " +
			"branch — and the half that makes the name true could be deleted with the whole " +
			"package staying green. A zero-byte output is the crashed-mid-write case --resume is " +
			"for; without it that stage is skipped and the empty string is what the next prompt " +
			"gets.",
	},
	{
		Name: "vamp/a foreach item's escaping path stops being a refusal",
		File: "internal/vamp/exec.go",
		Find: "\t\t\tif errors.Is(err, errOutputPathEscape) {\n\t\t\t\t// An item whose rendered path leaves the run dir is a",
		// Falls through to the "can't decide resumability" return, which
		// is exactly the quiet treatment the comment argues against.
		Replace:  "\t\t\tif false && errors.Is(err, errOutputPathEscape) {\n\t\t\t\t// An item whose rendered path leaves the run dir is a",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestTryResumeForeachStage_SurfacesRunDirEscape"},
		Why: "the foreach half of the refusal whose non-foreach twin is already registered above. " +
			"A foreach item is a field of a PRIOR STAGE'S LLM OUTPUT interpolated into the output " +
			"template; the next line joins the result to RunDir and READS it, so a sampled " +
			"\"/etc/passwd\" is loaded into a stage output — into the next prompt — while the run " +
			"reports merely \"nothing to resume\". Added with a five-line comment and no test.",
	},
	{
		Name:     "vamp/resume suppresses the foreach collision refusal",
		File:     "internal/vamp/exec.go",
		Find:     "\t\tif _, dup := seenPaths[path]; dup {",
		Replace:  "\t\tif _, dup := seenPaths[path]; false && dup {",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestResumeForeach_CollisionStaysRefused", "TestResumeForeach_NonTemplatedOutputStaysRefused"},
		Why: "executeForeachStage refuses two items that render to the same output path, but resume " +
			"marking the stage complete means the stage never runs and the error can never fire. " +
			"Both items then \"resume\" from the one file that exists: .outputs is N copies of one " +
			"body and the run reports success — a per-chapter TTS fan-out ships the same chapter " +
			"N times. The guard is on the path resume prevents you from reaching. It covers the " +
			"NON-TEMPLATED refusal too, which is why two tests are named: a static output path " +
			"renders to the same constant for every item and arrives here as a collision.",
	},
	{
		Name:     "vamp/foreach resume pairs prior files to items by position",
		File:     "internal/vamp/exec.go",
		Find:     "\tif !e.foreachPathsRecordItems(st, items, outPaths) && !e.resumedFromPriorRun(st.Foreach.From) {",
		Replace:  "\tif false && !e.foreachPathsRecordItems(st, items, outPaths) && !e.resumedFromPriorRun(st.Foreach.From) {",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestResumeForeach_IndexPathDoesNotPairByPositionAcrossADifferentList"},
		Why: "with an index-derived path (assets/img_{{.i}}.png) the only thing pairing an on-disk " +
			"file to an item is its POSITION, and nothing records which item produced it. When the " +
			"upstream could not resume it re-runs against a non-deterministic model and returns a " +
			"different list: a prior run over [a b c d] hands item w the body generated for a, and " +
			"the run exits 0 with two bodies labelled as items that were never generated.",
	},
	{
		Name:     "vamp/compact drops a chunk whose reply came back empty",
		File:     "internal/vamp/compact_executor.go",
		Find:     "\t\tif part == \"\" {",
		Replace:  "\t\tif false && part == \"\" {",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestCompactExecutor_AnEmptyChunkReplyIsAnErrorNotADeletion"},
		Why: "compact's doc comment sells it as the alternative to truncating, \"which silently drops " +
			"content\". stripModelArtifacts removes a leading <think> block, so a reasoning model " +
			"that emits only its reasoning reduces to \"\" — appended as that chunk's summary. The " +
			"chunk is then ABSENT from the compacted text, with nil error and nothing in the log, " +
			"and the study-guide prompt downstream is built from a summary with a hole in it.",
	},
	{
		Name: "vamp/compact adopts a pass that made the text bigger",
		File: "internal/vamp/compact_executor.go",
		// Restores the exact line this replaced.
		Find:     "\t\tif len(next) >= len(current) {\n\t\t\tc.logf(in, \"compact %s: pass %d returned %d chars for %d in (no progress); keeping the shorter text\\n\", st.ID, iter+1, len(next), len(current))\n\t\t\tbreak\n\t\t}",
		Replace:  "\t\tif len(next) >= len(current) {\n\t\t\tcurrent = next\n\t\t\tbreak\n\t\t}",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestCompactExecutor_ANonShrinkingPassKeepsTheSmallerText"},
		Why: "one token with an invisible consequence: the comment says \"stop rather than spin\" and " +
			"the code stopped AND kept the LARGER text. Measured 20,000 chars in, 50,018 out for " +
			"target_chars: 500, nil error, silent log — strictly worse than not running the pass, " +
			"and the next stage's context window is what finds out.",
	},
	{
		Name:     "vamp/resume stops noticing that the inputs changed",
		File:     "internal/vamp/exec.go",
		Find:     "\treturn e.checkResumeInputs()",
		Replace:  "\treturn nil",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestCheckResumeSnapshot_InputsAreDrift"},
		Why: "the drift hash covered the pipeline YAML and nothing the YAML is parameterised BY, so " +
			"`--input topic=cats` then `--resume --input topic=dogs` was accepted: the cat-era " +
			"stages resume as complete, the rest generate dogs, and one run dir describes both. " +
			"Inputs reach prompts through {{ .inputs.x }} — this is the same class the pipeline " +
			"hash already refuses, one rung down.",
	},
	{
		Name:     "vamp/a resumed run overwrites the record of what produced it",
		File:     "internal/vamp/exec.go",
		Find:     "\t\tif _, err := os.Stat(inputsPath); err == nil {\n\t\t\treturn nil\n\t\t}",
		Replace:  "\t\tif _, err := os.Stat(inputsPath); false && err == nil {\n\t\t\treturn nil\n\t\t}",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestSnapshot_ResumeKeepsThePriorRunsInputsRecord"},
		Why: "inputs.json is the only on-disk record of what the already-completed stages were " +
			"generated from, and `runs ls` and `vamp diff` both read it. Rewriting it on a forced " +
			"resume replaces the first half of the run's parameters with the second half's — the " +
			"pipeline snapshot beside it has carried exactly this carve-out for months.",
	},

	{
		Name:    "vamp/a mix stage races on the log it streams to",
		File:    "internal/vamp/mix_executor.go",
		Find:    "\tvar sink io.Writer = tail\n\tif in.Log != nil {\n\t\tsink = io.MultiWriter(tail, in.Log)\n\t}\n\tcmd.Stdout = sink\n\tcmd.Stderr = sink",
		Replace: "\tif in.Log != nil {\n\t\tcmd.Stdout = in.Log\n\t\tcmd.Stderr = io.MultiWriter(tail, in.Log)\n\t} else {\n\t\tcmd.Stderr = tail\n\t}",
		Pkg:     "./internal/vamp/",
		// The named test carries a chatty fake that talks on BOTH streams
		// at volume, so the detector has something to catch.
		MustFail: []string{"TestMixExecutor_NonZeroExitSurfacesFFmpegsOwnDiagnostic"},
		Why: "os/exec gives Stdout and Stderr separate pipes AND separate copying goroutines unless " +
			"the two fields compare equal. The sink vamp passes here is a per-item *bytes.Buffer " +
			"that executeForeachStage hands out unguarded, so the divergent form raced on every " +
			"foreach mix invocation with a live log — found by `go test -race`, and silent " +
			"corruption of the operator's log in production.",
	},
	{
		Name:     "vamp/a pandoc stage accepts an empty book",
		File:     "internal/vamp/pandoc_executor.go",
		Find:     "\tif err := requireNonEmptyOutput(st.ID, \"pandoc\", outAbs); err != nil {\n\t\treturn nil, err\n\t}",
		Replace:  "\tif err := requireNonEmptyOutput(st.ID, \"pandoc\", outAbs); false && err != nil {\n\t\treturn nil, err\n\t}",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestPandocExecutor_ZeroByteOutputFailsTheStage"},
		Why: "pandoc opens its output before it knows the conversion will work, so a missing LaTeX " +
			"engine or an undecodable cover leaves a 0-byte EPUB and exits 0 — and the docker " +
			"fallback adds a second route, since `docker run` reports the CLIENT's status. This " +
			"site checked existence only; the size half is what an empty book fails.",
	},
	{
		Name:     "vamp/a cacheable stage type loses its key composer",
		File:     "internal/vamp/cache_key.go",
		Find:     "\tcase StageTypePandoc:\n\t\t// pandoc was on stageCacheable's allow-list",
		Replace:  "\tcase StageType(\"pandoc-disabled\"):\n\t\t// pandoc was on stageCacheable's allow-list",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestCacheableAndKeyableAgree", "TestPandocCacheKeyDiscriminates"},
		Why: "stageCacheable is the ADVERTISEMENT and computeStageCacheKey is the PERFORMANCE, and " +
			"they were switches over the same domain in different functions. `pandoc` sat on the " +
			"first from the day the type landed and never appeared in the second, so every pandoc " +
			"stage reported `cache: miss` forever and re-ran a whole EPUB conversion each time — " +
			"while buildPandocArgs sorted its metadata keys 'so the cache key doesn't oscillate'. " +
			"This mutation is that defect exactly, and the named test is what makes it " +
			"unrepresentable rather than merely fixed.",
	},
	{
		Name:     "vamp/an empty raster is admitted to the SVG cache",
		File:     "internal/vamp/vision_executor.go",
		Find:     "\tif info, statErr := os.Stat(tmpPath); statErr != nil || info.Size() == 0 {",
		Replace:  "\tif info, statErr := os.Stat(tmpPath); false && (statErr != nil || info.Size() == 0) {",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestRasterizeSVG_ZeroBytePNGNeverEntersTheCache"},
		Why: "this cache is CONTENT-ADDRESSED and permanent. A 0-byte render renamed into place is " +
			"served by the Stat fast path to every later call for that SVG — every lesson reusing " +
			"the boilerplate diagram, on every future run — handing the vision model an empty " +
			"image with no error anywhere until somebody deletes the cache dir by hand.",
	},
	{
		Name:     "vamp/a stage's declared timeout stops being applied",
		File:     "internal/vamp/exec.go",
		Find:     "\tif st.Timeout <= 0 || stageTypeOrDefault(st) == StageTypeConfirm {\n\t\treturn exec.Execute(ctx, in)\n\t}",
		Replace:  "\tif true || st.Timeout <= 0 || stageTypeOrDefault(st) == StageTypeConfirm {\n\t\treturn exec.Execute(ctx, in)\n\t}",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestExecuteBounded_StageTimeoutEndsAHungStage"},
		Why: "the class the fleet plan calls 'a deadline that is present but never reached'. Every " +
			"unreachable this package is normally tested against answers in microseconds " +
			"(ECONNREFUSED, a missing binary); the failure the bound exists for is a model server " +
			"that ACCEPTS the connection and says nothing, which without it hangs the stage, the " +
			"run and the overnight pipeline behind it, with no error to report and nothing to resume.",
	},
	{
		Name:     "vamp/the exponential backoff cap stops capping",
		File:     "internal/vamp/exec.go",
		Find:     "\t\tnext := time.Duration(float64(backoff) * policy.Multiplier)\n\t\tif next > policy.MaxBackoff || next <= 0 {\n\t\t\tnext = policy.MaxBackoff\n\t\t}",
		Replace:  "\t\tnext := time.Duration(float64(backoff) * policy.Multiplier)\n\t\tif next <= 0 {\n\t\t\tnext = policy.MaxBackoff\n\t\t}",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestExecutor_ExponentialBackoffTiming"},
		Why: "max_backoff is the only thing between a four-attempt retry policy and a geometric " +
			"series. The test that names the cap used to pick numbers whose UNCAPPED value still " +
			"satisfied its own upper bound, so the cap could be deleted without any test noticing — " +
			"the mutation-verified-once-then-drifted shape this registry exists to catch.",
	},
	{
		Name:     "vamp/the timing table stops saying whether the run worked",
		File:     "internal/vamp/timing.go",
		Find:     "\t\tif stageFailed(s.Status) {\n\t\t\tfailed = append(failed, fmt.Sprintf(\"%s (%s)\", s.ID, s.Status))\n\t\t}",
		Replace:  "\t\tif false && stageFailed(s.Status) {\n\t\t\tfailed = append(failed, fmt.Sprintf(\"%s (%s)\", s.ID, s.Status))\n\t\t}",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestFormatTable_RendersStageStatus"},
		Why: "the end-of-run table is what a human reads. Without the verdict a run with two failed " +
			"stages rendered identically to a clean one — same rows, same numbers, same closing " +
			"total: line — and the only place the truth existed was pipeline_timing.json.",
	},
	{
		Name:     "vamp/a run_when-gated stage is reported as a failure",
		File:     "internal/vamp/timing.go",
		Find:     "\tcase stageStatusOK, stageStatusSkipped, \"\":\n\t\treturn false",
		Replace:  "\tcase stageStatusOK, \"\":\n\t\treturn false",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestFormatTable_SkippedStageIsNotAFailure"},
		Why: "a `run_when: failure` notify stage is SKIPPED on every successful run. Counting skipped " +
			"as failed puts a FAILED line on every clean run of every pipeline that declares one, " +
			"which is how an operator learns to stop reading the line that exists to be read.",
	},
	{
		Name:     "vamp/the failure summary stops naming the way back",
		File:     "internal/vamp/failure_summary.go",
		Find:     "\tif cmd := resumeCommand(e); cmd != \"\" {",
		Replace:  "\tif cmd := \"\"; cmd != \"\" {",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestFailureSummary_PrintsTheResumeCommand"},
		Why: "--resume is fully built (per-item foreach granularity, snapshot-drift detection, JSON " +
			"revalidation) and for its whole life NO user-facing output mentioned it. A ten-stage " +
			"pipeline dying at stage nine gets re-run from stage one by an operator who had no way " +
			"to know there was another option.",
	},
	{
		Name:     "vamp/the invalid_output classifier stops recognising a bad TTS body",
		File:     "internal/vamp/exec.go",
		Find:     "\t\tif strings.Contains(msg, audioInvalidOutputTag) {\n\t\t\treturn true\n\t\t}",
		Replace:  "\t\tif false && strings.Contains(msg, audioInvalidOutputTag) {\n\t\t\treturn true\n\t\t}",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestAudioExecutor_KokoroEngine_EmptyBodyTaggedForRetry"},
		Why: "kokoro 200-OKs with an empty body during model warm-up — once in 1500+ chunks, and that " +
			"one chunk fails the whole foreach. The test that covers this asserted the error CONTAINS " +
			"the tag constant it was formatted with, which is a tautology: deleting this arm outright " +
			"left the entire suite green.",
	},
	{
		Name:     "vamp/the cache admits a zero-length artefact",
		File:     "internal/vamp/cache/cache.go",
		Find:     "\tif in.Bytes != nil && len(in.Bytes) == 0 {",
		Replace:  "\tif false && in.Bytes != nil && len(in.Bytes) == 0 {",
		Pkg:      "./internal/vamp/cache/",
		MustFail: []string{"TestStore_Put_RefusesAZeroLengthBinaryArtefact"},
		Why: "os.ReadFile of a 0-byte file returns an empty but NON-NIL slice, and Put's mode select " +
			"reads non-nil Bytes as 'binary output'. This is the rung that decides whether a " +
			"swallowed subprocess error is a bad run or a permanently poisoned content-addressed " +
			"entry.",
	},

	// ── class 4: a deadline that is present but never reached ─────────
	//
	// Every entry below was verified by hand in the U5 pass and is here
	// because the class is invisible to ordinary tests: this repo's whole
	// vocabulary for "unreachable" is an immediate ECONNREFUSED or a DNS
	// failure, both of which return in microseconds. A bound that was
	// deleted outright would not have failed a single test in the suite
	// before these — the far side always answered too fast to need it.
	{
		Name:     "u5/a zero warmTimeout seam becomes an already-expired context",
		File:     "internal/vibe/fleetapi/warmtarget.go",
		Find:     "func warmBound(d time.Duration) time.Duration {\n\tif d <= 0 {\n\t\treturn warmTimeout\n\t}\n\treturn d\n}",
		Replace:  "func warmBound(d time.Duration) time.Duration {\n\treturn d\n}",
		Pkg:      "./internal/vibe/fleetapi/",
		MustFail: []string{"TestU5_WarmRestoreDefaultsToTheProductionWarmTimeout"},
		Why: "the seam that lets a test dial the warm bound down is also the way to break every " +
			"production warm at once: context.WithTimeout(bg, 0) is already expired, so an unset " +
			"field would turn a 10-minute cold-start allowance into an instant deadline-exceeded " +
			"on every restore — and the piggyback queue would then carry the failure to the cell.",
	},
	// ── the external-input parsers, and the router classifier ─────────
	//
	// A review pass over internal/vamp/exec.go 3015–3495 and routererr.go
	// deleted fifteen guards in this region and NINE of them left the
	// package green. The four entries above (searxng/mediawiki) are the six
	// that went red; every entry below is a guard that either had no test
	// at all or, in one case, had a test that could not tell which of two
	// rungs was doing the work. These parsers have no in-repo caller — they
	// are reached only from templateFuncMap by user pipelines living outside
	// the repo — so nothing in CI will ever catch a regression here by
	// accident. The registry is the only thing standing under them.
	{
		Name:     "vamp/an-arxiv-api-error-feed-reads-as-a-source",
		File:     "internal/vamp/exec.go",
		Find:     "\t\tif strings.Contains(strings.ToLower(url), arxivAPIErrorMarker) {",
		Replace:  "\t\tif false && strings.Contains(strings.ToLower(url), arxivAPIErrorMarker) {",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestParseArxivTemplate_APIErrorFeedIsNotASource"},
		Why: "arXiv has NO error channel: a rejected request comes back as HTTP 200 carrying a valid " +
			"one-entry Atom feed whose id is an arxiv.org/api/errors# URL and whose title is \"Error\". " +
			"Without this rung a mistyped id_list produces a paper called Error whose abstract is the " +
			"API's own complaint, and the research stage cites it in the report. Both sibling parsers " +
			"carried this distinction and argued it in their doc comments; this one did not mention it.",
	},
	{
		Name:     "vamp/an-arxiv-feed-whose-entries-all-fail-to-parse-reads-as-zero-papers",
		File:     "internal/vamp/exec.go",
		Find:     "\tif len(feed.Entries) > 0 && len(out) == 0 {",
		Replace:  "\tif len(feed.Entries) < 0 && len(out) == 0 {",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestParseArxivTemplate_UnreadableEntriesAreNotZeroPapers"},
		Why: "the other half. Entries the parser cannot read were dropped one at a time, so an arXiv " +
			"namespace or schema change turned N papers into zero sources on a GREEN run — the exact " +
			"outcome parseSearXNG's nine-line comment exists to prevent, one function away.",
	},
	{
		Name:     "vamp/a-wikipedia-page-with-no-url-shares-every-other-pages-id",
		File:     "internal/vamp/exec.go",
		Find:     "\t\turl, _ := page[\"fullurl\"].(string)\n\t\tif url == \"\" {",
		Replace:  "\t\turl, _ := page[\"fullurl\"].(string)\n\t\tif url == \"\\x00\" {",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestParseWikipediaExtractTemplate_MissingFullurlIsAnError"},
		Why: "`fullurl` only exists when the query carries inprop=url, which this function's own doc " +
			"comment omitted. Without it every page hashes the empty string and every source gets the " +
			"id e3b0c44298fc — and the id's stated purpose is stable per-source FILENAMES, so three " +
			"sources write to one file, last write wins, two sources vanish, and no error is raised " +
			"anywhere. Both sibling parsers refuse the identical missing-url shape.",
	},
	{
		Name:     "vamp/the-missing-page-skip-is-absorbed-by-the-empty-extract-skip",
		File:     "internal/vamp/exec.go",
		Find:     "\t\tif pid, ok := page[\"pageid\"].(float64); ok && pid < 0 {",
		Replace:  "\t\tif pid, ok := page[\"pageid\"].(float64); ok && pid < -1000 {",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestParseWikipediaExtractTemplate_NotFound"},
		Why: "this rung was individually deletable with its own named test still green. MediaWiki's " +
			"missing-page entry has no `extract`, so the pageid check and the empty-extract check both " +
			"fire on the wire fixture and the test could not say which one was working. The fixture " +
			"that discriminates is the same page WITH an extract. Relaxing `extract == \"\"` is a " +
			"plausible future edit — exintro legitimately returns empty leads — and it must not " +
			"promote a nonexistent page to a source titled \"DoesNotExist\".",
	},
	{
		Name:     "vamp/multi-page-extract-output-goes-back-to-map-order",
		File:     "internal/vamp/exec.go",
		Find:     "\tsort.Strings(pageKeys)",
		Replace:  "\t_ = sort.Strings",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestParseWikipediaExtractTemplate_MultiPageOrderIsDeterministic"},
		Why: "titles=A|B|C is a legal MediaWiki query (up to 50 titles) and Go randomises map " +
			"iteration, so one 5-page response rendered five different prompts over 200 parses. " +
			"Nothing is reproducible and nothing keyed on the rendered prompt can cache. readFiles " +
			"sorts \"for determinism\" three hundred lines down; this is the same norm.",
	},
	{
		Name:     "vamp/searxng-results-that-all-fail-to-parse-read-as-zero-hits",
		File:     "internal/vamp/exec.go",
		Find:     "\t\tif len(results) > 0 && emitted == 0 {",
		Replace:  "\t\tif len(results) < 0 && emitted == 0 {",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestParseSearXNGTemplate_UnreadableResultsAreNotZeroHits"},
		Why: "the third rung on the existing searxng pair, and the sharpest: a body stating " +
			"number_of_results 12 whose items carry a RENAMED url field read as a clean zero, with " +
			"the disconfirming evidence sitting unused in the same map. The original guard was built " +
			"for the missing KEY and never extended to a key that is present and unreadable.",
	},
	{
		Name:     "vamp/a-non-array-searxng-results-field-reads-as-an-empty-search",
		File:     "internal/vamp/exec.go",
		Find:     "\t\t\tif rawResults == nil {",
		Replace:  "\t\t\tif rawResults == nil || true {",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestParseSearXNGTemplate_UnreadableResultsAreNotZeroHits"},
		Why: "the tolerant branch's own comment said \"null (or a scalar)\" while the code accepted " +
			"ANYTHING that was not an array — including an object holding twelve entries. Only null " +
			"is SearXNG's spelling of an empty result set; an object or a scalar there means something " +
			"other than SearXNG answered.",
	},
	{
		Name:     "vamp/a-wikipedia-title-goes-into-a-url-path-unescaped",
		File:     "internal/vamp/exec.go",
		Find:     "\tu := url.URL{Scheme: \"https\", Host: \"en.wikipedia.org\", Path: \"/wiki/\" + strings.ReplaceAll(title, \" \", \"_\")}\n\treturn u.String()",
		Replace:  "\treturn \"https://en.wikipedia.org/wiki/\" + strings.ReplaceAll(title, \" \", \"_\")",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestParseWikipediaSearchTemplate_TitlesArePathEscaped", "TestParseWikipediaSearchTemplate"},
		Why: "the title is upstream text landing in a URL PATH. Concatenated, \"C# (programming " +
			"language)\" yields a URL whose # opens a fragment that is never sent to the server, so " +
			"the citation in the report resolves to the article \"C\" — silently, with the sha256 id " +
			"computed over the broken URL. url.URL is the right tool and url.PathEscape is not: a " +
			"subpage \"/\" and a namespace \":\" have to survive.",
	},
	{
		Name:     "vamp/the-search-snippet-tag-stripper-eats-prose-again",
		File:     "internal/vamp/exec.go",
		Find:     "var wikiSearchTagRE = regexp.MustCompile(`</?[a-zA-Z][^>]*>`)",
		Replace:  "var wikiSearchTagRE = regexp.MustCompile(`<[^>]*>`)",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestParseWikipediaSearchTemplate_SnippetKeepsProseAndDecodesEntities"},
		Why: "a bare `<` opener read the prose BETWEEN two comparison operators as a tag and deleted " +
			"it: \"for all n where 0 < n and n > 5\" reached the model as \"0  5\". The snippet then " +
			"asserts something the source did not say, and nothing anywhere reports a problem. " +
			"Inequalities, generics and code fragments are ordinary content in a search snippet.",
	},
	{
		Name:     "vamp/mediawiki-search-snippets-stop-being-entity-decoded",
		File:     "internal/vamp/exec.go",
		Find:     "\t\tsnippet = html.UnescapeString(strings.TrimSpace(wikiSearchTagRE.ReplaceAllString(snippet, \"\")))",
		Replace:  "\t\tsnippet = html.UnescapeString(\"\") + strings.TrimSpace(wikiSearchTagRE.ReplaceAllString(snippet, \"\"))",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestParseWikipediaSearchTemplate_SnippetKeepsProseAndDecodesEntities"},
		Why: "the other half of the same half-done sanitiser: tags out, entities in. The model read " +
			"\"5 &deg;C\" and \"&amp;\" literally. The order is load-bearing and the mutation keeps " +
			"the import alive so the entry cannot pass by failing to compile: decoding BEFORE " +
			"stripping would turn a `&lt;b&gt;` the source wrote into a tag the stripper then eats.",
	},
	{
		Name:     "vamp/a-non-positive-truncate-limit-means-no-cap-again",
		File:     "internal/vamp/exec.go",
		Find:     "\tif n <= 0 {\n\t\treturn \"\", fmt.Errorf(\"truncate: limit must be positive, got %d (a non-positive limit would pass the whole %d-byte input through uncapped)\", n, len(s))\n\t}",
		Replace:  "\tif n <= 0 {\n\t\treturn s, nil\n\t}",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestTruncateTemplate_NonPositiveLimitIsAnError"},
		Why: "truncate is the one guard between an oversized document and the context window, and " +
			"`n <= 0` used to mean \"no cap\" — so a typo'd `truncate 0`, or a remaining-budget " +
			"expression that went non-positive, passed a 100KB document straight through and reported " +
			"success. A guard that disarms itself on a typo is the shape this package keeps paying " +
			"for; splitSentences answers the same question with a default rather than a bypass.",
	},
	{
		Name:     "vamp/stripDataURIs-becomes-a-no-op",
		File:     "internal/vamp/exec.go",
		Find:     "func stripDataURIsTemplate(s string) string {",
		Replace:  "func stripDataURIsTemplate(s string) string {\n\tif true {\n\t\treturn s\n\t}",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestStripDataURIsTemplate", "TestStripDataURIsTemplate_MeetsItsSizeGoal"},
		Why: "deleting this helper's whole body left the package green — it had no test at all, while " +
			"being the guard between a 30-reference lesson (10KB per reference) and a blown context " +
			"window. Measured on that document before the fix: 0.0% reduction on an uppercase DATA: " +
			"scheme, and 0.6% plus a CORRUPTED document when an un-encoded SVG body contained rgb(…), " +
			"where the payload survived as prose and the helper looked like it had worked.",
	},
	{
		Name:     "vamp/mediawiki-knows-one-of-its-three-error-shapes-again",
		File:     "internal/vamp/exec.go",
		Find:     "\tlist, ok := resp[\"errors\"].([]any)",
		Replace:  "\tlist, ok := resp[\"errors-that-are-never-there\"].([]any)",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestMediawikiErrorSuffix"},
		Why: "{\"errors\":[{\"code\":…,\"text\":…}]} is what MediaWiki returns whenever the request " +
			"carries errorformat= — the MODERN form. Knowing only the legacy {\"error\":{code,info}} " +
			"shape meant a real refusal produced \"response has no query object\" and no reason at " +
			"all, which is the exact outcome this helper exists to prevent. Cost is operator minutes, " +
			"paid every time.",
	},
	{
		Name:     "router/an-oom-substring-turns-a-name-error-into-a-retry-loop",
		File:     "internal/vamp/routererr.go",
		Find:     "\tcase containsAny(lower, capacityPhrases...) || containsToken(lower, \"oom\"):",
		Replace:  "\tcase containsAny(lower, capacityPhrases...) || strings.Contains(lower, \"oom\"):",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestClassifyFailureMessage_OOMIsMatchedAsAWord", "TestClassifyHTTPFailure_NameErrorAndCapacityDoNotSwap"},
		Why: "\"oom\" as a bare substring is inside bloom, bloomz, zoom, doom, room and bedroom — real " +
			"GGUF families and ordinary path segments — and the OOM arm is tested BEFORE the NOT_FOUND " +
			"arm, so it wins. A 404 saying `model not found: bloomz-7b1` classified as CAPACITY, which " +
			"WaitForWarm retries: the operator who typo'd one catalog entry hammered a 404 every 3s " +
			"for the full 10-minute warm budget and was then told the model did not fit in VRAM. The " +
			"same 404 for qwen3-30b failed in milliseconds.",
	},
	{
		Name:     "router/an-allocation-failure-outside-the-phrase-list-permanently-fails-a-capability",
		File:     "internal/vamp/routererr.go",
		Find:     "\t\"insufficient vram\", \"allocate\", \"allocation failed\",\n\t\"exit status 137\", \"signal: killed\",",
		Replace:  "\t\"insufficient vram\", \"failed to allocate\",",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestClassifyFailureMessage_AllocationFailuresAreRetryable"},
		Why: "the same three lines in the opposite direction, and the more expensive one. The list " +
			"required the exact phrase \"failed to allocate\", so \"unable to allocate CUDA0 buffer\" " +
			"and \"exit status 137\" (the Linux OOM-killer's signature) fell through to START_FAILED — " +
			"which WaitForWarm treats as an AUTHORITATIVE verdict and does not retry. A transient VRAM " +
			"squeeze, another cell's model still resident, is the normal state of a two-GPU fleet, and " +
			"it permanently failed the capability instead of succeeding three seconds later.",
	},
	{
		Name:     "router/an-empty-error-field-becomes-a-reasonless-non-retryable-failure",
		File:     "internal/vamp/routererr.go",
		Find:     "\t\tif strings.TrimSpace(s) == \"\" {\n\t\t\treturn \"\", false\n\t\t}",
		Replace:  "\t\tif strings.TrimSpace(s) == \"\\x00\" {\n\t\t\treturn \"\", false\n\t\t}",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestRouterFailureMessage_AnEmptyErrorFieldIsNotAMessage"},
		Why: "routerFailureMessage has two callers and only classifyHTTPFailure checked msg != \"\". " +
			"readWarmStream handed it straight through, so `{\"error\":\"\"}` mid-stream produced " +
			"`router: START_FAILED: model \"qwen3\"` and nothing else — and START_FAILED is " +
			"non-retryable, so the whole capability died with an error containing no reason to act on. " +
			"One helper, guarded on one of its two call paths.",
	},
	{
		Name:     "router/the-classifier-loses-its-own-bound-on-far-side-text",
		File:     "internal/vamp/routererr.go",
		Find:     "\tmsg = boundDetail(msg)",
		Replace:  "\t_ = boundDetail",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestClassifyHTTPFailure_DetailIsBounded"},
		Why: "the 8192-byte io.LimitReader was in WarmModel, the CALLER, so the classifier was a guard " +
			"in one of N call paths with N=1. Whatever the far side says lands verbatim in an error " +
			"warmCapability writes to the run log, and the moment a cloud_api backend joins the warm " +
			"path — which the fleet design plans — a caller that forgets the LimitReader hands this an " +
			"unbounded body. A 7KB HTML proxy page is not a diagnostic.",
	},
	{
		Name:     "router/a-connection-level-errno-stops-answering-is-anything-listening",
		File:     "internal/vamp/routererr.go",
		Find:     "\t\terrors.Is(err, syscall.EPIPE) ||\n\t\terrors.Is(err, syscall.ECONNABORTED) ||\n\t\terrors.Is(err, syscall.ETIMEDOUT) {",
		Replace:  "\t\tfalse {",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestIsConnectFailure_ConnectionLevelErrnos"},
		Why: "vamp/errors.go publishes errors.Is(err, ErrUpstreamDown) as THE documented way for an " +
			"external caller to ask \"is anything listening on the router port\". A router killed " +
			"mid-request answers with OpError{Op:\"write\", Err: EPIPE}, which the dial-only Op check " +
			"never saw, so the published API returned the wrong answer to the one question it exists " +
			"to answer.",
	},
	{
		Name:     "u5/a zero suspendTimeout seam becomes an already-expired context",
		File:     "internal/vibe/fleetapi/sleepsched.go",
		Find:     "func suspendBound(d time.Duration) time.Duration {\n\tif d <= 0 {\n\t\treturn suspendTimeout\n\t}\n\treturn d\n}",
		Replace:  "func suspendBound(d time.Duration) time.Duration {\n\treturn d\n}",
		Pkg:      "./internal/vibe/fleetapi/",
		MustFail: []string{"TestU5_ScheduledSuspendDefaultsToTheProductionSuspendTimeout"},
		Why: "same shape as the warm seam, on the verb that takes a box off the fleet. An expired " +
			"context makes every scheduled suspend report `failed` in microseconds, every night, " +
			"while the box keeps drawing its idle watts and the audit stays green.",
	},
	{
		Name:     "u5/a wedged suspend RPC stops dying with the server",
		File:     "internal/vibe/fleetapi/sleepsched.go",
		Find:     "ctx, cancel := s.warmCtx(suspendBound(cfg.suspendTimeout))",
		Replace:  "ctx, cancel := context.WithCancel(context.Background())",
		Pkg:      "./internal/vibe/fleetapi/",
		MustFail: []string{"TestU5_AWedgedSuspendDoesNotHoldClose"},
		Why: "warmCtx is what links a bound to s.done. Unlinked, a fleetd asked to shut down while " +
			"a suspend RPC is wedged waits the full 90s on wg.Wait() — on the one goroutine holding " +
			"a box's power state open. CC-2 fixed exactly this for the warm and the suspend was built later.",
	},
	{
		Name:     "u5/the sleep return grace shrinks inside the stale window",
		File:     "internal/vibe/fleetapi/sleepsched.go",
		Find:     "const sleepReturnGrace = 2 * time.Minute",
		Replace:  "const sleepReturnGrace = 30 * time.Second",
		Pkg:      "./internal/vibe/fleetapi/",
		MustFail: []string{"TestU5_TheReturnGraceOutlastsTheWindowWhereADeadBoxStillReadsFresh"},
		Why: "cellPresent reads an announce as present until staleAfter(interval) — 50s at the " +
			"default cadence — so a box suspended at T still reads PRESENT for the next fifty " +
			"seconds. A grace inside that window makes the entry forget it suspended the box, on " +
			"the evidence of a heartbeat that predates the suspend. The relation to staleAfter is " +
			"what makes this a guard rather than a literal: the two other grace tests name the " +
			"constant and would stay green at two seconds.",
	},
	{
		Name:     "u5/an absent cell is reconciled to awake",
		File:     "internal/vibe/fleetapi/sleepsched.go",
		Find:     "if st.asleep && present && !hasSleep && time.Since(st.asleepSince) > sleepReturnGrace {",
		Replace:  "if st.asleep && !hasSleep && time.Since(st.asleepSince) > sleepReturnGrace {",
		Pkg:      "./internal/vibe/fleetapi/",
		MustFail: []string{"TestU5_TheReturnGraceNeedsBothHalvesOfTheEvidence"},
		Why: "class 1 again, in the sleep half: the grace expiring is necessary, never sufficient. " +
			"Without the presence conjunct a box that is still asleep — which is the whole point — " +
			"has its record cleared two minutes in, and fleet_status then reports a sleeping box as " +
			"watching. Absence of evidence became evidence of return.",
	},
	{
		Name:     "u5/the warm schedule's warm loses its deadline",
		File:     "internal/vibe/fleetapi/warmsched.go",
		Find:     "ctx, cancel := s.warmCtx(warmTimeout)",
		Replace:  "ctx, cancel := context.WithCancel(context.Background())",
		Pkg:      "./internal/vibe/fleetapi/",
		MustFail: []string{"TestU5_WarmScheduleCarriesTheSameWarmTimeout"},
		Why: "class 3: warmTimeout has THREE consumers (the warm target, the warm schedule, the " +
			"post-wake warms) and the seam added in U5 only reaches the first. The other two are " +
			"pinned by reading the deadline back out of the context they hand their warm, which is " +
			"the only assertion available without waiting ten minutes.",
	},
	{
		Name:     "u5/the post-wake warms lose the warm half of their bound",
		File:     "internal/vibe/fleetapi/sleepsched.go",
		Find:     "ctx, cancel := s.warmCtx(e.WakeGrace + warmTimeout)",
		Replace:  "ctx, cancel := s.warmCtx(e.WakeGrace)",
		Pkg:      "./internal/vibe/fleetapi/",
		MustFail: []string{"TestU5_PostWakeWarmsCarryTheGraceAndTheWarmTimeout"},
		Why: "the wake sequence builds ONE context for the whole run, and the warms sit at the far " +
			"end of it. Bounded by the grace alone, every 07:15 warm starts with a context that the " +
			"return wait has already consumed — the warms fail instantly and get queued to the cell " +
			"as if the front had refused them.",
	},
	{
		Name:     "u5/the wake fallback command stops honouring its caller's context",
		File:     "internal/vibe/fleetapi/wake.go",
		Find:     "ctx, cancel := context.WithTimeout(ctx, wakeCmdTimeout)",
		Replace:  "ctx, cancel := context.WithTimeout(context.Background(), wakeCmdTimeout)",
		Pkg:      "./internal/vibe/fleetapi/",
		MustFail: []string{"TestU5_TheWakeFallbackCommandDiesWithItsCallersContext"},
		Why: "the sleep schedule calls SendWake with a warmCtx, so the fallback command is how a " +
			"wake reaches Close(). Rooted at Background it is bounded only by its own 30s timeout, " +
			"and a shutdown during the morning wake waits half a minute per wedged cell. The " +
			"one-character version of this bug is invisible to every other test in the package.",
	},
	{
		Name: "shellcmd/the kill stops reaching what the shell forked",
		File: "internal/vibe/shellcmd/shellcmd.go",
		// One character, not the whole block. Deleting Setpgid while
		// leaving the negative-pid Cancel behind would signal the TEST
		// BINARY's process group — a mutation that takes the harness down
		// rather than one that proves anything. Dropping the minus keeps
		// the group and removes the reach, which is the defect exactly.
		Find:     "syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)",
		Replace:  "syscall.Kill(cmd.Process.Pid, syscall.SIGKILL)",
		Pkg:      "./internal/vibe/shellcmd/",
		MustFail: []string{"TestNew_TheKillReachesWhatTheShellForked"},
		Why: "the deadline that fires but does not return. exec.CommandContext kills the process it " +
			"STARTED, and /bin/sh is dash on the fleet's boxes — dash FORKS the operator's ipmitool " +
			"rather than exec'ing into it, so the kill lands on the shell while the grandchild keeps " +
			"the stdout pipe open and Wait blocks on the copy until the far side answers on its own. " +
			"This shipped TWICE, in the wake command (U5) and the drain verb (U3): both times the " +
			"tests meant to prove the deadline went green on a dev box where /bin/sh is bash (which " +
			"execs, so the bug is invisible) and red on CI at exactly the command's own runtime.",
	},
	{
		Name:     "shellcmd/the Wait outlives the kill it could not deliver",
		File:     "internal/vibe/shellcmd/shellcmd.go",
		Find:     "\tcmd.WaitDelay = killGrace\n",
		Replace:  "",
		Pkg:      "./internal/vibe/shellcmd/",
		MustFail: []string{"TestNew_TheWaitIsBoundedEvenWhenTheKillCannotLand", "TestNew_WiresBothHalvesOfTheDeath"},
		Why: "the backstop for the descendant SIGKILL cannot reach: one that setsid'd out of the " +
			"group, or one wedged in uninterruptible I/O. A zero WaitDelay is documented to mean Wait " +
			"blocks INDEFINITELY on the I/O pipes, so that process pins the caller for as long as it " +
			"lives. U5 could only pin this by wiring assertion; U3 found that setsid(1) drives a " +
			"process out of its own group in one word, so the behavioural half exists now — and both " +
			"are named, because setsid is absent on macOS and a test that SKIPS is a test that did " +
			"not run.",
	},
	{
		Name: "u5/the wake command stops sharing the builder",
		File: "internal/vibe/fleetapi/wake.go",
		Find: "\treturn shellcmd.New(ctx, script, wakeCmdKillGrace)",
		// Disarmed at the call site rather than swapped for a bare
		// exec.CommandContext: dropping the only shellcmd reference leaves
		// the import unused and the tree does not build, and a mutation
		// that does not COMPILE proves nothing. (Tried it; the harness
		// said so.) The effect is the same — this site stops getting what
		// the shared builder gives every other one.
		Replace: "\tc := shellcmd.New(ctx, script, wakeCmdKillGrace)\n" +
			"\tc.SysProcAttr, c.Cancel, c.WaitDelay = nil, nil, 0\n" +
			"\treturn c",
		Pkg:      "./internal/vibe/fleetapi/",
		MustFail: []string{"TestU5_TheWakeCommandKillsWhatTheShellForked", "TestU5_TheWakeCommandBoundsTheWaitItCannotKill"},
		Why: "the shape a wake command had before U5, which is also the shape the drain verb still " +
			"had after it. A guard on one of N call paths is this repo's most recurring defect, so " +
			"each site's membership is pinned separately from the builder's own correctness — that " +
			"is the whole reason the builder is shared rather than copied.",
	},
	{
		Name:     "u5/the CLI's degraded wake stops sharing fleetd's runner",
		File:     "internal/vibe/cli/cmd_cell_actuate.go",
		Find:     "fleetapi.RunWakeCmd(sigCtx, c.Wake.Cmd)",
		Replace:  "exec.CommandContext(sigCtx, \"sh\", \"-c\", c.Wake.Cmd).CombinedOutput()",
		Pkg:      "./internal/vibe/cli/",
		MustFail: []string{"TestWakeCellDegradedPathBoundsTheOperatorsCommand"},
		Why: "the second of the two call paths that run an operator's wake.cmd, and the shape the " +
			"first one had before U5: no deadline of its own and a cancellation that kills the shell " +
			"and then waits on the ssh the shell forked. `vibe cell wake` is the path an operator " +
			"reaches for when fleetd is DOWN, which is exactly when it must not hang. A guard on one " +
			"of N call paths is this repo's most recurring defect, so the sharing is pinned rather " +
			"than assumed.",
	},
	{
		Name:     "u5/the warm target's probe forks the snapshot budget in two",
		File:     "internal/vibe/fleetapi/warmtarget.go",
		Find:     "ctx, cancel := s.warmCtx(s.snapTimeout)",
		Replace:  "ctx, cancel := s.warmCtx(snapshotTimeout)",
		Pkg:      "./internal/vibe/fleetapi/",
		MustFail: []string{"TestU5_TheWarmTargetProbeRunsOnTheServersSnapshotBudget"},
		Why: "one quantity, two sources of truth. The warm loop's fallback probe is the same round " +
			"the state handler runs, and U1 made that budget a field; this call site kept naming the " +
			"constant, so a fleetd that tuned the budget moved every probe in the round except this " +
			"one, which silently kept the compiled-in 3s against a cell that accepts and never " +
			"answers. A duplicated budget is only ever discovered by the half that did not move.",
	},

	// ── class 4, met again in the daemon's two shell call sites ───────
	//
	// The same defect as u5's wake command, in the other two places this
	// repo hands an operator's shell string to exec: `cell_cmds.drain`
	// and a profile's lifecycle hooks. Found independently, from the same
	// symptom (CI red at exactly the command's own runtime, green on a
	// workstation where /bin/sh is bash) — which is the argument both for
	// the builder being SHARED and for each site's membership being
	// pinned separately from the builder's own correctness.
	{
		Name: "u3/the drain verb's budget stops reaching its command",
		File: "internal/vibe/daemon/cell_drain.go",
		Find: "\tc := shellcmd.New(ctx, cmd, cellCmdWaitDelay)",
		// Disarmed AT the call site rather than swapped for a bare
		// exec.CommandContext, which would need an import this file no
		// longer has — and a mutation that fails to COMPILE proves
		// nothing. The effect is the same: this one site stops getting
		// what the shared builder gives every other one.
		Replace: "\tc := shellcmd.New(ctx, cmd, cellCmdWaitDelay)\n" +
			"\tc.SysProcAttr, c.Cancel, c.WaitDelay = nil, nil, 0",
		Pkg:      "./internal/vibe/daemon/",
		MustFail: []string{"TestCellDrain_HungVerbSurfacesUnavailableAtTheBudget"},
		Why: "the drain verb is where this wave met the defect: the budget fired on schedule, the " +
			"error said `signal: killed`, and the call still took 30.003s against a 400ms bound " +
			"because the sleep the shell forked still held the stderr pipe. A cell whose reclaim is " +
			"half-run while the RPC has already answered Unavailable is indistinguishable, on the " +
			"wire, from one where the bound worked.",
	},
	{
		Name: "u3/a lifecycle hook stops sharing the builder",
		File: "internal/vibe/daemon/daemon.go",
		Find: "\t\tc := shellcmd.New(ctx, cmd, hookWaitDelay)",
		Replace: "\t\tc := shellcmd.New(ctx, cmd, hookWaitDelay)\n" +
			"\t\tc.SysProcAttr, c.Cancel, c.WaitDelay = nil, nil, 0",
		Pkg:      "./internal/vibe/daemon/",
		MustFail: []string{"TestRunHooks_ACancelledPhaseDoesNotWaitOnWhatTheHookForked"},
		Why: "the third operator-shell site, and the one with NO deadline of its own — hooks run " +
			"under the RPC's context. That made the missing reach worse here rather than better: " +
			"there was no bound to be inert, so a cancelled `vibe start` killed the hook's shell and " +
			"then waited on whatever the hook had forked, with nothing anywhere to end it. This site " +
			"was found by grepping for the shape once U5 gave it a name, which is the argument for " +
			"the shape having one.",
	},

	// ── class 4, the fourth site — and the seam in front of it ────────
	//
	// fleetannounce's desired-intent verb was the last `sh -c` in the repo
	// still building its own command. It was deferred out of U3 because of
	// its test seam: every test in that package replaces execSh, so moving
	// the production path onto shellcmd without touching the seam would
	// have produced a bound that no test in the package ever executes.
	// Both halves are pinned — the call site's membership, and the seam's
	// default — because either alone leaves the verb unbounded.
	{
		Name: "c26a/the desired-intent verb stops sharing the builder",
		File: "internal/vibe/fleetannounce/fleetannounce.go",
		Find: "\treturn shellcmd.New(ctx, cmd, verbKillGrace)",
		// Disarmed at the call site rather than swapped for a bare
		// exec.CommandContext: this file needs os/exec for the *exec.Cmd
		// return type either way, but the disarm is the honest shape of the
		// regression — the site stops getting what the shared builder gives
		// every other one.
		Replace: "\tc := shellcmd.New(ctx, cmd, verbKillGrace)\n" +
			"\tc.SysProcAttr, c.Cancel, c.WaitDelay = nil, nil, 0\n" +
			"\treturn c",
		Pkg: "./internal/vibe/fleetannounce/",
		MustFail: []string{
			"TestVerbSeam_ProductionDefaultIsTheBoundedRunner",
			"TestDesiredIntentVerb_TheBudgetReachesWhatTheShellForked",
			"TestDesiredIntentVerb_TheWaitIsBoundedEvenWhenTheKillCannotLand",
		},
		Why: "the fourth operator-shell site, reached from the announce loop rather than an RPC. A " +
			"drain arriving as a desired_intent runs the operator's `systemctl stop` here; with the " +
			"kill landing on `sh` alone, the 60s budget fires on schedule, the cell logs `signal: " +
			"killed`, and the loop does not return until the reclaim finishes on its own — while " +
			"fleetd is told the drain failed and hands the request back on the next heartbeat.",
	},
	{
		Name: "c26a/the verb seam's default drifts off the builder",
		File: "internal/vibe/fleetannounce/fleetannounce.go",
		Find: "var execSh = runShellVerb",
		Replace: "var execSh = func(ctx context.Context, cmd string) (string, error) {\n" +
			"\tc := verbCmd(ctx, cmd)\n" +
			"\tc.SysProcAttr, c.Cancel, c.WaitDelay = nil, nil, 0\n" +
			"\tout, err := c.CombinedOutput()\n" +
			"\treturn strings.TrimSpace(string(out)), err\n" +
			"}",
		Pkg: "./internal/vibe/fleetannounce/",
		MustFail: []string{
			"TestVerbSeam_ProductionDefaultIsTheBoundedRunner",
			"TestDesiredIntentVerb_TheBudgetReachesWhatTheShellForked",
			"TestDesiredIntentVerb_TheWaitIsBoundedEvenWhenTheKillCannotLand",
		},
		Why: "the reason this fix was deferred rather than typed. Every test in fleetannounce assigns " +
			"over execSh, so a default that quietly stops going through shellcmd leaves all of them " +
			"green while the cell's verbs go unbounded again. A seam that tests replace with something " +
			"skipping the bounds is a seam that guarantees the bounds are never tested; the three " +
			"tests named here are the ones that run the UNREPLACED default.",
	},

	// ── class 5: a check that fires for a box it does not describe ────
	{
		Name: "c26a/doctor fails a box for a binary nothing on it declares",
		File: "internal/vibe/cli/cmd_doctor.go",
		Find: "\tconst name = \"llama-server\"\n\tif len(users) == 0 {\n\t\treturn checkResult{}, false\n\t}\n",
		// Back to asking $PATH and nothing else, which is the shape that
		// shipped.
		Replace:  "\tconst name = \"llama-server\"\n",
		Pkg:      "./internal/vibe/cli/",
		MustFail: []string{"TestCheckLlamaBinary_NothingDeclaresItIsNotApplicable"},
		Why: "a laptop whose only profiles are cloud_peer — pointed at a peer through a remote front, " +
			"which PR #14 made a supported configuration — exited non-zero from `vibe doctor` over a " +
			"binary it will never invoke. Same for comfyui-only and mlx-only boxes. A doctor that " +
			"fails for a reason that does not apply is a doctor operators learn to ignore, which " +
			"costs every OTHER check on the box.",
	},
	{
		Name:     "c26a/the declaration scan stops seeing backend defs",
		File:     "internal/vibe/cli/cmd_doctor.go",
		Find:     "\tscan(backendsDir, \"backend \")\n",
		Replace:  "",
		Pkg:      "./internal/vibe/cli/",
		MustFail: []string{"TestLlamaServerUsers_TheFourShapesOnDisk"},
		Why: "the applicable set has two sources, and a fleet box's llama_server declarations live in " +
			"backends/, not profiles/. Scanning only profiles/ makes the check not-applicable on " +
			"exactly the boxes that spawn llama-server most — the not-applicable path is the one that " +
			"has to be hardest to reach by accident, because its failure is silence.",
	},

	// ── class 6: two entries, one client-facing id ────────────────────
	{
		Name: "c26a/a cloud peer's model ids stop being canonical",
		File: "internal/vibe/router/render.go",
		Find: "\t\t\tfor _, id := range cp.Models {\n" +
			"\t\t\t\tif _, taken := peerIDs[id]; !taken {\n" +
			"\t\t\t\t\tpeerIDs[id] = d.Name\n" +
			"\t\t\t\t}\n" +
			"\t\t\t}\n",
		Replace: "",
		Pkg:     "./internal/vibe/router/",
		MustFail: []string{
			"TestResolveAliases_APeerModelIDIsCanonicalAndCannotBeAliased",
			"TestRender_TheAliasPeerCollisionSurfacesFromRender",
		},
		Why: "PR #14's rule met a second time: a cloud peer's catalog ids are its cloud_peer.models " +
			"entries, never its def name, so code keyed by def.Name works for every other backend " +
			"kind and silently misses for peers. Without the reservation a llama_server def whose " +
			"alias equals a peer's model id resolves cleanly and renders two entries under one id, " +
			"and llama-swap serves whichever wins.",
	},
	{
		Name:     "c26a/the rendered catalog stops being checked for duplicate ids",
		File:     "internal/vibe/router/render.go",
		Find:     "\tif err := checkCatalogIDsUnique(cfg); err != nil {\n\t\treturn \"\", err\n\t}\n\n",
		Replace:  "",
		Pkg:      "./internal/vibe/router/",
		MustFail: []string{"TestRender_NoCatalogIDIsAdvertisedTwice"},
		Why: "the alias reservation cannot be the whole rule: it sees DEFS, so it says nothing about a " +
			"llama_server def NAMED after a peer's model id (no alias involved) or two peers listing " +
			"the same id. Both put two entries in one catalog under one id. Checking the rendered " +
			"config instead of the defs is what makes this cover the class rather than the two paths " +
			"into it that are known today.",
	},
	{
		Name:     "c26a/the catalog check runs before the extras merge again",
		File:     "internal/vibe/router/render.go",
		Find:     "\t\tif err := checkCatalogIDsUnique(mergedCfg); err != nil {",
		Replace:  "\t\tif err := checkCatalogIDsUnique(cfg); err != nil {",
		Pkg:      "./internal/vibe/router/",
		MustFail: []string{"TestRender_NoCatalogIDIsAdvertisedTwice"},
		Why: "the entry above pinned the check and NOT where it runs, and the check shipped upstream " +
			"of the extras merge — so the namespace invariant held everywhere except the front, which " +
			"is the one host that always renders with extras (fleet.front_extras carries its apiKeys) " +
			"and the one config the whole fleet dials. mergeExtras guards the map KEYS it merges and " +
			"nothing inside them, so an extras alias or a model id under an extras peer went into the " +
			"catalog unexamined: three entries advertising one id, exit 0. This mutation is the shipped " +
			"defect exactly — check the pre-merge structure — and it must stay red.",
	},

	// ── class 7: a starter template the loader then refuses ───────────
	{
		Name: "c26a/the cloud-peer starter stops being loadable",
		File: "internal/vibe/cli/profile_templates/cloud-peer.yaml",
		Find: "    models:\n      - REPLACE-model-id\n",
		// A peer with no ids is a peer that serves nothing; validateCloudPeer
		// refuses it, which is the whole point — a template `vibe profile
		// new` can emit and `vibe start` then rejects is a broken command.
		Replace:  "    models: []\n",
		Pkg:      "./internal/vibe/cli/",
		MustFail: []string{"TestProfileInit_Kinds"},
		Why: "`vibe profile new --kind cloud-peer` had no template at all before C26a, so the one " +
			"shape PR #14 legalised was the one the CLI could not generate. The guard that matters " +
			"for a template is not that it exists but that what the command drops actually LOADS: a " +
			"starter that fails validation sends a first-time operator to debug their own typing for " +
			"a file they did not write.",
	},

	// ── class 5: private traffic reaching a place it may not (C25) ────
	//
	// C25 replays a cell's own recent requests through a candidate model,
	// and the bytes it handles are the verbatim prompts and completions of
	// real requests. The failure mode is not a wrong number; it is a
	// prompt in a public git repository or on somebody's terminal.
	{
		Name:     "c25/the fixture recorder stops refusing the captures endpoint",
		File:     "internal/swaptest/captures.go",
		Find:     "\tif !strings.Contains(strings.ToLower(p), CaptureRoutePrefix) {",
		Replace:  "\tif true || !strings.Contains(strings.ToLower(p), CaptureRoutePrefix) {",
		Pkg:      "./internal/swaptest/",
		MustFail: []string{"TestRefuseCaptureEndpoint_RefusesEveryFormOfTheRoute"},
		Why: "internal/swaptest's recorder reads a REAL llama-swap on the operator's box and commits " +
			"what it reads to a PUBLIC repository. GET /api/captures/{id} answers the verbatim prompt, " +
			"system prompt, tool definitions and completion of a real request, and the recorder's two " +
			"existing redactions cannot help: they work because the sensitive part is a substring of " +
			"something still worth keeping, and a capture's sensitive part is the whole object. " +
			"Without this refusal the next fixture commit publishes somebody's prompt.",
	},
	{
		Name:     "c25/the recorder's fetch guard moves off the one fetch path",
		File:     "internal/swaptest/record_test.go",
		Find:     "\tif why := RefuseCaptureEndpoint(url); why != \"\" {\n\t\tt.Fatal(why)\n\t}\n\treq, err := http.NewRequestWithContext(ctx, method, url, body)",
		Replace:  "\treq, err := http.NewRequestWithContext(ctx, method, url, body)",
		Pkg:      "./internal/swaptest/",
		MustFail: []string{"TestRecorderFetchesOnlyThroughTheCaptureRefusal"},
		Why: "the refusal exists and is not called. recordGET is the recorder's ONE fetch path, so a " +
			"guard removed from it is a guard removed from everything — and the AST scan is what " +
			"notices, because a reviewer reading the endpoint LIST would see nothing wrong.",
	},
	{
		Name:     "c25/the replay sample is harvested after the apply",
		File:     "internal/vibe/modeltry/modeltry.go",
		Find:     "\treturn benchreplay.Harvest(ctx, opt, t.Incumbent, t.State == StateApplied || t.State == StateMeasured)",
		Replace:  "\treturn benchreplay.Harvest(ctx, opt, t.Incumbent, false)",
		Pkg:      "./internal/vibe/modeltry/",
		MustFail: []string{"TestHarvestIsRefusedOnceTheTrialIsApplied"},
		Why: "the apply writes the cell's llama-swap config, and `-watch-config` does not mutate a " +
			"running server — it builds a new one, with a fresh EMPTY capture buffer. So at the moment " +
			"the candidate first exists the sample is gone, and a harvest that ran anyway would score " +
			"a model against no evidence. The ordering constraint and the privacy invariant are the " +
			"same constraint: the only place a sample can live across an apply is this process's memory.",
	},
	{
		Name:     "c25/an undeclared tool name is echoed instead of marked",
		File:     "internal/vibe/benchreplay/shape.go",
		Find:     "\t\ts.toolOutcome = ToolUndeclared\n\t\tif f.toolNames[tc.Function.Name] {\n\t\t\ts.toolOutcome = ToolDeclared\n\t\t}",
		Replace:  "\t\ts.toolOutcome = tc.Function.Name",
		Pkg:      "./internal/vibe/benchreplay/",
		MustFail: []string{"TestUndeclaredToolNameIsReportedAndNeverEchoed", "TestClosedSetStringsAreActuallyClosed"},
		Why: "a tool name a model invented is MODEL OUTPUT, which is to say private traffic, and the " +
			"per-request table is a thing an operator pastes into a chat window. The closed set " +
			"none/declared/<undeclared> is what keeps the field from being a channel for it, and it " +
			"is also what makes Report's string fields checkable at all.",
	},
	{
		Name:     "c25/the divergence claim stops being gated on the noise floor",
		File:     "internal/vibe/benchreplay/score.go",
		Find:     "\tif got <= noise {",
		Replace:  "\tif false {",
		Pkg:      "./internal/vibe/benchreplay/",
		MustFail: []string{"TestNoiseFloorSuppressesTheDivergenceClaim"},
		Why: "a captured request carries the CLIENT's own temperature, so replaying it through the " +
			"very model that produced the recorded response yields different text: the divergence " +
			"floor is not zero and is not knowable in advance. Reporting a candidate's raw " +
			"disagreement rate as divergence therefore reports the sampler, and it does it in the " +
			"direction that makes every candidate look worse than the incumbent.",
	},
	{
		Name:     "c25/a proportion is printed below the rate floor",
		File:     "internal/vibe/benchreplay/score.go",
		Find:     "\tif sc.Requests >= floor {",
		Replace:  "\tif true {",
		Pkg:      "./internal/vibe/benchreplay/",
		MustFail: []string{"TestBelowTheFloorThereIsATableAndNoRate"},
		Why: "n is whatever survived a 10 MB FIFO buffer, not something the operator chose, and a " +
			"tool-call rate on three requests is noise wearing a percent sign. The rates are " +
			"observed.Values so that under the floor they read `unknown` rather than a number — the " +
			"absent-evidence-as-a-healthy-value class, in the one screen this phase exists to print.",
	},
	{
		Name:     "c25/the paired ratio becomes a ratio of means",
		File:     "internal/vibe/benchreplay/score.go",
		Find:     "\tp.MedianRatio = observed.Known(median(ratios))",
		Replace:  "\tsum := 0.0\n\tfor _, r := range ratios {\n\t\tsum += r\n\t}\n\tp.MedianRatio = observed.Known(sum / float64(len(ratios)))",
		Pkg:      "./internal/vibe/benchreplay/",
		MustFail: []string{"TestPairedRatioIsAMedianOfRatiosNotARatioOfMeans"},
		Why: "the sample is the cell's own traffic, so it contains one 100k-token agentic request " +
			"beside thirty small ones. A mean lets that single request decide the answer; the median " +
			"of per-request ratios is the figure both sides having seen the IDENTICAL sample actually " +
			"licenses.",
	},
	{
		Name:     "c25/an unreducible recorded response counts as agreement",
		File:     "internal/vibe/benchreplay/score.go",
		Find:     "\t\tif !rec.known || !inc.shapes[i].known || !cand.shapes[i].known {",
		Replace:  "\t\tif false {",
		Pkg:      "./internal/vibe/benchreplay/",
		MustFail: []string{"TestDivergenceIsNotMeasuredWhenTheRecordedResponseCannotBeReduced"},
		Why: "llama-swap stores the request but NEVER the response body for a non-200, and a request " +
			"that made the model choke is the most interesting one to replay and the one with nothing " +
			"to diverge from. A zero-valued shape scores as `answered nothing, called no tool, " +
			"finished with none` — three claims nobody measured, folded into a divergence figure.",
	},
	{
		Name:     "c25/a replay loads a model that is not resident",
		File:     "internal/vibe/benchreplay/replay.go",
		Find:     "\tif !resident {\n\t\treturn replayResult{}, fmt.Errorf(\"%s is %w\", side.Model, ErrNotResident)\n\t}",
		Replace:  "\t_ = resident",
		Pkg:      "./internal/vibe/benchreplay/",
		MustFail: []string{"TestEveryRefusalFiresByNameAndWritesNothing"},
		Why: "C8's cardinal rule, on the other side of the seam. llama-swap's contract is " +
			"JIT-on-request, so replaying n requests at a model that is not resident STARTS it — and " +
			"on a one-model cell that evicts the model serving the user. The measurement would have " +
			"become an actuator, and the thing it actuated is eviction.",
	},
	{
		Name: "c25/a replay side loses its wall bound",
		File: "internal/vibe/benchreplay/replay.go",
		Find: "\t\tif s.opt.Now().After(deadline) {",
		// `_ = deadline` keeps it compiling: a mutation that fails to build
		// proves nothing about the guard.
		Replace:  "\t\t_ = deadline\n\t\tif false {",
		Pkg:      "./internal/vibe/benchreplay/",
		MustFail: []string{"TestASideThatOutrunsItsBudgetRefusesRatherThanTruncating"},
		Why: "a per-request timeout is not a bound on the command: 40 samples at the 5-minute " +
			"per-request timeout is nearly four hours PER SIDE, and the sample is real agentic " +
			"traffic carrying its own max_tokens. `vibe model try` holds a four-hour lease and a " +
			"four-hour C11 hold on the incumbent while this runs, so a replay that outlived them " +
			"would be measuring a cell the fleet had resumed using. The refusal rather than a short " +
			"n is the other half: a side that replayed 31 of 40 while the other replayed 40 is a " +
			"metric lying about its own n.",
	},
	{
		Name:     "c25/a rate is gated on the sample size instead of its own denominator",
		File:     "internal/vibe/benchreplay/score.go",
		Find:     "\tif sc.ToolsOffered >= floor {",
		Replace:  "\tif sc.Requests >= floor {",
		Pkg:      "./internal/vibe/benchreplay/",
		MustFail: []string{"TestEachRateIsGatedOnItsOwnDenominator"},
		Why: "the tool-call rate divides by the number of requests that OFFERED tools, not by the " +
			"sample size, and in ordinary chat traffic most requests offer none. A 40-sample run " +
			"can therefore compute three-of-five and print it wearing a forty-sample floor's " +
			"authority — which is the number the operator adopts or rejects a model on.",
	},
	{
		Name:     "c25/the divergence excess loses its n floor",
		File:     "internal/vibe/benchreplay/score.go",
		Find:     "\tif d.N < floor {",
		Replace:  "\tif d.N == 0 {",
		Pkg:      "./internal/vibe/benchreplay/",
		MustFail: []string{"TestTheDivergenceExcessHasItsOwnFloor"},
		Why: "the divergence n is routinely far below the sample size — non-200 rows carry no " +
			"recorded response body and unreducible ones drop out — so without a floor of its own, " +
			"ONE sample where the incumbent agreed and the candidate did not prints `100 points " +
			"ABOVE the floor`. A proportion difference is still a proportion.",
	},
	{
		Name:     "c25/a chat-basis path the replay cannot rebuild is admitted",
		File:     "internal/vibe/benchreplay/harvest.go",
		Find:     " || !replayable(row.ReqPath) {",
		Replace:  " {",
		Pkg:      "./internal/vibe/benchreplay/",
		MustFail: []string{"TestEveryRefusalFiresByNameAndWritesNothing"},
		Why: "usagemeter's chat basis covers /v1/completions, /infill, /completion, /v1/responses " +
			"and /v1/messages as well as chat-completions — real endpoints a llama.cpp cell serves, " +
			"with request shapes this package cannot rebuild. Admitted, the rewrite fails, the shape " +
			"comes back unknown, and the row is scored as a request the MODEL failed on both sides. " +
			"/v1/messages is worse: it parses, is POSTed to the wrong API, and its Anthropic-shaped " +
			"tools array reads as `no tools declared`, so it leaves the tool-call denominator silently.",
	},
	{
		Name:     "c25/a refused capture fetch reads as an idle box",
		File:     "internal/vibe/benchreplay/harvest.go",
		Find:     "\t\tif set.stats.Refused > 0 {",
		Replace:  "\t\tif false {",
		Pkg:      "./internal/vibe/benchreplay/",
		MustFail: []string{"TestEveryRefusalFiresByNameAndWritesNothing"},
		Why: "on a cell that keys its own llama-swap — the case C15 exists for, and the one " +
			"swapauth.go's comment says matters — EVERY capture fetch answers 401. Folding that into " +
			"`this cell has served nothing recently` invents a fact about the operator's workload out " +
			"of a credential problem, in the sentence they act on. Absent evidence read as a healthy " +
			"value, one more time.",
	},
	{
		Name:     "vamp/parseJSON stops stripping the model's reasoning block",
		File:     "internal/vamp/exec.go",
		Find:     "\tcleaned := stripModelArtifacts(s)",
		Replace:  "\tcleaned := strings.TrimSpace(s)",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestJSONRecovery_ParseJSONAndExtractCleanJSONAgree"},
		Why: "two functions answered the same question differently on the same bytes, both with a nil " +
			"error: parseJSON stripped fences only, so `readFile … | parseJSON | toJSON` — the chain " +
			"templateFuncs advertises for threading a stage result into a webhook body — returned the " +
			"draft the model discarded inside <think>, while extractCleanJSON returned the answer. A " +
			"wrong answer that parses is the one failure mode nothing downstream can detect.",
	},
	{
		Name:     "vamp/the reasoning-block strip goes back to prefix-only",
		File:     "internal/vamp/exec.go",
		Find:     "\t\topen := strings.Index(s, openTag)",
		Replace:  "\t\topen := -1\n\t\tif strings.HasPrefix(s, openTag) {\n\t\t\topen = 0\n\t\t}",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestJSONRecovery_HostileShapes"},
		Why: "one conversational token in front of the tag — \"Sure,\", \"Okay —\", a quoted line the " +
			"model restated the task on — defeats a prefix test, and what gets through is the model's " +
			"scratch work. It lands as the vision executor's stage output, which every downstream stage " +
			"then consumes, and it passes the output_format: json resume gate, which reports the stage " +
			"healthy about bytes that are not its answer. The test named here is deliberate: the " +
			"orphan-`</think>` rule RESCUES the obvious cases from this mutation (everything before the " +
			"last closer goes), so the agreement test stays green and only a reasoning block that " +
			"follows the payload separates the two rules. Registered by the harness reporting this " +
			"entry UNPROTECTED against the obvious test first.",
	},
	{
		Name:     "vamp/an unclosed reasoning block reads as content",
		File:     "internal/vamp/exec.go",
		Find:     "\t\tif end < 0 {\n\t\t\treturn strings.TrimSpace(b.String())\n\t\t}",
		Replace:  "\t\tif end < 0 {\n\t\t\ts = rest\n\t\t\tbreak\n\t\t}",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestJSONRecovery_UnclosedThinkIsNotAnAnswer"},
		Why: "a generation cut off inside the reasoning stream never reached an answer, so there is no " +
			"answer in it to find. Read as content, the extractor hands back the first draft object in " +
			"the scratch work and the run continues on it. Failing costs one re-run in GPU minutes, " +
			"which is the cheaper of the two mistakes and the only one that is visible.",
	},
	{
		Name:     "vamp/the JSON extractor's opener scan stops skipping quoted delimiters",
		File:     "internal/vamp/exec.go",
		Find:     "\tif block, ok := scanJSONBlock(s, true); ok {",
		Replace:  "\tif block, ok := scanJSONBlock(s, false); ok {",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestExtractFirstJSONBlock_OpenerSkipsQuotedDelimiters"},
		Why: "this is the layer that exists to salvage messy model output, and a `{` quoted in the " +
			"preamble made it decline the payload sitting right behind it. The doc comment claimed the " +
			"scan was string-aware while only the BODY half was — the defect a reader verifying the " +
			"comment cannot see. On the resume gate it re-runs a stage that succeeded; on the live path " +
			"it fails the stage with an error pointing at the prose instead of the JSON.",
	},
	{
		Name:     "vamp/a non-JSON balanced span shadows the payload behind it",
		File:     "internal/vamp/exec.go",
		Find:     "\t\ti = end + 1",
		Replace:  "\t\treturn \"\", false",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestExtractFirstJSONBlock_OpenerSkipsQuotedDelimiters"},
		Why: "markdown link syntax in front of the answer (`a **[list](x)** now:` then the array) is an " +
			"ordinary model output shape and `[list]` is balanced, so a single-candidate scan hands the " +
			"caller a span that cannot parse and stops. The advance is also what keeps the scan linear: " +
			"it moves PAST the span it just rejected rather than restarting at the next opener, so " +
			"200k unmatched braces stay 200k steps instead of becoming quadratic.",
	},
	{
		Name:     "vamp/writeFile publishes its outputs past the umask",
		File:     "internal/vamp/exec.go",
		Find:     "\ttmp, err := createTempMode(dir, 0o644)",
		Replace:  "\ttmp, err := os.CreateTemp(dir, \".vamp-write-*\")\n\tif err == nil {\n\t\terr = os.Chmod(tmp.Name(), 0o644)\n\t}",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestWriteFile_HonoursUmask"},
		Why: "the exact pair the atomic-write refactor reached for, and the reason a run dir stopped " +
			"being private: os.WriteFile is umask-filtered and os.Chmod is not, so `umask 077` got 0644 " +
			"where the code this replaced gave 0600. writeFile persists StageOutput text, and for a " +
			"webhook stage that is the raw HTTP response body #76 chose to write verbatim BECAUSE the " +
			"run dir is private. The mode travels when a run dir is rsync'd, tarred or backed up; the " +
			"enclosing directory's permissions do not.",
	},
	{
		Name:     "vamp/a cleanup glob reaches the run's own provenance",
		File:     "internal/vamp/exec.go",
		Find:     "\t\t\tif IsRunMetadataFile(rel) {",
		Replace:  "\t\t\tif false {",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestRunStageCleanup_KeepsRunMetadata"},
		Why: "`cleanup: [\"*.json\"]` is an ordinary thing to write for a pipeline whose intermediates " +
			"are JSON, and it took inputs.json with them — written once, never rebuilt, the only " +
			"on-disk record of what the run was given. `cleanup: [\"*\"]` also takes " +
			"pipeline.yaml.snapshot (so --resume now demands --resume-force, the mode that skips drift " +
			"detection entirely) and vamp.pid (so `vamp cancel` can no longer find the job). Both " +
			"reported a successful cleanup.",
	},
	{
		Name:     "c25/a partially refused harvest reports a short n and no reason",
		File:     "internal/vibe/benchreplay/report.go",
		Find:     "\tif r.Sample.Refused > 0 {",
		Replace:  "\tif false {",
		Pkg:      "./internal/vibe/benchreplay/",
		MustFail: []string{"TestAPartiallyRefusedHarvestSaysSoOnTheReport"},
		Why: "the mixed case the refusal STRING cannot cover: some capture fetches answered and some " +
			"401'd, so the harvest succeeds with a short n and prints a denominator that shrank for a " +
			"reason it never names. The reason is a credential problem wearing a workload's clothes, " +
			"and the operator reads the number as a fact about what their box serves.",
	},
	{
		Name:     "c25/the harvest loses its wall bound",
		File:     "internal/vibe/benchreplay/harvest.go",
		Find:     "\t\t\tif opt.Now().After(deadline) {",
		Replace:  "\t\t\t_ = deadline\n\t\t\tif false {",
		Pkg:      "./internal/vibe/benchreplay/",
		MustFail: []string{"TestTheHarvestHasItsOwnWallBound"},
		Why: "MaxPages by up to 999 rows, each candidate costing a fetch with its own 30s timeout, " +
			"and a row that 404s or 401s never counting toward MaxSample — so the fetch count has no " +
			"cap of its own. It runs under the trial's four-hour lease and C11 hold, immediately in " +
			"front of a config write that evicts every resident model.",
	},
	{
		Name:     "c25/a trial without --replay reads the operator's captures",
		File:     "internal/vibe/cli/cmd_model_try.go",
		Find:     "\tif !gate.replay {\n\t\treturn nil, nil\n\t}",
		Replace:  "\tif false {\n\t\treturn nil, nil\n\t}",
		Pkg:      "./internal/vibe/cli/",
		MustFail: []string{"TestWithoutTheFlagNoCaptureIsEverRead"},
		Why: "the one guard between an ordinary `vibe model try` and a read of the operator's " +
			"verbatim prompts. A verb that read them in order to decide whether to read them would " +
			"be a bad joke — and the first version of the test that covers this could not see the " +
			"deletion at all, because it drove against a dead fleetd and returned before the harvest " +
			"call site was ever reached. Registered so the reachability cannot rot again.",
	},
	{
		Name:     "c25/the replay edits the client's own sampling",
		File:     "internal/vibe/benchreplay/replay.go",
		Find:     "\tobj[\"stream\"] = json.RawMessage(\"false\")",
		Replace:  "\tobj[\"stream\"] = json.RawMessage(\"false\")\n\tobj[\"seed\"] = json.RawMessage(\"42\")\n\tobj[\"temperature\"] = json.RawMessage(\"0\")",
		Pkg:      "./internal/vibe/benchreplay/",
		MustFail: []string{"TestReplayRewritesOnlyTheModelAndTheStreamFlag"},
		Why: "the obvious fix to a noisy divergence metric, and the wrong one. Real agentic clients " +
			"send temperature above zero and no seed; injecting either makes the replay deterministic " +
			"by falsifying the request — and a sample you had to falsify is no longer YOUR workload, " +
			"which was this phase's entire point. Same objection C7a raised to injecting stream_options.",
	},
	// U6 (PR #48) hand-verified these three and deliberately did not add
	// them: eight sibling branches were landing in parallel, and a shared
	// slice literal is exactly where a merge-created defect comes from.
	// The reconciliation pass is where that debt gets paid.
	{
		Name:     "c8/a probe issued through Run carries no deadline",
		File:     "internal/vibe/modelprobe/modelprobe.go",
		Find:     "\tctx, cancel := context.WithTimeout(ctx, probeTimeout)\n\tdefer cancel()\n",
		Replace:  "\tctx, cancel := context.WithCancel(ctx)\n\tdefer cancel()\n",
		Pkg:      "./internal/vibe/modelprobe/",
		MustFail: []string{"TestProbeDeadlines_EveryProbeRequestCarriesItsOwnBound"},
		Why: "Start is not the only caller: `vibe model try` calls Run directly and the default client " +
			"has no timeout of its own, so without this line a wedged llama-server holds the cell's " +
			"single probe slot for as long as the socket stays open.",
	},
	{
		Name:     "c8/the residency read's error is dropped",
		File:     "internal/vibe/modelprobe/modelprobe.go",
		Find:     "\tresident, err := p.isResident(ctx, model)\n\tif err != nil {\n\t\treturn p.note(model, spec.Kind, \"cannot read local llama-swap /running: \"+err.Error())\n\t}\n",
		Replace:  "\tresident, _ := p.isResident(ctx, model)\n",
		Pkg:      "./internal/vibe/modelprobe/",
		MustFail: []string{"TestProbeDeadlines_AResidencyReadThatNeverAnswersIsNotAResidencyVerdict", "TestIsResidentsErrorIsNeverDiscarded"},
		Why: "`false, err` is no answer and `false, nil` is an answered no. Dropping the error prints " +
			"\"not resident — warm it first\" — the note that means the cardinal rule fired — when " +
			"nothing was checked, and sends the operator to warm a model that was already warm.",
	},
	{
		Name:     "c8/an abandoned probe is not billed as an attempt",
		File:     "internal/vibe/modelprobe/modelprobe.go",
		Find:     "\tp.markAttempt(model, now)\n\tres, err := p.measure(ctx, spec)\n\tif err != nil {\n\t\treturn p.note(model, spec.Kind, \"probe request failed: \"+err.Error())\n\t}\n",
		Replace:  "\tres, err := p.measure(ctx, spec)\n\tif err != nil {\n\t\treturn p.note(model, spec.Kind, \"probe request failed: \"+err.Error())\n\t}\n\tp.markAttempt(model, now)\n",
		Pkg:      "./internal/vibe/modelprobe/",
		MustFail: []string{"TestProbeDeadlines_AnAbandonedProbeIsAFailedAttemptAndNeverAMeasurement"},
		Why: "an abandoned probe spent the GPU time anyway. Billing only successes makes a timing-out " +
			"model the CHEAPEST thing to probe, and a wedged box re-probes it every interval against " +
			"neither the cooldown nor the 96/day cap.",
	},
	// ── the wrapper's exit status (U7) ────────────────────────────────
	//
	// Two commands in this repo are WRAPPERS a launcher is pointed at, and
	// root.go states the rule for both: a wrapper that swallows its child's
	// status is a wrapper a launcher cannot be pointed at. C24 fixed the
	// drain wrapper and registered nothing; the `vibe run` half was found
	// still returning nil eleven phases later. Both halves are registered
	// now, because "we fixed this once" is exactly the claim this package
	// exists to stop trusting.
	{
		Name:     "u7/`vibe run` swallows the frontend's exit status",
		File:     "internal/vibe/cli/cmd_run.go",
		Find:     "\t\treturn exitCodeError{msg: binary, code: exitErr.ExitCode()}",
		Replace:  "\t\treturn nil",
		Pkg:      "./internal/vibe/cli/",
		MustFail: []string{"TestRunPropagatesTheFrontendsExitStatus", "TestFrontendExitError_CarriesTheChildsCode"},
		Why: "`vibe run` is the wrapper README points a launcher at, and it returned nil for ANY " +
			"*exec.ExitError — so a frontend that died on a bad config, an OOM or a panic reached the " +
			"shell as a clean quit and `vibe run omp && next` ran next. The C24 sibling flattened 3 to " +
			"1; this flattened it to 0, which is the worse direction. The comment that shipped over it " +
			"claimed nil was needed \"so the teardown defer fires cleanly\", which is not how Go works — " +
			"so the entry names the teardown assertion too.",
	},
	{
		Name: "u7/an aborted drain reads as a completed one",
		File: "internal/vibe/cli/cmd_cell_actuate.go",
		Find: "\t\tif strings.ToLower(strings.TrimSpace(answer)) != \"y\" {\n" +
			"\t\t\tfmt.Fprintln(out, \"aborted\")\n\t\t\treturn errDrainAborted\n\t\t}",
		// The pre-U7 spelling: print "aborted" and answer the caller with
		// the same nil a completed drain returns. The EOF branch above is
		// deliberately NOT the mutation target — an empty answer falls
		// through to this test anyway, so that branch is a redundancy and an
		// entry on it would report UNPROTECTED for a guard that works.
		Replace: "\t\tif strings.ToLower(strings.TrimSpace(answer)) != \"y\" {\n" +
			"\t\t\tfmt.Fprintln(out, \"aborted\")\n\t\t\treturn nil\n\t\t}",
		Pkg:      "./internal/vibe/cli/",
		MustFail: []string{"TestCellDrainPromptAnswers", "TestDrainUntilExitAbortedDrainRunsNothing"},
		Why: "the only caller that reads this error is drainUntilExit, whose whole contract is drain → " +
			"run → resume. Answering an abort with nil ran the operator's command against a cell that " +
			"was still serving, then issued a CellResume for a cell nothing had drained, and exited 0 — " +
			"so the wrapper said the reclaim happened. The shipped launcher passes --yes and never " +
			"reaches the prompt, which is why this survived: the defect is real only for a human typing " +
			"the verb, and nothing in the suite could type it until the prompt got its test seams.",
	},

	{
		Name: "c26b/the model-rewrite reader buffers the stream",
		File: "internal/vibe/proxy/proxy.go",
		Find: "\t\t\temit := len(r.buf)\n\t\t\tif !r.eof {\n" +
			"\t\t\t\temit -= r.partialTail()\n\t\t\t}\n",
		Replace:  "\t\t\temit := len(r.buf)\n\t\t\tif !r.eof {\n\t\t\t\temit = 0\n\t\t\t}\n",
		Pkg:      "./internal/vibe/proxy/",
		MustFail: []string{"TestProxy_StreamingCompletionIsUnbuffered", "TestReplacingReader_HoldsBackOnlyAnAmbiguousTail"},
		Why: "ground rule 1, mechanically. The rewrite reader holds back the tail that could still " +
			"become a match, so a needle split across chunks is still found; holding back EVERYTHING " +
			"until EOF is the same code one condition simpler and passes every correctness test in the " +
			"package, because the bytes are still right — they just arrive at the end. Claude Code kills " +
			"a stalled stream at ~5 min, so that turns a slow model into a broken one. Registered " +
			"because the guard went red once in CI for a reason of its own making (C26b), and a guard " +
			"people have learned to re-run is worse than no guard. Re-pointed when the hold-back stopped " +
			"being a fixed needle LENGTH: at that size the guard's own upstream id (11 chars) was 27 " +
			"short of the length that would have stalled it, so it could not tell a hold-back bounded by " +
			"ambiguity from one bounded by the id — which is what the second named test, and the " +
			"mlx-path case inside the first, now assert.",
	},

	// ── class 6: the retrieval plane's egress and its front door ──────
	//
	// vibe-search was verified end to end as an unauthenticated SSRF
	// proxy: POST /fetch and MCP fetch_url returned the body of an
	// internal service to a caller with no credential, followed a 302
	// into one without re-checking the target, and accepted any Origin.
	// The five entries below are the five guards that replaced that.
	{
		Name:    "search/the fetch dialer stops refusing non-global addresses",
		File:    "internal/vibe/search/dialguard.go",
		Find:    "\t\t\tif reason := blockedIPReason(ip); reason != \"\" {",
		Replace: "\t\t\tif reason := \"\"; reason != \"\" {",
		Pkg:     "./internal/vibe/search/",
		MustFail: []string{
			"TestDirectFetcherRefusesALoopbackTarget",
			"TestFetchIsRefusedAtTheRedirectHopIntoThePrivateNetwork",
			"TestEveryResolvedAddressIsChecked",
			"TestPostFetchOfALoopbackURLIsRefused",
		},
		Why: "the one line between an LLM-supplied URL and every HTTP service the search host can " +
			"reach — NAS and router admin pages, the registry, the other cells. The REDIRECT test is " +
			"named alongside the direct one on purpose: it is what proves the check is at the dialer " +
			"and not on the URL. Under this mutation the 302 into 127.0.0.1 is followed and the " +
			"internal page comes back as the fetch result, which no URL-level allowlist would have " +
			"caught, because net/http follows the hop with no second URL for anything to inspect.",
	},
	{
		Name:    "search/an IPv6 address that wraps a private IPv4 one is dialled",
		File:    "internal/vibe/search/dialguard.go",
		Find:    "\tif v4 := embeddedIPv4(ip); v4 != nil {",
		Replace: "\tif v4 := embeddedIPv4(ip); false && v4 != nil {",
		Pkg:     "./internal/vibe/search/",
		MustFail: []string{
			"TestBlockedIPReasonSeesThroughIPv6TransitionAddresses",
		},
		Why: "the predicate set only ever saw the one wrapped form net.IP.To4 normalizes for it " +
			"(::ffff:a.b.c.d). Measured, `::127.0.0.1`, `64:ff9b::c0a8:1` and `2002:7f00:1::` all came " +
			"back as ordinary global unicast and every other test in the package stayed green — the " +
			"guard looks complete, is at the dialer, checks every resolved answer, and waves the " +
			"textbook bypass through. The URL is supplied by a model that has just read somebody " +
			"else's page, and on a v6-only fleet the NAT64 prefix IS how IPv4 is reached, so the " +
			"unwrap must judge the wrapped address rather than refuse the prefix.",
	},
	{
		Name:     "search/the validated address is handed back to the resolver",
		File:     "internal/vibe/search/dialguard.go",
		Find:     "conn, dialErr := g.dialAddr(ctx, network, net.JoinHostPort(ip.String(), port))",
		Replace:  "conn, dialErr := g.dialAddr(ctx, network, net.JoinHostPort(host, port))",
		Pkg:      "./internal/vibe/search/",
		MustFail: []string{"TestTheDialGoesToTheAddressThatWasChecked"},
		Why: "DNS rebinding, and the reason resolve-then-dial-the-NAME is not a fix. Every predicate " +
			"above this line still runs and every other test in the package still passes — the guard " +
			"looks present, judges a public address, and then lets the OS resolve the name a second " +
			"time on the way to the socket. A resolver that answers differently on the second lookup " +
			"walks straight into the fleet's LAN with the verdict already recorded as clean.",
	},
	{
		Name:     "search/private targets are permitted unless the operator forbids them",
		File:     "internal/vibe/search/dialguard.go",
		Find:     "func AllowPrivateFetch() bool { return os.Getenv(AllowPrivateEnv) == \"1\" }",
		Replace:  "func AllowPrivateFetch() bool { return os.Getenv(AllowPrivateEnv) != \"0\" }",
		Pkg:      "./internal/vibe/search/",
		MustFail: []string{"TestPrivateFetchIsOffUnlessTheEnvSaysExactlyOne"},
		Why: "the escape hatch's DEFAULT, which is the whole of its security value. Inverted like " +
			"this the guard is still fully implemented and every dial test still passes, because the " +
			"tests construct the fetcher directly — but no deployment that did not explicitly set " +
			"VIBE_SEARCH_ALLOW_PRIVATE=0 has a guard at all. An opt-out that reads a stray or absent " +
			"value as ON is indistinguishable from having shipped no opt-out.",
	},
	{
		Name:     "search/a token-less start is allowed to serve",
		File:     "cmd/vibe-search/main.go",
		Find:     "\tif !noAuth {\n",
		Replace:  "\tif false {\n",
		Pkg:      "./cmd/vibe-search/",
		MustFail: []string{"TestStartupRefusesToServeWithoutATokenUnlessNoAuth"},
		Why: "the pre-fix default, restored exactly: no VIBE_SEARCH_TOKEN warns and then serves every " +
			"route to everyone. It fails OPEN on the service whose documented deployment is " +
			"--bind 0.0.0.0 beside the router, so any host on the LAN gets fetch_url and the search " +
			"quota. The warning it replaced scrolled past once at boot and the service worked " +
			"perfectly afterwards, which is why nobody ever went back and read it.",
	},
	{
		Name:     "search/the MCP and fetch surfaces stop checking Origin",
		File:     "internal/vibe/search/server.go",
		Find:     "if origin := r.Header.Get(\"Origin\"); origin != \"\" && !isLoopbackOrigin(origin) {",
		Replace:  "if origin := r.Header.Get(\"Origin\"); false && origin != \"\" && !isLoopbackOrigin(origin) {",
		Pkg:      "./internal/vibe/search/",
		MustFail: []string{"TestCrossOriginBrowserRequestIsRefused"},
		Why: "a JSON-RPC body posted as text/plain is a CORS simple request, so a page the operator " +
			"is merely looking at can drive /mcp on 127.0.0.1 with no preflight — and after a DNS " +
			"rebind the browser treats the reply as same-origin, so CORS stops withholding it. The " +
			"Origin header is the one signal that survives the rebind, which is why MCP's own " +
			"transport spec requires this check on every streamable-HTTP server.",
	},
	{
		Name: "search/the bearer compare goes back to byte-wise ==",
		File: "internal/vibe/search/server.go",
		Find: "if got == \"\" || subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {",
		// The pre-fix line, with subtle kept referenced so the mutation
		// COMPILES — a build failure proves nothing.
		Replace:  "if _ = subtle.ConstantTimeCompare; got != s.Token {",
		Pkg:      "./internal/vibe/search/",
		MustFail: []string{"TestBearerCompareIsConstantTime"},
		Why: "`==` on strings returns at the first differing byte, so this endpoint answers a " +
			"near-miss measurably later than a miss and a 32-byte token falls to ~8k requests. What " +
			"makes it worth a registry entry rather than a review note is that the mutation is " +
			"BEHAVIOUR-PRESERVING: every other test in the package stays green, including the " +
			"bearer/basic/healthz auth test, because the two spellings accept and refuse exactly the " +
			"same inputs. Measured. Only a structural assertion can see it, and the credential it " +
			"protects unlocks fetch_url — the SSRF surface dialguard.go exists for — on a service " +
			"whose documented deployment is --bind 0.0.0.0.",
	},
	{
		Name:     "search/a caller's URL goes into the log with its query intact",
		File:     "internal/vibe/search/redact.go",
		Find:     "\t\t\tparts[i] = k + \"=\" + redactedValue",
		Replace:  "\t\t\tparts[i] = k + \"=\" + v",
		Pkg:      "./internal/vibe/search/",
		MustFail: []string{"TestRedactURLWithholdsUserinfoAndQueryValues", "TestFetchLogsWithholdTheCallersCredential"},
		Why: "url.URL.Redacted() strips USERINFO and nothing else, and the credential shape that " +
			"actually reaches this service rides in the QUERY: a presigned S3 link's " +
			"X-Amz-Signature, a share link's ?token=. Those URLs are handed to a model and passed " +
			"straight to fetch_url, and the log they land in is journald on a host whose logs get " +
			"pasted into issues — a bearer credential in a file with a different lifetime from the " +
			"thing it opens. The mutation leaves userinfo handling fully intact, which is the point: " +
			"the half that looks like the whole fix is not.",
	},
	{
		Name:     "search/the URL net/http embeds in *url.Error survives",
		File:     "internal/vibe/search/redact.go",
		Find:     "\tif errors.As(err, &ue) && ue.Err != nil {",
		Replace:  "\tif errors.As(err, &ue) && false {",
		Pkg:      "./internal/vibe/search/",
		MustFail: []string{"TestDirectFetchErrorDoesNotCarryTheCallersCredential"},
		Why: "the second copy, and redacting our own format argument does not touch it. net/http's " +
			"stripPassword replaces the password in *url.Error and leaves the query alone, so " +
			"`Get \"http://svc:***@h/p?X-Amz-Signature=…\": dial tcp …` carries the signature into " +
			"every message that wraps the transport error with %w. Verified both ways on a " +
			"workstation: with redactURL applied and this unwrap removed, the signature was still in " +
			"the error text. Pinned separately from the entry above for the same reason " +
			"fleetnotify's two halves are — neither covers the other's case.",
	},

	// ── class 8: a contract between two packages that only prose held ─
	//
	// The four entries below pin one chain end to end: the daemon emits a
	// typed refusal, vibeclient classifies it, and vamp's candidate walk
	// acts on it. It is registered because the chain has ALREADY broken
	// once in exactly the way a registry catches and a review does not.
	//
	// vibeclient.IsVRAMRejection used to substring-match the daemon's
	// human-facing pre-flight message for "free VRAM". 4a4c5ea rewrote
	// that message AND deleted the condition that emitted it — short of
	// free memory became a slog.Warn — so from that commit forward no
	// code path in the tree produced a string the classifier could match.
	// Every capability with N>1 candidates silently degraded to
	// candidate-1-or-abort, and candidates 2..N of capabilities.yaml were
	// dead config for as long as nobody read the daemon and the client in
	// the same sitting.
	//
	// CI stayed green throughout, because each of the two covering tests
	// CONSTRUCTED the daemon message it then asserted on. That is the
	// whole lesson: a test that mints its own input proves the matcher
	// matches itself. These entries assert the opposite property — break
	// the sender, and the receiver's test goes red.
	{
		Name:    "vram/the start refusal loses its typed reason",
		File:    "internal/vibe/daemon/rejection.go",
		Find:    "\tce.AddDetail(d)\n\treturn ce",
		Replace: "\t_ = d\n\treturn ce",
		Pkg:     "./internal/vibe/daemon/",
		MustFail: []string{
			"TestDaemon_VRAMCheck_ExceedsCapacity",
			"TestDaemon_VRAMCheck_StrictRefusesInsufficientFree",
			"TestDaemon_VRAMCheck_ARefusalDoesNotBlockTheNextCandidate",
		},
		Why: "THE producer→classifier entry. Both named tests drive a real daemon over a real Connect " +
			"socket and ask vibeclient to classify what came back, so a refusal that stops carrying " +
			"its typed reason goes red at the receiver. Without this, the refusal degrades to prose " +
			"only — which is precisely the state the tree was in from 4a4c5ea until this was fixed, " +
			"and in that state vamp's candidate walk cannot tell 'try a smaller model' from 'your " +
			"config is broken' and aborts the pipeline on both.",
	},
	{
		Name:    "vram/the strict pre-flight stops refusing a short free read",
		File:    "internal/vibe/daemon/daemon.go",
		Find:    "\t\tcase res.Warn && req.Msg.StrictVram:",
		Replace: "\t\tcase res.Warn && req.Msg.StrictVram && false:",
		Pkg:     "./internal/vibe/daemon/",
		MustFail: []string{
			"TestDaemon_VRAMCheck_StrictRefusesInsufficientFree",
			"TestDaemon_VRAMCheck_ARefusalDoesNotBlockTheNextCandidate",
		},
		Why: "this branch IS the rejection class. Delete it and insufficient free memory is a warning " +
			"for everyone again — the 4a4c5ea state, in which the one case vamp's biggest-first walk " +
			"exists for ('another model is resident') can never fire, so a capability's second " +
			"candidate is unreachable no matter what the classifier does. Its twin " +
			"TestDaemon_VRAMCheck_StrictIsOptInOnly stays GREEN under this mutation on purpose: the " +
			"pair is what distinguishes an opt-in from a restored hard failure.",
	},
	{
		Name:     "vram/the candidate walk stops asking for a strict pre-flight",
		File:     "internal/vamp/exec.go",
		Find:     "\t\topts := vibeclient.StartOptions{StrictVRAM: i < len(candidates)-1}",
		Replace:  "\t\topts := vibeclient.StartOptions{StrictVRAM: i < 0}",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestExecutor_VRAMFallbackPicksNextCandidate"},
		Why: "the consumer's half. The daemon's default is warn-don't-block, so a walk that never asks " +
			"to be refused gets a successful start from candidate 1 every time and never reaches the " +
			"smaller model behind it — green, silent, and identical in behaviour to having no " +
			"candidate list at all. `i < 0` rather than `false` because an unused range variable does " +
			"not compile, and a mutation that does not build proves nothing.",
	},
	{
		Name:     "vram/strictness is applied to the last candidate too",
		File:     "internal/vamp/exec.go",
		Find:     "\t\topts := vibeclient.StartOptions{StrictVRAM: i < len(candidates)-1}",
		Replace:  "\t\topts := vibeclient.StartOptions{StrictVRAM: i >= 0}",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestExecutor_SingleCandidateIsNotAskedForAStrictPreFlight"},
		Why: "the over-correction, which is the likelier future edit — 'why not just always be strict?' " +
			"reads as a simplification. Strictness is only ever paid for by having somewhere else to " +
			"go; applied to the last candidate it converts every tight-but-workable pipeline start " +
			"into a dead pipeline, which is the false-negative behaviour 4a4c5ea removed on measured " +
			"evidence (the same profile on the same box read 15.2 and 23.5 GiB free minutes apart). " +
			"Same Find as the entry above, opposite direction: one guards the floor, one the ceiling.",
	},

	// ── vamp template helpers: the parsers that read model output ──────
	{
		Name:    "vamp/a-lesson-name-may-climb-out-of-the-lesson-root",
		File:    "internal/vamp/exec.go",
		Find:    "\tif err != nil || rel == \"..\" || strings.HasPrefix(rel, \"..\"+string(filepath.Separator)) {",
		Replace: "\tif err != nil || rel == \"\" {",
		Pkg:     "./internal/vamp/",
		MustFail: []string{
			"TestEnumerateImagePairs_RefusesLessonEscapingTheRoot",
			"TestEnumerateUniqueImages_RefusesLessonEscapingTheRoot",
			"TestImageDescriptionsFor_RefusesLessonEscapingTheRoot",
			"TestLessonEscapeRefusalReachesTheTemplate",
		},
		Why: "the same threat ensureUnderRunDir names, one directory over. The lessons array these three " +
			"helpers join against the lesson root is a PRIOR STAGE'S OUTPUT — the model's own text — and " +
			"every image_path enumerateImagePairs emits is then read and attached to a vision prompt. " +
			"filepath.Join does not refuse \"..\", it RESOLVES it, so a lesson entry of \"../../..\" turns " +
			"the fan-out into an arbitrary-file read whose contents land in the next prompt. " +
			"`rel == \"\"` rather than `false` because Rel never returns the empty string, and an unused " +
			"variable does not compile.",
	},
	{
		Name:     "vamp/a-searxng-body-with-no-results-field-reads-as-zero-hits",
		File:     "internal/vamp/exec.go",
		Find:     "\t\trawResults, hasKey := resp[\"results\"]\n\t\tif !hasKey {",
		Replace:  "\t\trawResults, hasKey := resp[\"results\"]\n\t\tif !hasKey && false {",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestParseSearXNGTemplate_MissingResultsKeyIsAnError"},
		Why: "a search that found nothing carries a results key (empty or null) and is a real answer; a " +
			"body with no results key is an error page, a proxy response or an LLM echo. Folded together, " +
			"a research pipeline whose search backend is down completes GREEN with zero sources and " +
			"writes the report anyway — the operator's only signal is a short document.",
	},
	{
		Name:     "vamp/an-upstream-that-wrote-nothing-reads-as-an-empty-search",
		File:     "internal/vamp/exec.go",
		Find:     "\tif responses == 0 {",
		Replace:  "\tif responses < 0 {",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestParseSearXNGTemplate_EmptyInputIsAnError"},
		Why: "the inverse half of the same guard. json.Decoder over an empty string reaches EOF on the " +
			"first read, so a webhook stage that produced no body at all parsed to {\"items\":[]} and no " +
			"error. Zero responses is a broken fetch, not an empty result set.",
	},
	{
		Name:     "vamp/a-mediawiki-error-body-reads-as-zero-search-hits",
		File:     "internal/vamp/exec.go",
		Find:     "\t\treturn \"\", fmt.Errorf(\"parseWikipediaSearch: response has no \\\"query\\\" object%s\", mediawikiErrorSuffix(resp))",
		Replace:  "\t\treturn `{\"items\":[]}`, nil",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestParseWikipediaSearchTemplate_APIErrorIsNotZeroHits"},
		Why: "MediaWiki's zero-hit shape always carries query.search, so an absent query object is a " +
			"REFUSED request — invalidparammix, invalidtitle, a rate limit — or a body that is not a " +
			"MediaWiki response at all. Returning empty items for it is how a research run reports " +
			"success on a document with no sources in it, and it hides the API's own explanation.",
	},
	{
		Name:     "vamp/a-mediawiki-error-body-reads-as-zero-extract-pages",
		File:     "internal/vamp/exec.go",
		Find:     "\t\treturn \"\", fmt.Errorf(\"parseWikipediaExtract: response has no \\\"query\\\" object%s\", mediawikiErrorSuffix(resp))",
		Replace:  "\t\treturn `{\"items\":[]}`, nil",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestParseWikipediaExtractTemplate_APIErrorIsNotZeroPages"},
		Why: "the sibling parser. A page that does not exist arrives as a query.pages entry with pageid " +
			"-1 and is correctly skipped; a response with no query object is a fault. Guarding one of the " +
			"two MediaWiki parsers is the one-of-N defect this package keeps producing.",
	},
	{
		Name:     "vamp/empty-tts-text-fans-out-as-one-empty-segment",
		File:     "internal/vamp/exec.go",
		Find:     "\tif text == \"\" {",
		Replace:  "\tif text == \"\\x00\" {",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestSplitSentences_EmptyInputIsEmptyArray"},
		Why: "splitSentences returned [\"\"] for empty input — ONE chunk holding the empty string, which " +
			"the TTS foreach downstream fans out into a synthesis call with nothing to say. A stage that " +
			"produced nothing must read downstream as zero units of work, not one.",
	},
	{
		Name:     "vamp/an-empty-chunk-list-marshals-as-json-null",
		File:     "internal/vamp/exec.go",
		Find:     "\tout := []chunk{}",
		Replace:  "\tvar out []chunk",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestChunkParagraphs_EmptyInputIsEmptyArrayNotNull"},
		Why: "a nil slice marshals to `null`, and null is the one JSON value a foreach source cannot be: " +
			"resolveForeachItems rejects it with \"must be a JSON array ... got <nil>\". The helper's own " +
			"doc comment promised `[]`. The empty case therefore FAILED the next stage instead of running " +
			"zero iterations, and the error named the consumer rather than the producer.",
	},
	{
		Name:     "vamp/an-empty-image-fan-out-marshals-as-json-null",
		File:     "internal/vamp/exec.go",
		Find:     "\tpairs := []pair{}",
		Replace:  "\tvar pairs []pair",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestEnumerateImagePairs_NoImagesIsEmptyArrayNotNull"},
		Why: "same shape, different helper: enumerateUniqueImages built through make and emitted `[]`, " +
			"enumerateImagePairs used a nil slice and emitted `null`. A curriculum whose lessons carry no " +
			"diagrams broke the vision fan-out rather than skipping it.",
	},
	{
		Name:     "vamp/a-lesson-glob-matching-nothing-reads-as-no-lessons",
		File:     "internal/vamp/exec.go",
		Find:     "\tif len(matches) == 0 {\n\t\treturn \"\", fmt.Errorf(\"enumerateLessons %s: no directories matched\", pattern)\n\t}",
		Replace:  "\tif len(matches) < 0 {\n\t\treturn \"\", fmt.Errorf(\"enumerateLessons %s: no directories matched\", pattern)\n\t}",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestEnumerateLessons_NoMatchesIsAnError"},
		Why: "enumerateDirs returned JSON `null` and no error for a stale or typo'd lesson root, while " +
			"BOTH its siblings (readFiles, readFileBatch) already errored on the same condition. The " +
			"foreach it feeds then completed green having processed nothing. The test asserts the " +
			"zero-match MESSAGE, not merely that some error occurred, because the all-filtered guard " +
			"below would otherwise absorb this mutation.",
	},
	{
		Name:     "vamp/lesson-dirs-that-all-fail-the-filter-read-as-no-lessons",
		File:     "internal/vamp/exec.go",
		Find:     "\tif len(dirs) == 0 {",
		Replace:  "\tif len(dirs) < 0 {",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestEnumerateLessons_NoMatchesIsAnError"},
		Why: "the other half: the glob matched, but nothing survived the is-a-directory / has-a-lesson.md " +
			"/ under-1MB filter. Same silent-empty outcome, different remedy, so it is a separate rung " +
			"with its own message — and without it the nil dirs slice marshals to `null` again.",
	},

	// ── vamp diff / viz ────────────────────────────────────────────────
	{
		Name:     "vamp/the-differ-reads-an-output-path-outside-the-run-dir-prior-state",
		File:     "internal/vamp/diff.go",
		Find:     "\t\tif err := ensureUnderRunDir(outPath); err != nil {\n\t\t\tout[st.ID] = &stageResult{}\n\t\t\tcontinue\n\t\t}",
		Replace:  "\t\tif err := ensureUnderRunDir(outPath); err != nil && false {\n\t\t\tout[st.ID] = &stageResult{}\n\t\t\tcontinue\n\t\t}",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestCompare_RefusesOutputPathEscapingTheRunDir"},
		Why: "the differ was one of four consumers of a rendered `output:` template and the only pair " +
			"that did not apply the containment rule. Executor.snapshot writes pipeline.yaml.snapshot " +
			"VERBATIM before any stage runs, so a template the executor would refuse still reaches the " +
			"differ — which reads the file and, if it looks textual, embeds it whole into the report " +
			"the operator prints or pipes as JSON.",
	},
	{
		Name:     "vamp/the-differ-stats-an-output-path-outside-the-run-dir",
		File:     "internal/vamp/diff.go",
		Find:     "\tif err := ensureUnderRunDir(outPath); err != nil {\n\t\treturn StageOutputSide{Missing: true}\n\t}",
		Replace:  "\tif err := ensureUnderRunDir(outPath); err != nil && false {\n\t\treturn StageOutputSide{Missing: true}\n\t}",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestCompare_RefusesOutputPathEscapingTheRunDir"},
		Why: "the second of the two sites, and the one that feeds loadStageOutput — which sha256s the " +
			"file and puts its bytes in StageOutputSide.Content. Guarding one of two is the defect " +
			"class; both rungs are separate entries so removing either is caught.",
	},
	{
		Name:     "vamp/a-sub-line-difference-is-reported-as-no-difference",
		File:     "internal/vamp/diff.go",
		Find:     "\tvar out strings.Builder\n\tif len(hunks) == 0 {",
		Replace:  "\tvar out strings.Builder\n\tif len(hunks) == 0 {\n\t\treturn out.String()",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestUnifiedDiff_TrailingNewlineIsNotReportedAsIdentical", "TestCompare_TrailingNewlineChangeSurfacesInTheReport"},
		Why: "splitLinesKeepEmpty strips exactly one trailing newline, so \"x\\n\" and \"x\" produce " +
			"identical line slices and zero hunks. The empty string that came back is every caller's " +
			"sentinel for NO CHANGE, and the human renderer prints `output: (identical)` for two " +
			"outputs that differ. A missing final newline is one of the commonest real diffs there is.",
	},
	{
		Name:     "vamp/a-stage-filter-matching-nothing-exits-zero",
		File:     "internal/vamp/diff.go",
		Find:     "\t\t\treturn DiffReport{}, fmt.Errorf(\"stage %q not found in either run (have: %s)\", opts.StageFilter, strings.Join(known, \", \"))",
		Replace:  "\t\t\t_ = known",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestCompare_StageFilterNoMatchIsAnError"},
		Why: "`vamp diff a b --stage scrpt` printed the headers, zero stage blocks and exited 0 — " +
			"indistinguishable from \"the two runs agree about that stage\". The sibling command " +
			"(RenderStageForPipeline) already refuses the same typo with the ids it does know.",
	},
	{
		Name:     "vamp/a-mermaid-label-can-inject-flowchart-syntax",
		File:     "internal/vamp/viz.go",
		Find:     "\tif !labelNeedsQuoting(s) {",
		Replace:  "\tif !strings.ContainsAny(s, \":|\\\"()[]\") {",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestRenderMermaid_LabelCannotInjectFlowchartSyntax"},
		Why: "restores the denylist the allowlist replaced. It held none of the characters that inject " +
			"STRUCTURE (- > < { } # ; &), and Stage.Capability — which goes straight into the label — is " +
			"never charset-validated: Validate only requires it to be non-empty. Pipeline yaml here is " +
			"model-generated, so `capability: \"a-->b\"` emitted an unquoted node that Mermaid renders " +
			"as an extra edge in the operator's diagram.",
	},
	{
		Name:     "vamp/a-quoted-mermaid-label-escapes-with-backslashes",
		File:     "internal/vamp/viz.go",
		Find:     "\tesc = strings.ReplaceAll(esc, `\"`, \"#quot;\")",
		Replace:  "\tesc = strings.ReplaceAll(esc, `\"`, `\\\"`)",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestRenderMermaid_QuotedLabelUsesMermaidEntitiesNotBackslashes"},
		Why: "Mermaid has no backslash escape inside a quoted label — the entity is #quot;. Emitting " +
			"\\\" produced exactly the parse error the over-quoting policy exists to prevent, so the " +
			"one label Mermaid could not render was the one the guard had already decided was dangerous.",
	},
	{
		Name:     "vamp/the-dry-run-elision-footer-counts-items-it-printed",
		File:     "internal/vamp/dryrun.go",
		Find:     "\tif elided := len(items) - printed; elided > 0 {",
		Replace:  "\tif elided := len(items) - maxItemsToPrint; len(items) > maxItemsToPrint {",
		Pkg:      "./internal/vamp/",
		MustFail: []string{"TestDryRunForeachElisionCountMatchesWhatWasElided"},
		Why: "restores the arithmetic the fix replaced. The print loop ALWAYS renders the last item, so " +
			"len(items)-maxItemsToPrint over-counted by one on every foreach past the cap, and at " +
			"exactly cap+1 items it announced an elision when nothing had been elided. The dry run's " +
			"whole job is to tell the operator what the real run will do; a footer that disagrees with " +
			"the list above it is the plan lying about its own contents.",
	},

	// ── the diagnostic command's own claims ───────────────────────────
	//
	// `vibe doctor` is the one command an operator runs BECAUSE they
	// already believe something is wrong, so a false definite costs more
	// here than anywhere else in the CLI. The first four entries pin one
	// cascade: a single unanswered probe of :9001 used to produce three
	// claims — a port conflict, a stopped daemon, and a stolen :9000 —
	// none of which any evidence supported. They are separate rungs
	// because they are separate CLAIMS, made in three different
	// functions, and the pre-fix code made each one independently.
	{
		Name: "doctor/an unanswered control-plane probe is a port thief",
		File: "internal/vibe/cli/cmd_doctor.go",
		// The pre-fix behaviour exactly: every statusFn error is "in use
		// by another process". errors and connect stay referenced,
		// because a mutation that does not compile proves nothing.
		Find: "\tcase errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):\n" +
			"\t\treturn probeNoAnswer\n" +
			"\tcase connect.CodeOf(err) == connect.CodeDeadlineExceeded:\n" +
			"\t\treturn probeNoAnswer\n" +
			"\tcase connect.CodeOf(err) == connect.CodeUnauthenticated:\n" +
			"\t\treturn probeRefusedUs\n",
		Replace: "\tcase errors.Is(err, context.DeadlineExceeded), connect.CodeOf(err) == connect.CodeUnknown:\n" +
			"\t\treturn probeNotVibe\n",
		Pkg: "./internal/vibe/cli/",
		MustFail: []string{
			"TestCheckControlPlanePort_SlowDaemonIsNotAPortThief",
			"TestCheckControlPlanePort_ARefusedCredentialIsNotAPortConflict",
			"TestDoctorCascade_OneSlowProbeDoesNotProduceThreeClaims",
		},
		Why: "the daemon this probe asks about is holding a model and serving the proxy, so it can " +
			"miss a one-second window with nothing whatever wrong — and the row answered `FAIL: in " +
			"use by another process`, on the command someone opened because they already suspected " +
			"a problem. It sends them hunting a port conflict that does not exist. The second named " +
			"test is the other half and it is NOT a duplicate: a 401 means the holder ANSWERED and " +
			"refused this box's credential, which is `vibe fleet doctor`'s C15 case — the remedy is " +
			"a token, and \"another process\" points at the wrong thing entirely.",
	},
	{
		Name: "doctor/an unattributed :9000 holder is reported as a thief",
		File: "internal/vibe/cli/cmd_doctor.go",
		Find: "\t\treturn checkResult{\n\t\t\tName:   name,\n\t\t\tStatus: statusUnknown,\n" +
			"\t\t\tMessage: \"in use, holder unattributed: :9001 did not identify itself (see the control-plane row), \" +\n" +
			"\t\t\t\t\"so this may well be vibe's own proxy\",\n\t\t}",
		Replace: "\t\treturn checkResult{\n\t\t\tName:    name,\n\t\t\tStatus:  statusFail,\n" +
			"\t\t\tMessage: \"in use by another process (no vibe daemon detected on :9001)\",\n\t\t}",
		Pkg:      "./internal/vibe/cli/",
		MustFail: []string{"TestCheckProxyPortAt_AnUnattributedHolderIsNotAThief"},
		Why: "the cascade's most expensive claim: :9000 is the port the model is SERVED on, so \"in " +
			"use by another process\" reads as \"something has stolen your inference port\". It was " +
			"printed from a bool that had merely never been set — and when :9001 does not identify " +
			"itself, the likeliest holder of :9000 is vibe's own proxy. The genuine conflict (a " +
			"control plane that answered and is not vibe's) still FAILs here, which is the half a " +
			"vaguer fix would have thrown away.",
	},
	{
		Name: "doctor/a daemon nobody could reach is reported as stopped",
		File: "internal/vibe/cli/cmd_doctor.go",
		// Only the two fields, so the `default:` case still ENDS in a
		// return: a switch whose last case falls through the end is not a
		// terminating statement, and `missing return` is a build failure,
		// which proves nothing.
		Find:     "\t\t\tStatus: statusUnknown,\n\t\t\tMessage: \"could not tell — the control-plane row above says why. \" +\n\t\t\t\t\"`not running` is a claim, and nothing here supports it\",\n",
		Replace:  "\t\t\tStatus: statusInfo,\n\t\t\tMessage: \"not running\",\n",
		Pkg:      "./internal/vibe/cli/",
		MustFail: []string{"TestDaemonRow_AnUnansweredProbeIsNotAStoppedDaemon"},
		Why: "`daemon — not running` is a definite statement about a process, and it was printed " +
			"from the same never-set flag. An operator reading it stops looking for the daemon and " +
			"starts looking for what killed it — while it is up with a model resident. This is " +
			"daemonAbsent's class (client.go) one transport over, which is why the fix is a " +
			"three-valued presence rather than a better sentence.",
	},
	{
		Name:     "doctor/an incomplete report exits 0",
		File:     "internal/vibe/cli/cmd_doctor.go",
		Find:     "\tif anyUnknown {\n\t\treturn errDoctorLevel{doctorExitUnknown}\n\t}\n",
		Replace:  "\t_ = anyUnknown\n",
		Pkg:      "./internal/vibe/cli/",
		MustFail: []string{"TestDoctorOutcome_AnIncompleteReportIsNotAPassAndNotAFailure"},
		Why: "the exit status is what a wrapper reads, and \"this box is broken\" and \"I could not " +
			"find out\" are different facts — the distinction `vibe fleet doctor` has encoded as " +
			"exit 3 since C13 and this command was collapsing. Under the mutation a doctor that " +
			"could not evaluate the daemon, the control-plane port and the proxy port exits 0, and " +
			"`vibe doctor && vibe start` proceeds on a report that established nothing.",
	},
	{
		Name:     "doctor/a port we were not allowed to bind is reported as in use",
		File:     "internal/vibe/cli/cmd_doctor.go",
		Find:     "\t\tcase isAddrInUse(err):\n\t\t\tbound = append(bound, label)\n",
		Replace:  "\t\tcase err != nil:\n\t\t\tbound = append(bound, label)\n",
		Pkg:      "./internal/vibe/cli/",
		MustFail: []string{"TestCheckCommonPorts_APortWeCouldNotProbeIsNotReportedAsInUse"},
		Why: "isAddrInUse has three call sites and this was the one that never asked it: `if ok, _ " +
			":= tryBind(...); ok` discarded the error, so EVERY listen failure became \"in use\". " +
			"Measured: a declared loopback port under 1024 fails with `bind: permission denied` as " +
			"an ordinary user, and the row whose entire job is telling an operator which ports are " +
			"taken reported it as taken. The guard-on-some-call-sites shape, in the same file as " +
			"the cascade above.",
	},
	{
		Name:    "doctor/a failed `hf auth whoami` is reported as a login",
		File:    "internal/vibe/cli/cmd_doctor.go",
		Find:    "\tif err != nil {\n\t\twhy := \"failed: \" + err.Error()\n",
		Replace: "\tif err != nil && false {\n\t\twhy := \"failed: \" + err.Error()\n",
		Pkg:     "./internal/vibe/cli/",
		MustFail: []string{
			"TestCheckHFAuth_AFailedWhoamiIsNotALogin",
			"TestCheckHFAuth_OurTimeoutIsNotTheToolsFailure",
		},
		Why: "the run error was discarded outright (`out, _ :=`) and firstNonEmptyLine of a FAILED " +
			"run went straight into \"logged in as …\". Measured: a command that prints a traceback " +
			"and exits 1 rendered as `[ OK ] hf auth  logged in as Traceback (most recent call " +
			"last):`. An OK is the strongest claim this report makes and it was being manufactured " +
			"out of an error message — the box then fails on the first gated pull with nothing in " +
			"the doctor output to have warned it.",
	},
	{
		Name:    "doctor/our own deadline is reported as the tool's failure",
		File:    "internal/vibe/cli/cmd_doctor.go",
		Find:    "func runTimedOut(ctx context.Context, budget time.Duration) string {\n\tswitch {\n",
		Replace: "func runTimedOut(ctx context.Context, budget time.Duration) string {\n\treturn \"\"\n\tswitch {\n",
		Pkg:     "./internal/vibe/cli/",
		MustFail: []string{
			"TestCheckGPU_AHungNvidiaSmiIsNotAFailedOne",
			"TestCheckDockerForProfiles_OurTimeoutDoesNotClaimTheDaemonIsDown",
			"TestCheckLlamaVersion_OurTimeoutIsNotANonZeroExit",
			"TestCheckHFAuth_OurTimeoutIsNotTheToolsFailure",
		},
		Why: "exec.CommandContext kills the child and reports `signal: killed`, and " +
			"errors.Is(err, context.DeadlineExceeded) is FALSE for it — measured — so the error " +
			"alone cannot tell these four rows apart and the CONTEXT has to be asked. Without it a " +
			"wedged nvidia-smi, which is the single most recognisable symptom of a GPU in a bad " +
			"state and HANGS rather than exits, reads as \"nvidia-smi failed: signal: killed\", and " +
			"a docker daemon that is merely slow reads as \"(daemon not running?)\". Four tests " +
			"because the helper is one line and the four call sites are where it is not asked.",
	},
	{
		Name:     "search/the operator's upstream URL rides out in the error",
		File:     "internal/vibe/search/free.go",
		Find:     "\t\treturn nil, fmt.Errorf(\"searxng: %w\", causeWithoutURL(err))",
		Replace:  "\t\treturn nil, fmt.Errorf(\"searxng: %w\", err)",
		Pkg:      "./internal/vibe/search/",
		MustFail: []string{"TestSearxngSearchErrorDoesNotCarryTheOperatorsUpstream"},
		Why: "the tenth site, and the only URL in this package that is not the caller's: " +
			"--search-upstream, which the shipped zero-cost deployment points at a SearXNG " +
			"container on a private network and a basic-auth-fronted instance carries userinfo in. " +
			"net/http embeds it structurally in *url.Error and stripPassword replaces the password " +
			"and nothing else, so `%w` published the scheme, the internal host, the port, the " +
			"username, the operator's path and the CALLER's query text to three audiences at once: " +
			"the journal, the 502 body GET /search hands anyone holding the bearer token, and the " +
			"MCP tool result that lands in a model's transcript.",
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
