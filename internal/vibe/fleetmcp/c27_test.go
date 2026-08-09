package fleetmcp

import (
	"strings"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
)

// TestC27FleetStatusDescribesEveryDisplayState. The tool description is
// the only place an agent learns what a `display` value means, and an
// agent that has never heard of STOPPED is a real consumer of this
// string — it will either guess or ignore the row. Checked against
// fleetapi.DisplayStates rather than a list in this file, so a ninth
// state fails here until the description names it.
func TestC27FleetStatusDescribesEveryDisplayState(t *testing.T) {
	var desc string
	for _, tool := range (&Server{}).mcpTools() {
		m, ok := tool.(map[string]any)
		if !ok || m["name"] != "fleet_status" {
			continue
		}
		desc, ok = m["description"].(string)
		if !ok {
			t.Fatal("fleet_status has no string description")
		}
	}
	if desc == "" {
		t.Fatal("no fleet_status tool in the facade")
	}
	// The ENUMERATION, not the description as a whole: the prose after it
	// names some of these states again, so a `Contains(desc, …)` over the
	// whole string stays green while the list an agent actually reads
	// loses a state. (Mutation-checked: dropping STOPPED from the list
	// alone did not fail this test until it was scoped this way.)
	open := strings.Index(desc, "per cell (")
	if open < 0 {
		t.Fatal("fleet_status's description no longer enumerates the display states where this guard looks")
	}
	list := desc[open+len("per cell ("):]
	if end := strings.Index(list, ")"); end >= 0 {
		list = list[:end]
	}
	// TOKENS, not substrings, and set equality in both directions.
	//
	// The substring form this replaced was disarmed by the state names
	// themselves: OFF ⊂ OFF/AWAY ⊂ OFF/AWAY?, so a description that had
	// DELETED "OFF" and "OFF/AWAY" from the list still contained both as
	// substrings of the one remaining entry, and this guard stayed green
	// while an agent reading the list had never heard of either. The
	// page's equivalent guard (fleetapi's c27_test.go) has always matched
	// `"OFF"` WITH its quotes for exactly this reason; this is that
	// property, expressed for a prose list.
	//
	// Splitting on " / " rather than "/" is load-bearing: two of the eight
	// states contain a slash of their own.
	got := map[string]bool{}
	for _, tok := range strings.Split(list, " / ") {
		if tok = strings.TrimSpace(tok); tok != "" {
			got[tok] = true
		}
	}
	for _, d := range fleetapi.DisplayStates {
		if !got[d] {
			t.Errorf("fleet_status's state list (%q) does not name %q; an agent reading it cannot "+
				"know what that row means", list, d)
		}
		delete(got, d)
	}
	// The other direction: a name in the list that is not a display state
	// is a state an agent will look for on a row and never find.
	for extra := range got {
		t.Errorf("fleet_status's state list names %q, which is not in fleetapi.DisplayStates", extra)
	}
	// DRAINED? and OFF/AWAY? contain their unqualified forms, so a
	// description naming only the question marks would pass the loop
	// above. Pin the distinction this phase exists to draw instead.
	for _, want := range []string{"somebody chose it", "nothing recorded", "None of the three is a reason to act"} {
		if !strings.Contains(desc, want) {
			t.Errorf("fleet_status's description does not say %q — DRAINED vs STOPPED vs DRAINED? is "+
				"the distinction, not the list", want)
		}
	}
}
