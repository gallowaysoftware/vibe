package fleetapi

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
)

// The usage ledger (fleet-control C7a §5/§6): fleetd folds the cumulative
// per-(model, basis) counters riding each announce into append-only
// per-(day, cell, model, basis, epoch) buckets of RAW COUNTS. No prices,
// no rates, no dollars — storing counts is what lets C7b re-price the
// whole history when a price table changes.
//
// Storage is deliberately NOT history.go's rewrite-the-whole-file-on-
// every-record. That is correct there (starts are minutes apart) and
// wrong here: fleetd folds an announce from every cell every 15s.

// Two bases are fleetd's own rather than a cell's token-semantics
// branch. Keeping them in the same keyspace is what keeps replay
// uniform; giving them their own basis values is what keeps them out of
// every token sum.
const (
	// usageBasisResident carries fleetd-observed residency seconds per
	// model. No tokens.
	usageBasisResident = "resident"
	// usageBasisCell carries cell-level bookkeeping with no model at all
	// (today: lost_rows).
	usageBasisCell = "cell"
)

// reservedBases are fleetd's own basis values, closed to cells. An
// unknown basis from a newer cell is welcome (it is a pricing question,
// C7b's); one of THESE is a collision with a bucket fleetd writes itself.
var reservedBases = map[string]bool{usageBasisResident: true, usageBasisCell: true}

// clampUsage folds a negative counter to zero. The cell already clamps
// llama-swap's -1 sentinels, but the ledger is APPEND-ONLY and fed by
// untrusted announces: a negative that lands can never be corrected, and
// it silently SUBTRACTS from every rollup that later reads the file.
// Clamping the cumulative total (not the delta) is what makes the next
// honest announce produce an honest delta against it.
func clampUsage(m AnnounceUsageModel) AnnounceUsageModel {
	nn := func(v int64) int64 {
		if v < 0 {
			return 0
		}
		return v
	}
	m.Req = nn(m.Req)
	m.InFresh = nn(m.InFresh)
	m.InCached = nn(m.InCached)
	m.Out = nn(m.Out)
	m.PokeReq = nn(m.PokeReq)
	m.ErrReq = nn(m.ErrReq)
	m.UnmeasuredReq = nn(m.UnmeasuredReq)
	m.BusyMS = nn(m.BusyMS)
	return m
}

// UsageBucket is one JSONL line: one (day, cell, model, basis, epoch)
// bucket, plus the cumulative cursor that produced it.
//
// LastTotal lives in the SAME line as the counts so a crash cannot leave
// the cursor and the counter disagreeing — the failure that turns a
// restart into either a lost window or a replayed one.
type UsageBucket struct {
	Day   string `json:"d"`
	TZ    string `json:"tz"`
	Cell  string `json:"cell"`
	Model string `json:"model"`
	Basis string `json:"basis"`
	Epoch string `json:"epoch,omitempty"`

	LastTotal *AnnounceUsageModel `json:"last_total,omitempty"`

	Req           int64 `json:"req"`
	InFresh       int64 `json:"in_fresh"`
	InCached      int64 `json:"in_cached"`
	Out           int64 `json:"out"`
	PokeReq       int64 `json:"poke_req"`
	ErrReq        int64 `json:"err_req"`
	UnmeasuredReq int64 `json:"unmeasured_req"`
	BusyMS        int64 `json:"busy_ms"`
	ResidentS     int64 `json:"resident_s,omitempty"`
	LostRows      int64 `json:"lost_rows,omitempty"`
}

// usageKey identifies a bucket. Epoch is part of the key because a new
// epoch starts a NEW row and keeps the old one accumulated (C7a §5).
type usageKey struct {
	Day   string
	Cell  string
	Model string
	Basis string
	Epoch string
}

func (b *UsageBucket) key() usageKey {
	return usageKey{Day: b.Day, Cell: b.Cell, Model: b.Model, Basis: b.Basis, Epoch: b.Epoch}
}

// cursorKey is the day-independent identity of a cumulative counter.
// The cursor must survive a day rollover: keying it by day would make
// the first fold after local midnight read last_total as zero and count
// the cell's whole lifetime again.
type cursorKey struct {
	Cell  string
	Model string
	Basis string
	Epoch string
}

type usageCursor struct {
	Totals AnnounceUsageModel
	Day    string
}

// usageLedger is the append-only bucket store.
type usageLedger struct {
	mu      sync.Mutex
	path    string
	loc     *time.Location
	buckets map[usageKey]*UsageBucket
	cursors map[cursorKey]usageCursor
	dirty   map[usageKey]bool
	// lastFold is the previous announce instant per cell, the residency
	// credit's denominator.
	lastFold map[string]time.Time
	// warnedBasis dedupes the reserved-basis rejection log. A cell that
	// sends one sends one every 15 s forever, and a warning that repeats
	// forever is a warning nobody reads.
	warnedBasis map[string]bool
	// loadDegraded records that load() could not read the whole file.
	// Compaction rewrites the file from memory, so compacting a partial
	// read would DELETE the part that didn't parse — turning a transient
	// read error into permanent data loss.
	loadDegraded bool
}

// newUsageLedger loads (and compacts) the ledger at path. A missing file
// is an empty ledger; a corrupt LINE is skipped with a warning rather
// than discarding the whole history — JSONL degrades line by line, which
// is most of why it was chosen.
func newUsageLedger(path string, loc *time.Location) *usageLedger {
	if loc == nil {
		loc = time.Local
	}
	l := &usageLedger{
		path:        path,
		loc:         loc,
		buckets:     map[usageKey]*UsageBucket{},
		cursors:     map[cursorKey]usageCursor{},
		dirty:       map[usageKey]bool{},
		lastFold:    map[string]time.Time{},
		warnedBasis: map[string]bool{},
	}
	if path == "" {
		return l
	}
	l.load()
	if l.loadDegraded {
		slog.Warn("usage ledger read was incomplete; skipping compaction so nothing unread is rewritten away", "path", path)
		return l
	}
	if err := l.compact(); err != nil {
		slog.Warn("usage ledger compaction failed; the file keeps its replay history", "path", path, "err", err)
	}
	return l
}

// load replays the file with last-line-wins per key.
func (l *usageLedger) load() {
	f, err := os.Open(l.path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("usage ledger unreadable; starting empty", "path", l.path, "err", err)
			l.loadDegraded = true
		}
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	// Buckets are ~250 B; 1 MB of slack costs nothing and survives a
	// pathological model name.
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	bad := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var b UsageBucket
		if err := json.Unmarshal([]byte(line), &b); err != nil {
			bad++
			continue
		}
		if b.Day == "" || b.Cell == "" {
			bad++
			continue
		}
		cp := b
		l.buckets[b.key()] = &cp
		// Cursor precedence is (day, file order): flushes append in day
		// order, so the last line for the newest day carries the live
		// cumulative total.
		ck := cursorKey{Cell: b.Cell, Model: b.Model, Basis: b.Basis, Epoch: b.Epoch}
		if b.LastTotal != nil {
			if prev, ok := l.cursors[ck]; !ok || b.Day >= prev.Day {
				l.cursors[ck] = usageCursor{Totals: *b.LastTotal, Day: b.Day}
			}
		}
	}
	if err := sc.Err(); err != nil {
		slog.Warn("usage ledger truncated on read; keeping what parsed", "path", l.path, "err", err)
		l.loadDegraded = true
	}
	if bad > 0 {
		// Unparseable LINES are not a degraded read: JSONL degrades line
		// by line by design, and compacting them away is the cleanup.
		slog.Warn("usage ledger had unparseable lines; skipped", "path", l.path, "lines", bad)
	}
}

// fold applies the delta rule per (cell, model, basis, epoch):
//
//   - same epoch, total >= last → add total − last
//   - same epoch, total < last  → treat as a reset, add total, log
//   - new epoch                 → start a new row, keep the old accumulated
//
// No-double-count is a WHITELIST, not a filter: the ledger accepts only
// announce-carried totals, and only cells announce. fleetcfg.FrontCell is
// skipped structurally by name here — the front sees every request that
// any cell also sees, so folding it would double the entire fleet.
func (l *usageLedger) fold(cell string, u *AnnounceUsage, at time.Time) {
	if cell == fleetcfg.FrontCell {
		return
	}
	if u == nil {
		return
	}
	day := dayKey(at, l.loc)
	tz := l.loc.String()

	l.mu.Lock()
	defer l.mu.Unlock()
	for _, m := range u.Models {
		if m.Model == "" {
			continue
		}
		if reservedBases[m.Basis] {
			// An announce is untrusted input (C3/C5's posture — the fleet
			// token is every cell's voice). These two basis values are
			// FLEETD's: a cell-announced "resident" row keys onto exactly
			// the bucket line foldResidency credits seconds to, producing
			// one line that is simultaneously a token row and a residency
			// row with no way to separate the halves afterwards. Drop the
			// entry, keep the rest of the announce.
			if k := cell + "\x00" + m.Basis; !l.warnedBasis[k] {
				l.warnedBasis[k] = true
				slog.Warn("cell announced a fleetd-reserved usage basis; those rows are dropped",
					"cell", cell, "basis", m.Basis, "reserved", []string{usageBasisResident, usageBasisCell})
			}
			continue
		}
		m = clampUsage(m)
		key := usageKey{Day: day, Cell: cell, Model: m.Model, Basis: m.Basis, Epoch: u.Epoch}
		delta := l.deltaLocked(cursorKey{Cell: cell, Model: m.Model, Basis: m.Basis, Epoch: u.Epoch}, m, day)

		b := l.bucketLocked(key, tz)
		b.Req += delta.Req
		b.InFresh += delta.InFresh
		b.InCached += delta.InCached
		b.Out += delta.Out
		b.PokeReq += delta.PokeReq
		b.ErrReq += delta.ErrReq
		b.UnmeasuredReq += delta.UnmeasuredReq
		b.BusyMS += delta.BusyMS
		total := m
		b.LastTotal = &total
		l.dirty[key] = true
	}

	if u.LostRows > 0 {
		// Loss is a property of the cell's READ, not of any one model: it
		// goes on a cell-level row so it can never be mistaken for a
		// model's token count. It runs through the same delta machinery as
		// the token counters — the cell reports it cumulatively, so
		// assigning it instead would make every day after the loss repeat
		// the same number and any cross-day sum wrong.
		synth := AnnounceUsageModel{Basis: usageBasisCell, Req: u.LostRows}
		key := usageKey{Day: day, Cell: cell, Basis: usageBasisCell, Epoch: u.Epoch}
		delta := l.deltaLocked(cursorKey{Cell: cell, Basis: usageBasisCell, Epoch: u.Epoch}, synth, day)
		b := l.bucketLocked(key, tz)
		b.LostRows += delta.Req
		total := synth
		b.LastTotal = &total
		l.dirty[key] = true
	}
}

// deltaLocked resolves one cumulative total against its stored cursor
// and advances the cursor. Callers hold l.mu.
func (l *usageLedger) deltaLocked(ck cursorKey, now AnnounceUsageModel, day string) AnnounceUsageModel {
	prev, known := l.cursors[ck]
	delta := now
	switch {
	case !known:
		// First sighting of this epoch: the whole cumulative total is the
		// delta. A new epoch therefore starts a new row and leaves every
		// earlier epoch's row untouched.
	case usageRegressed(now, prev.Totals):
		slog.Warn("cell usage totals went backwards within an epoch; treating as a counter reset",
			"cell", ck.Cell, "model", ck.Model, "basis", ck.Basis, "epoch", ck.Epoch)
	default:
		delta = usageSub(now, prev.Totals)
	}
	l.cursors[ck] = usageCursor{Totals: now, Day: day}
	return delta
}

// foldResidency credits wall-clock seconds to each model the cell
// announced as resident. The credit is the gap since this cell's last
// announce, CAPPED at the staleness bound: a cell that was off for six
// hours must not have those hours credited to whatever it happened to be
// serving when it left.
//
// The whole gap lands on the day of `at`, so a heartbeat straddling
// local midnight misattributes at most one staleness bound (~50 s at the
// default cadence) to the later day. Splitting it would cost a second
// bucket write per heartbeat to move a rounding error.
func (l *usageLedger) foldResidency(cell string, models []AnnounceModel, intervalS int, at time.Time) {
	if cell == fleetcfg.FrontCell {
		return
	}
	day := dayKey(at, l.loc)
	tz := l.loc.String()

	l.mu.Lock()
	defer l.mu.Unlock()
	last, ok := l.lastFold[cell]
	l.lastFold[cell] = at
	if !ok {
		return
	}
	gap := at.Sub(last)
	if ceiling := staleAfter(intervalS); gap > ceiling {
		gap = ceiling
	}
	if gap <= 0 {
		return
	}
	// Round, don't truncate: heartbeats land on a jittered ~15 s cadence,
	// so truncating each one loses half a second on average — a steady
	// ~3% under-count of residency, which is exactly the kind of quiet
	// bias C7b would then build an energy figure on.
	secs := int64(gap.Round(time.Second).Seconds())
	if secs <= 0 {
		return
	}
	for _, m := range models {
		if m.ID == "" || m.State == "" || m.State == "stopped" {
			continue
		}
		b := l.bucketLocked(usageKey{Day: day, Cell: cell, Model: m.ID, Basis: usageBasisResident}, tz)
		b.ResidentS += secs
		l.dirty[b.key()] = true
	}
}

func (l *usageLedger) bucketLocked(k usageKey, tz string) *UsageBucket {
	if b := l.buckets[k]; b != nil {
		return b
	}
	b := &UsageBucket{Day: k.Day, TZ: tz, Cell: k.Cell, Model: k.Model, Basis: k.Basis, Epoch: k.Epoch}
	l.buckets[k] = b
	return b
}

// usageRegressed reports whether any counter moved backwards. Any single
// field going down means the cell's counters restarted; adding a
// per-field delta then would double-count the fields that did not.
func usageRegressed(now, prev AnnounceUsageModel) bool {
	return now.Req < prev.Req ||
		now.InFresh < prev.InFresh ||
		now.InCached < prev.InCached ||
		now.Out < prev.Out ||
		now.PokeReq < prev.PokeReq ||
		now.ErrReq < prev.ErrReq ||
		now.UnmeasuredReq < prev.UnmeasuredReq ||
		now.BusyMS < prev.BusyMS
}

func usageSub(now, prev AnnounceUsageModel) AnnounceUsageModel {
	return AnnounceUsageModel{
		Model:         now.Model,
		Basis:         now.Basis,
		Req:           now.Req - prev.Req,
		InFresh:       now.InFresh - prev.InFresh,
		InCached:      now.InCached - prev.InCached,
		Out:           now.Out - prev.Out,
		PokeReq:       now.PokeReq - prev.PokeReq,
		ErrReq:        now.ErrReq - prev.ErrReq,
		UnmeasuredReq: now.UnmeasuredReq - prev.UnmeasuredReq,
		BusyMS:        now.BusyMS - prev.BusyMS,
	}
}

// dayKey is the ledger's ONLY day-boundary rule: the calendar date in an
// explicit Location.
//
// Never round a timestamp to a 24-hour boundary to get a day.
// time.Time.Truncate rounds against absolute time since the Unix epoch,
// so it lands on UTC midnight no matter what Location the value carries
// — silently, with no error and no type mismatch. fleetd runs
// containerized and defaults to TZ=UTC, so an evening session would
// split 40/60 across two bars and look like a usage pattern instead of a
// bug. A test greps this package for that idiom.
func dayKey(ts time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.Local
	}
	y, m, d := ts.In(loc).Date()
	return fmt.Sprintf("%04d-%02d-%02d", y, int(m), d)
}

// flush appends every dirty bucket, oldest day first so the cursor
// precedence rule on replay ((day, file order)) holds.
func (l *usageLedger) flush() error {
	l.mu.Lock()
	if l.path == "" || len(l.dirty) == 0 {
		l.mu.Unlock()
		return nil
	}
	keys := make([]usageKey, 0, len(l.dirty))
	for k := range l.dirty {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, compareUsageKey)
	lines := make([][]byte, 0, len(keys))
	for _, k := range keys {
		b := l.buckets[k]
		if b == nil {
			continue
		}
		data, err := json.Marshal(b)
		if err != nil {
			slog.Warn("usage bucket unmarshalable; dropped from this flush", "cell", b.Cell, "model", b.Model, "err", err)
			continue
		}
		lines = append(lines, data)
	}
	path := l.path
	// Marked clean here, re-marked on failure below. A bucket that stayed
	// dirty through a failed write and then never changed again would
	// never reach disk at all — and the buckets most likely to go quiet
	// are exactly the cells that went away.
	clear(l.dirty)
	l.mu.Unlock()

	if len(lines) == 0 {
		return nil
	}
	err := appendLines(path, lines)
	if err != nil {
		l.mu.Lock()
		for _, k := range keys {
			l.dirty[k] = true
		}
		l.mu.Unlock()
	}
	return err
}

func appendLines(path string, lines [][]byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// 0600 matches the intent and lease stores: fleetd-private state,
	// and creating it 0644 here while compaction's tmp+rename creates it
	// 0600 would make the mode depend on which path happened to run
	// first.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, line := range lines {
		if _, err := w.Write(line); err != nil {
			return err
		}
		if err := w.WriteByte('\n'); err != nil {
			return err
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return f.Sync()
}

// compact rewrites the file as one line per key via tmp + rename (the
// repo's write discipline), collapsing the replay history a long-running
// fleetd accumulates. Runs at daemon start, when nothing else holds the
// file.
func (l *usageLedger) compact() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.path == "" || len(l.buckets) == 0 {
		return nil
	}
	keys := make([]usageKey, 0, len(l.buckets))
	for k := range l.buckets {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, compareUsageKey)

	dir := filepath.Dir(l.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".usage-*.jsonl")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	w := bufio.NewWriter(tmp)
	for _, k := range keys {
		data, err := json.Marshal(l.buckets[k])
		if err != nil {
			// Dropping a bucket here deletes it: compaction REPLACES the
			// history rather than appending to it. Say so out loud, the
			// way the append path already does.
			slog.Warn("usage bucket unmarshalable; dropped from the compacted file",
				"cell", k.Cell, "model", k.Model, "basis", k.Basis, "err", err)
			continue
		}
		if _, err := w.Write(append(data, '\n')); err != nil {
			tmp.Close()
			os.Remove(tmpName)
			return err
		}
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, l.path); err != nil {
		os.Remove(tmpName)
		return err
	}
	clear(l.dirty)
	return nil
}

// compareUsageKey orders by day first so a flush appends oldest-day-last
// lines in the order replay expects.
func compareUsageKey(a, b usageKey) int {
	for _, c := range []int{
		strings.Compare(a.Day, b.Day),
		strings.Compare(a.Cell, b.Cell),
		strings.Compare(a.Model, b.Model),
		strings.Compare(a.Basis, b.Basis),
		strings.Compare(a.Epoch, b.Epoch),
	} {
		if c != 0 {
			return c
		}
	}
	return 0
}

// UsageReport is the read surface: raw counts, never money.
type UsageReport struct {
	GeneratedAt time.Time     `json:"generated_at"`
	TZ          string        `json:"tz"`
	SinceDay    string        `json:"since_day,omitempty"`
	Buckets     []UsageBucket `json:"buckets"`
	// Note states the contract in-band so an agent reading this document
	// cannot mistake it for a bill.
	Note string `json:"note"`
}

// usageReportNote is deliberately part of the payload: C7a ships counts,
// and every way the money number can be silly is C7b's problem. It is
// worded without money vocabulary on purpose — a test asserts none
// appears anywhere in the served document.
const usageReportNote = "raw token counts only; this ledger holds no money figures and no rates. poke_req is fleet self-traffic (excluded from req and from every token sum); unmeasured_req counts 200s that reported no tokens and is NEVER summed as zero; err_req counts non-200s. basis says which token-semantics branch produced the numbers: chat=llama.cpp timings (in_fresh is cache-miss only, in_cached is the rest), embed/other=OpenAI usage (in_fresh is the full prompt, in_cached is 0)."

// UsageTZ reports the Location the ledger buckets days in, so a caller
// building a "last N days" window computes the same boundaries the
// ledger did.
func (s *Server) UsageTZ() *time.Location {
	if s.usage == nil {
		return time.Local
	}
	return s.usage.loc
}

// UsageReport returns the ledger contents from sinceDay forward (empty
// sinceDay = everything).
func (s *Server) UsageReport(sinceDay string) UsageReport {
	rep := UsageReport{GeneratedAt: time.Now().UTC(), SinceDay: sinceDay, Note: usageReportNote, Buckets: []UsageBucket{}}
	if s.usage == nil {
		rep.TZ = time.Local.String()
		return rep
	}
	l := s.usage
	rep.TZ = l.loc.String()
	l.mu.Lock()
	defer l.mu.Unlock()
	keys := make([]usageKey, 0, len(l.buckets))
	for k := range l.buckets {
		if sinceDay != "" && k.Day < sinceDay {
			continue
		}
		keys = append(keys, k)
	}
	slices.SortFunc(keys, compareUsageKey)
	for _, k := range keys {
		// Deep-copy LastTotal. A struct copy taken under the lock still
		// hands the caller a POINTER into ledger memory — captured under a
		// lock, dereferenced after it, which is the shape that shipped in
		// C4. Nothing writes through it today; the point is that nothing
		// can start to.
		b := *l.buckets[k]
		if b.LastTotal != nil {
			lt := *b.LastTotal
			b.LastTotal = &lt
		}
		rep.Buckets = append(rep.Buckets, b)
	}
	return rep
}

// usageFlushInterval is the ledger's write cadence. Coalescing in memory
// between flushes is the whole point: fleetd folds an announce from every
// cell every 15s, and a write per fold would be a write amplification of
// roughly the fleet size.
const usageFlushInterval = 60 * time.Second

// usageFlushLoop persists the ledger on a ticker and once more at
// shutdown, so at most one interval of folds is lost to a hard kill (and
// nothing is: the cell's totals are cumulative, so the next announce
// after a restart carries the arrears).
func (s *Server) usageFlushLoop() {
	defer s.wg.Done()
	tick := time.NewTicker(usageFlushInterval)
	defer tick.Stop()
	for {
		select {
		case <-s.done:
			if err := s.usage.flush(); err != nil {
				slog.Warn("usage ledger final flush failed", "err", err)
			}
			return
		case <-tick.C:
			if err := s.usage.flush(); err != nil {
				slog.Warn("usage ledger flush failed", "err", err)
			}
		}
	}
}

// handleUsage serves GET /api/fleet/usage?since=YYYY-MM-DD.
//
// A malformed `since` is a 400, not an empty document. The filter is a
// STRING comparison against the day key, so `since=aug3` silently
// excludes every bucket and `since=1` silently includes all of them —
// either way a typo reads as an answer about the fleet.
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	since := strings.TrimSpace(r.URL.Query().Get("since"))
	if since != "" {
		if _, err := time.Parse("2006-01-02", since); err != nil {
			http.Error(w, "since must be YYYY-MM-DD", http.StatusBadRequest)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.UsageReport(since))
}
