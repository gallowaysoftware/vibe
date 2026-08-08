package fleetapi

// Derived display states (design doc §4 table), computed at read time from
// the three ownership axes — never stored: availability is observed,
// intent is declared, residency is llama-swap's. The DRAINED? form is a
// question for a human, never a trigger for automation, and neither is
// STOPPED: a display state actuates nothing.
const (
	DisplayServing      = "SERVING"
	DisplayDrained      = "DRAINED"
	DisplayStopped      = "STOPPED"
	DisplayDrainedQ     = "DRAINED?"
	DisplayOff          = "OFF"
	DisplayOffAway      = "OFF/AWAY"
	DisplayOffAwayQ     = "OFF/AWAY?"
	DisplayInconsistent = "INCONSISTENT"
)

// DisplayStates is every derived display state, in the design table's
// order. It exists because this set has consumers that must ENUMERATE it
// — the page's badge classes, the MCP tool description an agent reads to
// know what it is looking at — and a consumer that enumerates from memory
// is how a new state ends up rendering as `b-off` and reading as
// "unknown". Those consumers are checked against this list, and this
// list is checked against the constants above by `c27_test.go`, so a
// ninth state cannot be added without every enumerating surface failing
// until it is named.
var DisplayStates = []string{
	DisplayServing,
	DisplayDrained,
	DisplayStopped,
	DisplayDrainedQ,
	DisplayOff,
	DisplayOffAway,
	DisplayOffAwayQ,
	DisplayInconsistent,
}

// displayState derives the per-cell display state from availability
// (observed host/cell reachability) and declared intent. Precedence
// follows the design table: a responding cell is proof of life and beats
// a disagreeing host probe; drained intent + silent host reads as OFF
// (it was drained first); without a host_probe the host/cell distinction
// is unknowable and the display says so.
//
// C27 splits DRAINED in two, because two different facts were sharing
// one word. DRAINED means somebody CHOSE this — an operator or a
// schedule declared it and wrote down why. A cell unit's own stop record
// (C24) proves only that the serving stack stopped: `systemctl stop` and
// a crash-stop fire the identical ExecStopPost, so the record is more
// than DRAINED?'s nothing and less than a declaration. That is STOPPED.
//
// The host-unreachable case deliberately stays OFF. The box being gone
// dominates the stack being gone — it is the fact that decides what the
// operator does next, and OFF already carries the "it was drained first"
// reading, of which a recorded stop is one instance. Nothing loses the
// distinction there: the record itself is still on the snapshot's Intent
// field, which is where `absentAlarm` and the doctor read it.
func displayState(hostReachable *bool, cellUp bool, intent *Intent) string {
	drained := intent != nil && intent.State == "drained"
	// IsStopRecord implies drained, so this case only ever splits the
	// drained region below — it can never invent a new one.
	stopped := IsStopRecord(intent)
	switch {
	case cellUp && drained:
		// Intent says drained but the cell answers — a resume forgot to
		// clear intent. Nag until cleared. A stop record that reads this
		// way is C24 §7's known limitation (an announcer outliving the
		// stack it announces), and INCONSISTENT is still the honest
		// rendering: the cell is observably up.
		return DisplayInconsistent
	case cellUp:
		return DisplayServing
	case drained && hostReachable != nil && !*hostReachable:
		return DisplayOff
	case stopped:
		// The unit recorded its own stop. Nobody said why.
		return DisplayStopped
	case drained:
		return DisplayDrained
	case hostReachable == nil:
		return DisplayOffAwayQ
	case *hostReachable:
		// Host up, cell down, no intent: deliberate stop or crash loop.
		return DisplayDrainedQ
	default:
		return DisplayOffAway
	}
}
