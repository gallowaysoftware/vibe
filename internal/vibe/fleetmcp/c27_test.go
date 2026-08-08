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
	if close := strings.Index(list, ")"); close >= 0 {
		list = list[:close]
	}
	for _, d := range fleetapi.DisplayStates {
		if !strings.Contains(list, d) {
			t.Errorf("fleet_status's state list (%q) does not name %q; an agent reading it cannot "+
				"know what that row means", list, d)
		}
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
