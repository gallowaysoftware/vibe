package fleetapi

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
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

// cronSpec is one parsed "min hour dom month dow" schedule.
type cronSpec struct {
	minute, hour, dom, month, dow cronField
}

// cronField is one parsed cron field: a set of matching integers.
type cronField map[int]bool

// parseCron parses the five standard fields: * */n a-b a-b/n x,y,z.
func parseCron(spec string) (cronSpec, error) {
	fields := strings.Fields(spec)
	if len(fields) != 5 {
		return cronSpec{}, fmt.Errorf("cron %q: want 5 fields (min hour dom month dow), got %d", spec, len(fields))
	}
	ranges := [][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}
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
		case 3:
			c.month = cf
		case 4:
			c.dow = cf
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
// spec in the given location, evaluating wall-clock fields in that zone
// (DST-gap minutes are skipped, repeated minutes fire at the first
// occurrence — standard cron behavior). False when nothing matches
// within four years (an impossible spec; four years covers Feb-29).
func (c cronSpec) nextFire(from time.Time, loc *time.Location) (time.Time, bool) {
	if loc == nil {
		loc = time.Local
	}
	// Start at the next whole minute.
	t := from.In(loc).Truncate(time.Minute).Add(time.Minute)
	for range 4 * 366 * 24 * 60 {
		if c.matches(t) {
			return t.UTC(), true
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}, false
}

func (c cronSpec) matches(t time.Time) bool {
	return c.minute[t.Minute()] &&
		c.hour[t.Hour()] &&
		c.dom[t.Day()] &&
		c.month[int(t.Month())] &&
		c.dow[int(t.Weekday())]
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

// StartScheduleLoop launches the warm-schedule loop with production
// defaults. cellOfModel resolves a model's cell for the eviction-fight
// guard; nil loc means the process TZ.
func (s *Server) StartScheduleLoop(entries []WarmScheduleEntry, cellOfModel func(string) string, loc *time.Location, frontURL string) {
	s.startScheduleLoopWithConfig(entries, cellOfModel, loc, nil, frontURL)
}

// startScheduleLoopWithConfig is the full seam (tests inject warmFn).
func (s *Server) startScheduleLoopWithConfig(entries []WarmScheduleEntry, cellOfModel func(string) string, loc *time.Location, warmFn func(ctx context.Context, frontURL, model string) error, frontURL string) {
	if loc == nil {
		loc = time.Local
	}
	if warmFn == nil {
		warmFn = warmViaFront
	}
	now := time.Now()
	for _, e := range entries {
		spec, err := parseCron(e.Cron)
		st := &warmScheduleState{Cron: e.Cron, Model: e.Model}
		if err != nil {
			st.LastNote = "invalid cron: " + err.Error()
			slog.Warn("warm schedule invalid", "cron", e.Cron, "err", err)
		} else {
			next, ok := spec.nextFire(now, loc)
			if ok {
				nextUTC := next
				st.NextFire = &nextUTC
			} else {
				st.LastNote = "no fire time within a year"
			}
		}
		s.mu.Lock()
		s.schedStates = append(s.schedStates, st)
		s.mu.Unlock()
		if err == nil {
			s.wg.Add(1)
			go s.scheduleEntryLoop(e, spec, st, cellOfModel, loc, warmFn, frontURL)
		}
	}
}

// scheduleEntryLoop ticks every minute and fires when due.
func (s *Server) scheduleEntryLoop(e WarmScheduleEntry, spec cronSpec, st *warmScheduleState, cellOfModel func(string) string, loc *time.Location, warmFn func(ctx context.Context, frontURL, model string) error, frontURL string) {
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
// cell is mid-work (in-flight) or holds an active lease.
func (s *Server) evalScheduleEntry(e WarmScheduleEntry, spec cronSpec, st *warmScheduleState, cellOfModel func(string) string, loc *time.Location, warmFn func(ctx context.Context, frontURL, model string) error, frontURL string, now time.Time) {
	s.mu.Lock()
	next := st.NextFire
	s.mu.Unlock()
	if next == nil || now.Before(*next) {
		return
	}

	note := ""
	cell := cellOfModel(e.Model)
	if cell != "" {
		if n, reported := s.InFlight(cell); reported && n > 0 {
			note = fmt.Sprintf("skipped (cell %s has %d in-flight — no eviction fight)", cell, n)
		} else if leases := s.leasesForCellActive(cell); len(leases) > 0 {
			note = fmt.Sprintf("skipped (%d active leases on %s)", len(leases), cell)
		}
	} else {
		slog.Warn("warm schedule model has no def/cell — firing unguarded", "model", e.Model)
	}

	fired := false
	if note == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		err := warmFn(ctx, frontURL, e.Model)
		cancel()
		if err != nil {
			note = "warm failed: " + err.Error()
			slog.Warn("warm schedule fire failed", "cron", e.Cron, "model", e.Model, "err", err)
		} else {
			fired = true
			note = "warmed"
			slog.Info("warm schedule fired", "cron", e.Cron, "model", e.Model)
		}
	}

	next2, ok := spec.nextFire(now, loc)
	s.mu.Lock()
	if ok {
		n2 := next2
		st.NextFire = &n2
	} else {
		st.NextFire = nil
		st.LastNote = "no further fire time within a year"
	}
	if fired {
		t := time.Now().UTC()
		st.LastFire = &t
	}
	st.LastNote = note
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
