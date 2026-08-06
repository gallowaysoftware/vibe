package fleetapi

import (
	"testing"

	"github.com/gallowaysoftware/vibe/internal/astscan"
)

// fleet-control C20, class 3: a guard that lives in one of N call paths.
//
// C15 wrote one AST scan for one verb and it earned its keep immediately
// — it caught C16's unauthorized /api/version reader at merge time, a
// genuine cross-phase composition failure that no unit test could see.
// The rule engine is now internal/astscan and this is the second
// instance, on the guard that has ALREADY been shipped incomplete once.

// TestEveryWarmProducerConsultsTheClassGuard.
//
// `model_classes` is the declaration that a model id must not receive a
// chat completion. Until the 2026-08-05 live gate only `warm_model`
// honoured it, and a `warm_schedule` fired five 500-ing chat completions
// at an embed-class id and then QUEUED them to the cell, because a 500 is
// a delivery failure. C4 fixed it in every producer; nothing stops the
// next producer from being written without it, and the failure mode is
// silent — an automated policy firing a request that 500s forever.
//
// The rule is deliberately about the FIRE-time rung rather than the
// wiring-time one: `queueWarm` and the two loops also refuse at wiring,
// but the wiring rung is a status row, and the rung that decides whether
// a request leaves the box is this one.
func TestEveryWarmProducerConsultsTheClassGuard(t *testing.T) {
	r := astscan.Rule{
		Name:    "every warm producer consults model_classes (C4)",
		Dir:     ".",
		Trigger: []string{"warmFn", "warmViaFront"},
		Require: []string{"warmClassRefusal", "WarmClassRefusal"},
		// restore (warm targets), evalScheduleEntry (warm schedules),
		// wakeWarm (C14's post-wake warms). fleetmcp's warm_model and the
		// daemon's config load are the other two producers and hold the
		// same rung; they are covered by their own packages' tests.
		MinProducers: 3,
		Because: "every warm in this fleet is a CHAT completion through the front, and hosts.yaml " +
			"pinning an id to a non-chat class is the declaration that it must not receive one. " +
			"A producer that skips the rung fires a request that 500s forever and then queues it " +
			"to the cell (C4's live gate).",
	}
	res, err := r.Check()
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Err(res); err != nil {
		t.Error(err)
	}
}
