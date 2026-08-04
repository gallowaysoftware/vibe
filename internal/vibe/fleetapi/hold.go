package fleetapi

import (
	"fmt"
	"strings"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
)

// Holds (fleet-control C11): the operator's declaration that fleetd's
// own warm policy must not act on this cell until T. It exists because
// C4's restore-after-idle is CORRECT and its conclusion is still wrong
// on an evaluation afternoon — the evidence for "finished with the
// swap" and "at lunch, mid-evaluation" is identical, so only a
// declaration can separate them.
//
// A hold IS a lease (C11 §1): same store, same (cell, model, holder)
// key, same TTL-at-read expiry, same file, same status surfaces. The
// one added field is Hold, which is what distinguishes a note about
// work in progress ("I am using this") from a policy override ("do not
// act here"). A parallel store would have duplicated every property the
// lease store already has, which is how one system's state ends up in
// another.
//
// A hold is NOT a pin. Residency belongs to llama-swap and its TTL is
// untouched: what a hold buys is that fleetd will not be the one to
// evict, and that nothing fleetd does pulls another model onto that GPU.

const (
	// HoldHolder is the reserved holder every hold is keyed under, so
	// ReleaseHold has a deterministic key and a re-issue refreshes the
	// same entry instead of adding a second one. Who is holding it goes
	// in the note, which every lease surface already prints.
	HoldHolder = "hold"

	// DefaultHoldFor is the evaluation afternoon the backlog entry names.
	DefaultHoldFor = 4 * time.Hour

	// MaxHoldFor is deliberately far below the lease store's 168h bound:
	// a lease is a note about work that is genuinely running, while a
	// hold DISABLES a policy the operator configured, and a forgotten one
	// must not survive a night's sleep. Re-issuing costs one command.
	MaxHoldFor = 24 * time.Hour
)

// validateHoldRequest enforces the hold half of a lease mutation: the
// reserved-holder pairing in both directions and the tighter TTL bound.
// Shared by the HTTP endpoint and the in-process verbs so the two cannot
// drift.
//
// The pairing is enforced BOTH ways on purpose. A hold under another
// holder would be invisible to release_hold; a plain lease squatting on
// the holder `hold` would be deleted by someone else's release.
func validateHoldRequest(hold bool, holder string, ttl time.Duration) error {
	switch {
	case hold && holder != HoldHolder:
		return fmt.Errorf("a hold's holder is always %q (got %q) — the reserved key is what makes release deterministic", HoldHolder, holder)
	case !hold && holder == HoldHolder:
		return fmt.Errorf("holder %q is reserved for holds; set hold: true or pick another holder", HoldHolder)
	case hold && ttl > MaxHoldFor:
		return fmt.Errorf("a hold may not exceed %s (it suspends a configured policy; re-issue instead of forgetting)", MaxHoldFor)
	}
	return nil
}

// SetHold declares a hold on (cell, model) for the given duration,
// returning the stored lease. In-process entry point for the MCP verb;
// the HTTP endpoint reaches the same store through the same putLease.
func (s *Server) SetHold(cell, model, note string, d time.Duration) (Lease, error) {
	cell, model, note = strings.TrimSpace(cell), strings.TrimSpace(model), strings.TrimSpace(note)
	if cell == "" || model == "" {
		return Lease{}, fmt.Errorf("cell and model are required")
	}
	if err := s.checkHoldTarget(cell); err != nil {
		return Lease{}, err
	}
	if d <= 0 {
		return Lease{}, fmt.Errorf("for must be a positive Go duration (e.g. \"4h\")")
	}
	if err := validateHoldRequest(true, HoldHolder, d); err != nil {
		return Lease{}, err
	}
	for label, v := range map[string]string{"model": model, "note": note} {
		if err := clean(label, v); err != nil {
			return Lease{}, err
		}
	}
	l := Lease{
		Cell:      cell,
		Model:     model,
		Holder:    HoldHolder,
		Note:      note,
		Hold:      true,
		ExpiresAt: time.Now().UTC().Add(d),
	}
	if err := s.putLease(l); err != nil {
		return Lease{}, err
	}
	return l, nil
}

// ReleaseHold drops the hold on (cell, model) before its expiry. The
// bool reports whether one was actually there: releasing nothing is not
// an error (release is idempotent), but the caller must be able to say
// so rather than claim it undid something.
func (s *Server) ReleaseHold(cell, model string) (bool, error) {
	cell, model = strings.TrimSpace(cell), strings.TrimSpace(model)
	if cell == "" || model == "" {
		return false, fmt.Errorf("cell and model are required")
	}
	return s.dropLease(cell, model, HoldHolder)
}

// checkHoldTarget refuses the cells a hold cannot mean anything on. The
// front's rendered config is peers-only (C8's probeGuard reason): a hold
// there protects nothing while looking like it does. An unknown cell is
// a typo, and the lease endpoint refuses those too.
func (s *Server) checkHoldTarget(cell string) error {
	if cell == fleetcfg.FrontCell {
		return fmt.Errorf("%s serves no models of its own; hold the cell that holds the model", fleetcfg.FrontCell)
	}
	for _, c := range s.cells {
		if c.Name == cell {
			return nil
		}
	}
	return fmt.Errorf("unknown cell %q (not in the registry)", cell)
}

// HoldOn returns the active hold on a cell, if any. Cell-scoped, not
// model-scoped, and deliberately so: the contended resource is the GPU,
// and a warm of ANY model on that cell makes llama-swap's matrix evict
// the held one. Same rule C10 applied to --idle and C4 to the schedule
// guard. The hold's model is the LABEL on the declaration, not its
// scope.
func (s *Server) HoldOn(cell string) (Lease, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Earliest expiry wins the report so the "N left" an operator reads
	// is the one that runs out first — a longer hold behind it keeps
	// suppressing, and the next evaluation says so.
	var best Lease
	found := false
	for _, l := range s.activeLeasesLocked() {
		if l.Cell != cell || !l.Hold {
			continue
		}
		if !found || l.ExpiresAt.Before(best.ExpiresAt) {
			best, found = l, true
		}
	}
	return best, found
}

// holdDetail is the operator-facing status string for a suppressed warm
// target: what is held and how much longer. fleet_status carries the
// absolute expires_at on the lease itself; this is the surface where the
// question "why has my default not come back?" is actually asked, so it
// answers in words.
func holdDetail(h Lease) string {
	left := time.Until(h.ExpiresAt).Round(time.Second)
	if left < 0 {
		left = 0
	}
	d := fmt.Sprintf("held: %s, %s left", h.Model, left)
	if h.Note != "" {
		d += " (" + h.Note + ")"
	}
	return d
}
