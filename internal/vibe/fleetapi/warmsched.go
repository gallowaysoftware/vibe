package fleetapi

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
)

// Warm schedules (fleet-control C4 §2): cron-firing model warming,
// moving scheduled warms out of loose host crontabs into fleet config
// where --check and git see them. Two rules are load-bearing: scheduled
// warms reuse the warm-target guard (an unconditional 06:30 landing
// mid-overnight-batch starts the eviction fight §1 exists to prevent),
// and the timezone is declared (the container's TZ env), never assumed —
// fleet_status prints each schedule's resolved next-fire so a wrong
// zone is visible at a glance.
//
// The evaluator is deliberately minimal: the five standard fields,
// minute granularity, stdlib time math. That is all the need is.

// cronSpec is one parsed "min hour dom month dow" schedule. domStar and
// dowStar are the Vixie dom/dow disambiguators and are TEXTUAL (the raw
// field's first byte), not derived from set cardinality: Vixie's entry.c
// sets DOM_STAR/DOW_STAR on any field beginning with "*", so "*/2" is a
// star while "1-31" is not.
type cronSpec struct {
	minute, hour, dom, month, dow cronField
	domStar, dowStar              bool
}

// cronField is one parsed cron field: a set of matching integers.
type cronField map[int]bool

// cronScanMinutes bounds nextFire's minute-by-minute scan. Eight years,
// not four: "0 0 29 2 *" starting 2096-03-01 next fires 2104-02-29,
// because 2100 is not a leap year.
const cronScanMinutes = 8 * 366 * 24 * 60

// parseCron parses the five standard fields: * */n a-b a-b/n x,y,z.
// Day/month NAMES (sun, jan) are deliberately unsupported — this is a
// minimal evaluator, not a Vixie-compatible one.
func parseCron(spec string) (cronSpec, error) {
	fields := strings.Fields(spec)
	if len(fields) != 5 {
		return cronSpec{}, fmt.Errorf("cron %q: want 5 fields (min hour dom month dow), got %d", spec, len(fields))
	}
	// dow accepts 7 as a second spelling of Sunday (Vixie does), folded
	// to 0 below — time.Weekday() never returns 7, so normalising later
	// would silently never match.
	ranges := [][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 7}}
	var c cronSpec
	for i, f := range fields {
		cf, err := parseCronField(f, ranges[i][0], ranges[i][1])
		if err != nil {
			return cronSpec{}, fmt.Errorf("cron %q field %d: %w", spec, i+1, err)
		}
		switch i {
		case 0:
			c.minute = cf
		case 1:
			c.hour = cf
		case 2:
			c.dom = cf
			c.domStar = strings.HasPrefix(f, "*")
		case 3:
			c.month = cf
		case 4:
			if cf[7] {
				cf[0] = true
				delete(cf, 7)
			}
			c.dow = cf
			c.dowStar = strings.HasPrefix(f, "*")
		}
	}
	return c, nil
}

func parseCronField(f string, lo, hi int) (cronField, error) {
	out := cronField{}
	for _, part := range strings.Split(f, ",") {
		if err := parseCronPart(part, lo, hi, out); err != nil {
			return nil, err
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty field %q", f)
	}
	return out, nil
}

func parseCronPart(part string, lo, hi int, out cronField) error {
	step := 1
	if idx := strings.Index(part, "/"); idx >= 0 {
		n, err := strconv.Atoi(part[idx+1:])
		if err != nil || n <= 0 {
			return fmt.Errorf("bad step %q", part)
		}
		step = n
		part = part[:idx]
	}
	var start, end int
	switch {
	case part == "*":
		start, end = lo, hi
	case strings.Contains(part, "-"):
		bounds := strings.SplitN(part, "-", 2)
		var err error
		start, err = strconv.Atoi(bounds[0])
		if err != nil {
			return fmt.Errorf("bad range %q", part)
		}
		end, err = strconv.Atoi(bounds[1])
		if err != nil {
			return fmt.Errorf("bad range %q", part)
		}
	default:
		v, err := strconv.Atoi(part)
		if err != nil {
			return fmt.Errorf("bad value %q", part)
		}
		start, end = v, v
	}
	if start < lo || end > hi || start > end {
		return fmt.Errorf("range %d-%d outside %d-%d", start, end, lo, hi)
	}
	for v := start; v <= end; v += step {
		out[v] = true
	}
	return nil
}

// nextFire returns the first time strictly after `from` matching the
// spec in the given location, evaluating wall-clock fields in that zone.
// A spring-forward gap minute never exists as a wall time and is
// skipped; a fall-back repeated minute exists TWICE in absolute time and
// fires twice — the scan walks absolute minutes, and the schedule's
// in-flight/lease guard makes a duplicate warm harmless. False when
// nothing matches within eight years (an impossible spec; eight years
// covers the 8-year Feb-29 gap around a century non-leap like 2100).
func (c cronSpec) nextFire(from time.Time, loc *time.Location) (time.Time, bool) {
	if loc == nil {
		loc = time.Local
	}
	// Start at the next whole minute.
	t := from.In(loc).Truncate(time.Minute).Add(time.Minute)
	for range cronScanMinutes {
		if c.matches(t) {
			return t.UTC(), true
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}, false
}

func (c cronSpec) matches(t time.Time) bool {
	if !c.minute[t.Minute()] || !c.hour[t.Hour()] || !c.month[int(t.Month())] {
		return false
	}
	// Vixie's find_jobs, byte for byte: when BOTH dom and dow are
	// restricted the fire happens when EITHER matches; if either is a
	// star, both must match (the star matches everything, so this also
	// collapses star&&star correctly).
	if c.domStar || c.dowStar {
		return c.dom[t.Day()] && c.dow[int(t.Weekday())]
	}
	return c.dom[t.Day()] || c.dow[int(t.Weekday())]
}

// warmScheduleState is one schedule's status surface: the resolved
// next-fire (so a wrong TZ is visible) and the last outcome.
type warmScheduleState struct {
	Cron     string     `json:"cron"`
	Model    string     `json:"model"`
	NextFire *time.Time `json:"next_fire,omitempty"`
	LastFire *time.Time `json:"last_fire,omitempty"`
	LastNote string     `json:"last_note,omitempty"`
}

// CellOfModel resolves a model id to the cell that serves it, for the
// eviction-fight guard. The error is what distinguishes resolve FAILURE
// (defs unreadable — guard unavailable, so the warm must not fire) from
// resolved-but-unassigned (a front-only alias, legitimately warmable and
// legitimately unguardable).
type CellOfModel func(model string) (string, error)

// StartScheduleLoop launches the warm-schedule loop with production
// defaults; nil loc means the process TZ.
func (s *Server) StartScheduleLoop(entries []WarmScheduleEntry, cellOfModel CellOfModel, loc *time.Location, frontURL string) {
	s.startScheduleLoopWithConfig(entries, cellOfModel, loc, nil, frontURL)
}

// startScheduleLoopWithConfig is the full seam (tests inject warmFn).
func (s *Server) startScheduleLoopWithConfig(entries []WarmScheduleEntry, cellOfModel CellOfModel, loc *time.Location, warmFn func(ctx context.Context, frontURL, model string) error, frontURL string) {
	if loc == nil {
		loc = time.Local
	}
	if warmFn == nil {
		warmFn = s.warmViaFront
	}
	now := time.Now()
	for _, e := range entries {
		spec, err := parseCron(e.Cron)
		st := &warmScheduleState{Cron: e.Cron, Model: e.Model}
		// A spec can parse and still never fire ("0 0 30 2 *"): the
		// goroutine is gated on BOTH, or it parks forever re-running the
		// full multi-year scan every minute.
		live := err == nil
		refused := s.warmClassRefusal(e.Model)
		switch {
		case refused != "":
			// A model hosts.yaml pins to a non-chat class can never be
			// warmed by this verb, so the entry is inert from wiring —
			// same shape as an invalid cron, and answered FIRST so the
			// status carries no next_fire for a warm that will never
			// happen.
			live = false
			st.LastNote = "refused: " + refused
			slog.Error("warm schedule refused: the warm verb is a chat completion", "cron", e.Cron, "model", e.Model, "why", refused)
		case err != nil:
			st.LastNote = "invalid cron: " + err.Error()
			slog.Warn("warm schedule invalid", "cron", e.Cron, "err", err)
		default:
			next, ok := spec.nextFire(now, loc)
			if ok {
				nextUTC := next
				st.NextFire = &nextUTC
			} else {
				live = false
				st.LastNote = "no fire time within 8 years"
				slog.Warn("warm schedule never fires", "cron", e.Cron, "model", e.Model)
			}
		}
		s.mu.Lock()
		s.schedStates = append(s.schedStates, st)
		s.mu.Unlock()
		if live {
			s.wg.Add(1)
			go s.scheduleEntryLoop(e, spec, st, cellOfModel, loc, warmFn, frontURL)
		}
	}
}

// scheduleEntryLoop ticks every minute and fires when due.
func (s *Server) scheduleEntryLoop(e WarmScheduleEntry, spec cronSpec, st *warmScheduleState, cellOfModel CellOfModel, loc *time.Location, warmFn func(ctx context.Context, frontURL, model string) error, frontURL string) {
	defer s.wg.Done()
	// Align to the next minute boundary, then tick each minute.
	first := time.Now().Truncate(time.Minute).Add(time.Minute)
	timer := time.NewTimer(time.Until(first))
	defer timer.Stop()
	var tick <-chan time.Time
	var ticker *time.Ticker
	tick = timer.C
	for {
		select {
		case <-s.done:
			if ticker != nil {
				ticker.Stop()
			}
			return
		case now := <-tick:
			if ticker == nil {
				ticker = time.NewTicker(time.Minute)
				tick = ticker.C
			}
			s.evalScheduleEntry(e, spec, st, cellOfModel, loc, warmFn, frontURL, now)
		}
	}
}

// evalScheduleEntry fires when due and re-parks the next slot. The
// guard is the eviction-fight rule: skip (with a note) when the target
// cell is mid-work (in-flight) or holds an active lease. A guard that
// cannot be EVALUATED — defs unreadable, in-flight never reported — is a
// skip too: unknown in-flight is not zero in-flight, and one malformed
// def file must not silently convert every scheduled warm into an
// unguarded one.
func (s *Server) evalScheduleEntry(e WarmScheduleEntry, spec cronSpec, st *warmScheduleState, cellOfModel CellOfModel, loc *time.Location, warmFn func(ctx context.Context, frontURL, model string) error, frontURL string, now time.Time) {
	s.mu.Lock()
	next := st.NextFire
	s.mu.Unlock()
	if next == nil || now.Before(*next) {
		return
	}

	note, unguarded := "", ""
	refused := s.warmClassRefusal(e.Model)
	frontAuth, frontBlocked := s.SwapAuthRefusal(fleetcfg.FrontCell)
	cell, err := cellOfModel(e.Model)
	switch {
	case refused != "":
		// Answered ahead of every guard rung and of whatever the resolve
		// returned: this is a permanent configuration refusal, not a
		// condition of this minute, so no rung below it can overwrite it
		// with a reason that reads as temporary.
		note = "refused: " + refused
	case frontBlocked:
		// Second, ahead of the per-cell rungs: the schedule's channel is
		// the front, so a front that will not accept fleetd's credential
		// makes every rung below irrelevant. Naming the busy cell instead
		// would send the operator to the wrong box (C15).
		note = "skipped (" + frontAuth + ")"
	case err != nil:
		note = fmt.Sprintf("skipped (cannot resolve cell for %s: %v)", e.Model, err)
		slog.Warn("warm schedule cell resolve failed — not firing unguarded", "model", e.Model, "err", err)
	case cell == "":
		// A front-only alias with no backend def stays warmable; it just
		// can't be guarded, and the status says so.
		unguarded = " (unguarded: no def/cell)"
		slog.Warn("warm schedule model has no def/cell — firing unguarded", "model", e.Model)
	default:
		n, reported := s.InFlight(cell).Observed()
		switch {
		case !reported:
			note = fmt.Sprintf("skipped (cell %s in-flight unknown — no eviction fight)", cell)
		case n > 0:
			note = fmt.Sprintf("skipped (cell %s has %d in-flight — no eviction fight)", cell, n)
		default:
			if leases := s.leasesForCellActive(cell); len(leases) > 0 {
				note = fmt.Sprintf("skipped (%d active leases on %s)", len(leases), cell)
				// A hold (C11) is one of those leases, and it is a
				// declaration with an expiry — "1 active leases" is not what
				// the operator who declared it needs to read back.
				if h, held := s.HoldOn(cell); held {
					note = fmt.Sprintf("skipped (%s on %s)", holdDetail(h), cell)
				}
			}
		}
	}

	fired := false
	if note == "" {
		var err error
		if cell == "" || s.frontCanRoute(cell) {
			// cell == "" is the front-only alias: the front IS its home,
			// so the front warm is the only channel it has.
			ctx, cancel := s.warmCtx(warmTimeout)
			err = warmFn(ctx, frontURL, e.Model)
			cancel()
		} else {
			err = noFrontRoute(cell)
		}
		// The failure path is the piggyback fallback (C6 MIN-G's
		// remaining half). `fired` stays FALSE when it queues: a queued
		// warm has not warmed anything, and LastFire is the record of a
		// warm that actually happened.
		qnote, qerr := "", error(nil)
		if err != nil {
			qnote, qerr = s.queueWarm(cell, e.Model, err)
		}
		switch {
		case err == nil:
			fired = true
			note = "warmed" + unguarded
			slog.Info("warm schedule fired", "cron", e.Cron, "model", e.Model)
		case qerr != nil:
			note = "warm failed: " + qerr.Error()
			slog.Warn("warm schedule fire failed", "cron", e.Cron, "model", e.Model, "err", qerr)
		default:
			note = "warm " + qnote
			slog.Info("warm schedule piggybacked on the announce", "cron", e.Cron, "model", e.Model, "cell", cell, "err", err)
		}
	}

	next2, ok := spec.nextFire(now, loc)
	s.mu.Lock()
	if fired {
		t := time.Now().UTC()
		st.LastFire = &t
	}
	st.LastNote = note
	if ok {
		n2 := next2
		st.NextFire = &n2
	} else {
		// Terminal: this note must survive, so it lands AFTER the
		// per-fire note rather than being overwritten by it.
		st.NextFire = nil
		st.LastNote = note + "; no further fire time within 8 years"
	}
	s.mu.Unlock()
}

// leasesForCellActive is the schedule guard's lease check (active
// leases naming the cell).
func (s *Server) leasesForCellActive(cell string) []Lease {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Lease
	for _, l := range s.activeLeasesLocked() {
		if l.Cell == cell {
			out = append(out, l)
		}
	}
	return out
}

// WarmScheduleEntry is one cron-firing warm (mirrors the daemon config).
type WarmScheduleEntry struct {
	Cron  string
	Model string
}
