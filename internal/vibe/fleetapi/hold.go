package fleetapi

import (
	"fmt"
	"log/slog"
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
	// A typo'd cell must not report "no active hold": that reads as "the
	// hold is already gone" to the operator who typed it, which is the
	// one answer that is definitely wrong. C6's fail-fast-on-unknown-cell
	// rule. The front refusal does NOT apply here — a hold there is
	// impossible to create, so releasing one is a harmless no-op.
	if !s.knownCell(cell) {
		return false, fmt.Errorf("unknown cell %q (not in the registry)", cell)
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
	if !s.knownCell(cell) {
		return fmt.Errorf("unknown cell %q (not in the registry)", cell)
	}
	return nil
}

// knownCell reports registry membership. s.cells is immutable after New
// (membership comes from hosts.yaml at startup), so no lock.
func (s *Server) knownCell(cell string) bool {
	for _, c := range s.cells {
		if c.Name == cell {
			return true
		}
	}
	return false
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
	return s.holdOnLocked(cell)
}

// holdOnLocked is the same read for callers that already hold s.mu.
// drainCommands is one, and HoldOn re-entering there would deadlock —
// the shape this repo has shipped a lock bug in before.
func (s *Server) holdOnLocked(cell string) (Lease, bool) {
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

// dropHeldWarmsLocked removes fleetd's own queued warms from a held
// cell's piggyback queue. Callers hold s.mu.
//
// The warm loops check the hold when they DECIDE, but the piggyback
// queue is at-least-once and delivery happens later: a restore queued
// one tick before the operator declared the hold would still land on the
// cell's next announce and evict exactly the model the hold was taken to
// protect. The sending side guarding and the receiving side not is this
// repo's most repeated defect; the hold has to hold at both ends.
//
// Only `warm` is dropped, and that is not a coincidence: every queued
// warm comes from queueWarm, i.e. from fleetd's OWN policy (the
// warm-target restore and the warm schedule), which is precisely what a
// hold suspends. `unload` is an operator's explicit verb and `probe` can
// be one — "an operator asking is not fleetd guessing", so both still
// deliver. Dropping rather than deferring is deliberate: the restore is
// re-decided every tick once the hold lifts, and a warm delivered hours
// late is a stale decision.
func (s *Server) dropHeldWarmsLocked(cell string) {
	if _, held := s.holdOnLocked(cell); !held {
		return
	}
	drop := func(cmds []AnnounceCommand) ([]AnnounceCommand, int) {
		kept := make([]AnnounceCommand, 0, len(cmds))
		for _, c := range cmds {
			if c.Verb == "warm" {
				continue
			}
			kept = append(kept, c)
		}
		return kept, len(cmds) - len(kept)
	}
	if q, ok := s.commands[cell]; ok {
		if kept, n := drop(q); n > 0 {
			slog.Info("dropped queued warms for a held cell", "cell", cell, "dropped", n)
			if len(kept) == 0 {
				delete(s.commands, cell)
			} else {
				s.commands[cell] = kept
			}
		}
	}
	// The in-flight slot too: an at-least-once batch handed out before
	// the hold is redelivered until a higher seq retires it.
	if pend, ok := s.cmdInflight[cell]; ok {
		if kept, n := drop(pend.cmds); n > 0 {
			slog.Info("dropped undelivered warms for a held cell", "cell", cell, "dropped", n)
			if len(kept) == 0 {
				delete(s.cmdInflight, cell)
			} else {
				pend.cmds = kept
				s.cmdInflight[cell] = pend
			}
		}
	}
}

// holdDetail is the operator-facing status string for a suppressed warm
// target: what is held and how much longer. fleet_status carries the
// absolute expires_at on the lease itself; this is the surface where the
// question "why has my default not come back?" is actually asked, so it
// answers in words.
func holdDetail(h Lease) string {
	d := fmt.Sprintf("held: %s, %s", h.Model, HoldLeft(h.ExpiresAt))
	if h.Note != "" {
		d += " (" + h.Note + ")"
	}
	return d
}

// HoldLeft renders the time remaining on a hold. Exported because the
// CLI's status column and the MCP reply say the same thing, and three
// copies of a duration format is three chances to disagree about what
// the operator is being told. Minute granularity above a minute: a
// countdown that churns every second is a status line nobody can diff.
func HoldLeft(expiresAt time.Time) string {
	left := time.Until(expiresAt)
	if left < time.Minute {
		if left < 0 {
			left = 0
		}
		return fmt.Sprintf("%ds left", int(left.Round(time.Second).Seconds()))
	}
	left = left.Round(time.Minute)
	if h := int(left / time.Hour); h > 0 {
		return fmt.Sprintf("%dh%dm left", h, int((left%time.Hour)/time.Minute))
	}
	return fmt.Sprintf("%dm left", int(left/time.Minute))
}
