package fleetapi

// Derived display states (design doc §4 table), computed at read time from
// the three ownership axes — never stored: availability is observed,
// intent is declared, residency is llama-swap's. The DRAINED? form is a
// question for a human, never a trigger for automation.
const (
	DisplayServing      = "SERVING"
	DisplayDrained      = "DRAINED"
	DisplayDrainedQ     = "DRAINED?"
	DisplayOff          = "OFF"
	DisplayOffAway      = "OFF/AWAY"
	DisplayOffAwayQ     = "OFF/AWAY?"
	DisplayInconsistent = "INCONSISTENT"
)

// displayState derives the per-cell display state from availability
// (observed host/cell reachability) and declared intent. Precedence
// follows the design table: a responding cell is proof of life and beats
// a disagreeing host probe; drained intent + silent host reads as OFF
// (it was drained first); without a host_probe the host/cell distinction
// is unknowable and the display says so.
func displayState(hostReachable *bool, cellUp bool, intent *Intent) string {
	drained := intent != nil && intent.State == "drained"
	switch {
	case cellUp && drained:
		// Intent says drained but the cell answers — a resume forgot to
		// clear intent. Nag until cleared.
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
		// Host up, cell down, no intent: deliberate stop or crash loop.
		return DisplayDrainedQ
	default:
		return DisplayOffAway
	}
}
