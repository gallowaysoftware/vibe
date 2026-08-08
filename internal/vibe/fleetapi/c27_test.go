package fleetapi

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
)

// C27 gives a cell unit's own stop record (C24) its own display state,
// STOPPED. The change is a RENAME of one cell of the derivation table,
// and this file's job is to prove that it is only that.
//
// The hazard it is written against: absentAlarm's switch ends in
// `default: return "", false` — no alarm. A new "down" state that is not
// named there silently stops paging for an always_on cell that went
// away, which is precisely the incident C9's notifier exists for. The
// doctor's intent-hygiene switch has the same shape, and so does the
// page's badgeClass chain, whose fall-through renders `b-off`.

// ─── the reference implementation: main at 89f15c5 ──────────────────────

// displayStateBeforeC27 is `displayState` as it stood on main before this
// phase, verbatim. It is the "what did the fleet show yesterday" half of
// the invariant below: every alarm and doctor outcome computed from the
// state THIS function returns must equal the one computed from the state
// the production function returns today.
func displayStateBeforeC27(hostReachable *bool, cellUp bool, intent *Intent) string {
	drained := intent != nil && intent.State == "drained"
	switch {
	case cellUp && drained:
		return DisplayInconsistent
	case cellUp:
		return DisplayServing
	case drained && hostReachable != nil && !*hostReachable:
		return DisplayOff
	case drained:
		return DisplayDrained
	case hostReachable == nil:
		return DisplayOffAwayQ
	case *hostReachable:
		return DisplayDrainedQ
	default:
		return DisplayOffAway
	}
}

// absentAlarmBeforeC27 is `absentAlarm`'s PREDICATE as it stood on main,
// verbatim minus the detail prose. Comparing the production function
// against it (rather than only against itself fed two displays) is what
// catches an edit to one of the arms this phase was not supposed to
// touch.
func absentAlarmBeforeC27(class, display string, intent *Intent) bool {
	if class != string(fleetcfg.ClassAlwaysOn) {
		return false
	}
	stopRecord := IsStopRecord(intent)
	switch display {
	case DisplayDrainedQ, DisplayOffAway, DisplayOffAwayQ:
	case DisplayDrained, DisplayOff:
		if !stopRecord {
			return false
		}
	default:
		return false
	}
	return true
}

// ─── the enumeration ────────────────────────────────────────────────────

type c27Combo struct {
	class      string
	classLabel string
	host       *bool
	hostLabel  string
	cellUp     bool
	intent     *Intent
	intentName string
}

// c27Combos is class × availability × intent × reason, exhaustively:
// three classes, three host-probe outcomes (up / down / no probe), the
// cell answering or not, and six intent-store shapes including the two
// reserved reasons. 3 × 3 × 2 × 6 = 108.
func c27Combos() []c27Combo {
	since := time.Date(2026, 8, 8, 18, 33, 59, 0, time.UTC)
	up, down := true, false
	intents := []struct {
		name string
		in   *Intent
	}{
		{"no entry", nil},
		{"declared drain with a reason", &Intent{State: "drained", Reason: "gaming", ETA: "23:00", Since: since}},
		{"declared drain with no reason", &Intent{State: "drained", Since: since}},
		{"C24 stop record", &Intent{State: "drained", Reason: StopIntentReason, Since: since}},
		{"C14 sleep record", &Intent{State: "drained", Reason: SleepIntentReason, ETA: "07:15", Since: since}},
		{"serving request", &Intent{State: "serving", Since: since}},
	}
	hosts := []struct {
		name string
		p    *bool
	}{{"host up", &up}, {"host down", &down}, {"no host probe", nil}}
	classes := []string{
		string(fleetcfg.ClassAlwaysOn),
		string(fleetcfg.ClassOpportunistic),
		string(fleetcfg.ClassRoaming),
	}

	var out []c27Combo
	for _, cl := range classes {
		for _, h := range hosts {
			for _, cellUp := range []bool{true, false} {
				for _, in := range intents {
					cell := "cell answers"
					if !cellUp {
						cell = "cell silent"
					}
					out = append(out, c27Combo{
						class: cl, classLabel: cl, host: h.p, hostLabel: h.name + ", " + cell,
						cellUp: cellUp, intent: in.in, intentName: in.name,
					})
				}
			}
		}
	}
	return out
}

// TestC27DisplayOnlyChangeLeavesEveryAlarmAndDoctorOutcomeIdentical is
// the governing invariant of this phase, and the only test in it that
// can fail for a reason worth a rollback.
//
// For all 108 combinations of class × availability × intent × reason it
// computes the display TWICE — once with main's derivation, once with
// this phase's — and then runs the real `absentAlarm` and the real
// `checkIntentHygiene` on both. The alarm's firing decision must be
// identical, and so must the doctor's level and bucket membership. (The
// notify KEY is `kind + "\x00" + scope` and neither is derived from the
// display, so no detail text can move dedup, re-fire or resolve;
// `TestC27StoppedAlarmsForAnAlwaysOnCell` walks the whole
// SetIntent → decorate → notifyConditions path and names the key it
// gets.) The operator PROSE is allowed to differ in exactly one
// way: where the display word itself appears in it, because that word is
// the change. Substituting the old state name for the new one must make
// the two strings equal — anything else is a behaviour change wearing a
// rename's clothes.
func TestC27DisplayOnlyChangeLeavesEveryAlarmAndDoctorOutcomeIdentical(t *testing.T) {
	combos := c27Combos()
	if len(combos) != 108 {
		t.Fatalf("enumeration is %d combinations, want 108 — the table this test claims to cover changed", len(combos))
	}
	changed := 0
	for _, c := range combos {
		name := c.classLabel + " / " + c.hostLabel + " / " + c.intentName
		before := displayStateBeforeC27(c.host, c.cellUp, c.intent)
		after := displayState(c.host, c.cellUp, c.intent)
		if before != after {
			changed++
		}

		snapBefore := CellSnapshot{Name: "heavy", Class: c.class, Display: before, Intent: c.intent}
		snapAfter := CellSnapshot{Name: "heavy", Class: c.class, Display: after, Intent: c.intent}

		// 1. the alarm fires, or does not, exactly as it did.
		detailBefore, firedBefore := absentAlarm(snapBefore)
		detailAfter, firedAfter := absentAlarm(snapAfter)
		if firedBefore != firedAfter {
			t.Errorf("%s: alarm %v → %v (display %s → %s). This is the incident C9 exists for.",
				name, firedBefore, firedAfter, before, after)
		}
		// 1b. …and main's own predicate agrees with today's function on
		// the state main would have shown, so no untouched arm moved.
		if want := absentAlarmBeforeC27(c.class, before, c.intent); want != firedBefore {
			t.Errorf("%s: absentAlarm on main's display = %v, main's predicate said %v", name, firedBefore, want)
		}
		if got := swapDisplayWord(detailBefore, before, after); got != detailAfter {
			t.Errorf("%s: alarm detail changed by more than the state's name:\n  before: %q\n  after:  %q", name, detailBefore, detailAfter)
		}

		// 2. the doctor reaches the same verdict and the same bucket.
		levelBefore, detBefore := c27Hygiene(t, snapBefore)
		levelAfter, detAfter := c27Hygiene(t, snapAfter)
		if levelBefore != levelAfter {
			t.Errorf("%s: intent.hygiene %s → %s (display %s → %s)", name, levelBefore, levelAfter, before, after)
		}
		if got := swapDisplayWord(detBefore, before, after); got != detAfter {
			t.Errorf("%s: intent.hygiene detail changed by more than the state's name:\n  before: %q\n  after:  %q", name, detBefore, detAfter)
		}
	}
	// A control on the enumeration itself: if NOTHING moved, the test
	// above is asserting that a no-op is a no-op.
	if changed == 0 {
		t.Fatal("no combination changed display — this test proves nothing about a rename that did not happen")
	}
	t.Logf("%d/%d combinations changed display; 0 changed an alarm or doctor outcome", changed, len(combos))
}

// c27Hygiene runs the REAL intent-hygiene check over one hand-built
// snapshot. Hand-built on purpose: the doctor consumes a document, and
// this phase's defensive arms exist for a document the local deriver
// would not produce.
func c27Hygiene(t *testing.T, c CellSnapshot) (Level, string) {
	t.Helper()
	s := &Server{}
	rep := &DoctorReport{}
	s.checkIntentHygiene(rep, StateSnapshot{Cells: []CellSnapshot{c}})
	for _, chk := range rep.Checks {
		if chk.ID == "intent.hygiene" {
			return chk.Level, chk.Detail
		}
	}
	t.Fatalf("no intent.hygiene check for %+v", c)
	return "", ""
}

// swapDisplayWord replaces the old display state's name with the new one
// wherever it appears as a whole word, so the comparison above measures
// everything about the sentence EXCEPT the word this phase renamed.
func swapDisplayWord(s, before, after string) string {
	if before == after || s == "" {
		return s
	}
	return strings.ReplaceAll(s, before, after)
}

// ─── the derivation, stated as a table ──────────────────────────────────

// TestC27StoppedIsItsOwnDisplayState is the phase's actual deliverable,
// including the case that was DECIDED rather than derived: a stop record
// whose host has also gone silent stays OFF, because the box being gone
// dominates the stack being gone.
func TestC27StoppedIsItsOwnDisplayState(t *testing.T) {
	up, down := true, false
	stop := &Intent{State: "drained", Reason: StopIntentReason, Since: time.Now().UTC()}
	human := &Intent{State: "drained", Reason: "gaming", ETA: "23:00", Since: time.Now().UTC()}
	for _, tc := range []struct {
		name   string
		host   *bool
		cellUp bool
		in     *Intent
		want   string
	}{
		{"stop record, host up, cell silent", &up, false, stop, DisplayStopped},
		{"stop record, no host probe, cell silent", nil, false, stop, DisplayStopped},
		{"stop record, host down: the box being gone dominates", &down, false, stop, DisplayOff},
		{"stop record but the cell answers (C24 §7)", &up, true, stop, DisplayInconsistent},
		{"a human's declaration is still DRAINED", &up, false, human, DisplayDrained},
		{"nothing recorded is still the question", &up, false, nil, DisplayDrainedQ},
	} {
		if got := displayState(tc.host, tc.cellUp, tc.in); got != tc.want {
			t.Errorf("%s: display = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// TestC27StoppedAlarmsForAnAlwaysOnCell is the hazard, asserted head-on.
// `systemctl stop` and a crash-stop fire the identical ExecStopPost, so
// the state that records one must page for an always_on cell — and must
// still not page for the two classes whose absence is normal.
func TestC27StoppedAlarmsForAnAlwaysOnCell(t *testing.T) {
	stop := &Intent{State: "drained", Reason: StopIntentReason, Since: time.Now().UTC()}
	detail, fired := absentAlarm(CellSnapshot{Name: "heavy",
		Class: string(fleetcfg.ClassAlwaysOn), Display: DisplayStopped, Intent: stop})
	if !fired {
		t.Fatal("an always_on cell whose unit recorded its own stop did not alarm — " +
			"the notifier is muted for exactly the incident it exists for")
	}
	for _, want := range []string{"STOPPED", "nothing recorded why"} {
		if !strings.Contains(detail, want) {
			t.Errorf("alarm detail = %q, want it to contain %q", detail, want)
		}
	}
	for _, class := range []fleetcfg.Class{fleetcfg.ClassOpportunistic, fleetcfg.ClassRoaming} {
		if _, fired := absentAlarm(CellSnapshot{Name: "gpu-cell",
			Class: string(class), Display: DisplayStopped, Intent: stop}); fired {
			t.Errorf("%s absence alarmed: the class table's alarm column is the whole policy", class)
		}
	}

	// End to end, through the real path a fleetd walks: the stop record is
	// written by the endpoint's own writer, `decorate` derives the
	// display, and `notifyConditions` — not absentAlarm directly — is what
	// the evaluator calls. Asserting the KEY, because kind+scope is what
	// dedup, re-fire and resolve are keyed on.
	s := notifyServer(t, []Cell{{Name: "front", URL: "http://127.0.0.1:1", Class: "always_on"}})
	if _, err := s.SetIntent("front", "drained", StopIntentReason, ""); err != nil {
		t.Fatal(err)
	}
	cell := snapCell(s, "front", false, boolp(true))
	if cell.Display != DisplayStopped {
		t.Fatalf("display = %s, want STOPPED — the rest of this proves nothing", cell.Display)
	}
	got := kinds(s.notifyConditions(StateSnapshot{Cells: []CellSnapshot{cell}}))
	if len(got) != 1 || got[0] != "cell_absent/front" {
		t.Errorf("conditions for a STOPPED always_on cell = %v, want exactly [cell_absent/front]", got)
	}
}

// TestC27DoctorKeepsAStoppedCellInTheUndeclaredBucket covers both routes
// into that bucket: the C24 branch that keys on the RECORD, and the
// display arm that exists only so a STOPPED cell arriving without one
// cannot fall through `default` and vanish from the report.
func TestC27DoctorKeepsAStoppedCellInTheUndeclaredBucket(t *testing.T) {
	stop := &Intent{State: "drained", Reason: StopIntentReason,
		Since: time.Now().UTC().Add(-2 * staleRequestAge)}
	for _, tc := range []struct {
		name string
		snap CellSnapshot
		want string
	}{
		{"with its record", CellSnapshot{Name: "heavy", Class: string(fleetcfg.ClassAlwaysOn),
			Display: DisplayStopped, Intent: stop}, "recorded by its own unit stop"},
		{"display only, no record (defensive)", CellSnapshot{Name: "heavy",
			Class: string(fleetcfg.ClassAlwaysOn), Display: DisplayStopped}, "heavy STOPPED"},
	} {
		level, detail := c27Hygiene(t, tc.snap)
		if level != LevelWarn {
			t.Errorf("%s: intent.hygiene = %s, want warn", tc.name, level)
		}
		if !strings.Contains(detail, "undeclared state") || !strings.Contains(detail, tc.want) {
			t.Errorf("%s: detail = %q, want an undeclared-state line containing %q", tc.name, detail, tc.want)
		}
	}
}

// ─── the enumerating consumers ──────────────────────────────────────────

// TestC27EveryDisplayStateIsNamedByEveryEnumeratingConsumer is the
// generalisation of this phase's hazard, and it is written to fail for a
// NINTH state nobody has invented yet.
//
// Three surfaces enumerate the set from memory: the page's badgeClass
// chain (whose fall-through is `b-off` — a state it has not heard of
// renders as if the box were merely off), the CSS that chain names, and
// the MCP tool description an agent reads to know what a row means. The
// list they are checked against is `DisplayStates`, and `DisplayStates`
// is checked against the constants in the source file — so the loop is
// closed at the declaration rather than at somebody's memory.
func TestC27EveryDisplayStateIsNamedByEveryEnumeratingConsumer(t *testing.T) {
	src, err := os.ReadFile("display.go")
	if err != nil {
		t.Fatal(err)
	}
	declared := regexp.MustCompile(`(?m)^\tDisplay\w+\s+= "([^"]+)"$`).FindAllStringSubmatch(string(src), -1)
	if len(declared) < 8 {
		t.Fatalf("scanned %d display constants out of display.go; the declaration shape changed and this "+
			"guard is now measuring nothing", len(declared))
	}
	var fromSource []string
	for _, m := range declared {
		fromSource = append(fromSource, m[1])
	}
	if got, want := sorted(DisplayStates), sorted(fromSource); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("DisplayStates = %v, but display.go declares %v — every enumerating consumer is "+
			"checked against DisplayStates, so a state missing from it is a state nothing checks", got, want)
	}

	page, err := fleetPageFS.ReadFile("fleet.html")
	if err != nil {
		t.Fatal(err)
	}
	badge := string(page)
	start := strings.Index(badge, "function badgeClass(")
	if start < 0 {
		t.Fatal("badgeClass is gone from the page; this guard cannot see what renders a badge")
	}
	end := strings.Index(badge[start:], "\n}")
	if end < 0 {
		t.Fatal("badgeClass has no closing brace where this guard expects one")
	}
	body := badge[start : start+end]
	for _, d := range DisplayStates {
		if !strings.Contains(body, `"`+d+`"`) {
			t.Errorf("badgeClass does not test for %q — it falls through to b-off, so the page renders "+
				"a state it has never heard of as if the box were merely off", d)
		}
	}
	// Every class badgeClass returns must exist in the stylesheet, or the
	// badge renders unstyled — the same defect one step later.
	for _, m := range regexp.MustCompile(`return "(b-[a-z]+)"`).FindAllStringSubmatch(body, -1) {
		if !strings.Contains(badge, "."+m[1]+" {") {
			t.Errorf("badgeClass returns %q and the stylesheet has no rule for it", m[1])
		}
	}
	// And the fall-through must not be one of the known states' classes.
	// It used to be b-off, which meant a state the page had never heard
	// of rendered exactly like a box that is merely off — a confident
	// wrong answer where the honest one is "this build does not know".
	fallthru := regexp.MustCompile(`\n  return "(b-[a-z]+)";`).FindStringSubmatch(body)
	if fallthru == nil {
		t.Fatal("badgeClass has no fall-through return where this guard expects one")
	}
	for _, m := range regexp.MustCompile(`=== "[^"]+"\) return "(b-[a-z]+)"`).FindAllStringSubmatch(body, -1) {
		if m[1] == fallthru[1] {
			t.Errorf("badgeClass falls through to %q, which is also a KNOWN state's class: an "+
				"unrecognised display state renders as one the page understands", fallthru[1])
		}
	}
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
