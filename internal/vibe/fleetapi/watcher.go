package fleetapi

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/observed"
)

// scannerBufMax sizes the SSE line scanner. llama-swap's initial logData
// frame carries the full log history in one line, which can run to megabytes.
const scannerBufMax = 8 << 20

// watchCell is the per-cell reconnect loop: stream until the cell drops,
// mark it down, back off (doubling to maxBackoff so an absent llama-swap
// never makes the daemon spin hot), retry forever. A successful connection
// resets the backoff.
func (s *Server) watchCell(c Cell) {
	defer s.wg.Done()
	backoff := s.baseBackoff
	for {
		connected := s.streamCell(c)
		select {
		case <-s.done:
			return
		default:
		}
		s.setCellUp(c.Name, false)
		s.clearInFlight(c.Name)
		if connected {
			backoff = s.baseBackoff
		}
		select {
		case <-s.done:
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, s.maxBackoff)
	}
}

// streamCell holds one /api/events connection open, wrapping every upstream
// message with the cell name and feeding modelStatus payloads to the
// start-duration tracker. Returns true if the connection was established
// (used to reset the reconnect backoff).
func (s *Server) streamCell(c Cell) (connected bool) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-s.done:
			cancel()
		case <-ctx.Done():
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL+"/api/events", nil)
	if err != nil {
		return false
	}
	// /api/events is gated by llama-swap's apiKeys like everything except
	// /health (C15, verified on v239). Without the credential the stream
	// 401s forever and the cell silently loses in-flight evidence,
	// per-model activity and every idle window built on them — the inputs
	// the warm policy, `await --idle` and the sleep guard all read.
	if err := s.AuthorizeSwap(req, c.Name); err != nil {
		return false
	}
	resp, err := s.streamClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	s.NoteSwapStatus(c.Name, resp.StatusCode)
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return false
	}
	s.setCellUp(c.Name, true)

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), scannerBufMax)
	var data []string
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if len(data) > 0 {
				s.handleUpstream(c.Name, strings.Join(data, "\n"))
				data = nil
			}
			continue
		}
		if rest, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimPrefix(rest, " "))
		}
		// event:/id:/retry: fields and comment lines are irrelevant:
		// llama-swap sends everything as event:message and the payload
		// type lives in the JSON envelope.
	}
	return true
}

// handleUpstream wraps one upstream SSE payload as a fleet Event. llama-swap
// frames every message as a {"type","data"} envelope with data being a JSON
// string; anything else is forwarded as type "message" with the raw payload
// string-encoded, so an upstream shape change degrades to noise instead of
// dropped events.
func (s *Server) handleUpstream(cell, payload string) {
	ev := Event{Cell: cell}
	var env struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(payload), &env); err == nil && env.Type != "" {
		ev.Type = env.Type
		ev.Data = env.Data
	} else {
		ev.Type = "message"
		raw, err := json.Marshal(payload)
		if err != nil {
			return
		}
		ev.Data = raw
	}
	if ev.Type == "modelStatus" {
		s.trackModelStatus(cell, ev.Data)
	}
	if ev.Type == "inflight" {
		s.trackInFlight(cell, ev.Data)
	}
	s.publish(ev)
}

// inflightEntry is one live request in llama-swap's inflight frames. The
// id is load-bearing on v240+, where a remove edge carries nothing else.
type inflightEntry struct {
	ID    string `json:"id"`
	Model string `json:"model"`
}

// trackInFlight folds llama-swap's inflight frames into the per-cell SET of
// live requests, keyed by request id. The frame's data is a JSON string
// (double-encoded: the SSE data field is an envelope whose data member is
// itself JSON text), and its shape is VERSION-DEPENDENT in a way that is
// not cosmetic:
//
//   - v239 sends the full list on every edge: {"requests":[…]}.
//   - v240+ tags each frame with an "operation" and sends the DELTA —
//     {"operation":"upsert","request":{…}} and {"operation":"remove","id":…} —
//     with "requests" omitempty and therefore absent.
//
// Counting len(requests) reads every v240+ edge as ZERO in flight while
// still reporting the count as known. That is the one reading this
// subsystem may never produce: a reported zero is a positive claim of
// idleness, and eight busy guards (drain, suspend, probe, both warm loops)
// disarm on it silently.
//
// So an operation this code does not recognise DISARMS the cell —
// s.inFlight[cell] returns to its unknown zero value, which every one of
// those guards already renders as a refusal. Fail toward "no evidence",
// never toward "confirmed idle": the drift nobody noticed is the normal
// case.
//
// Entries also feed per-model activity timestamps (fleetd-side clock), the
// idle windows C4's warm targets restore on. Holding the model per id is
// what lets a remove edge — which names only an id — still resolve a model
// for the completion stamp below.
func (s *Server) trackInFlight(cell string, data json.RawMessage) {
	var inner string
	if err := json.Unmarshal(data, &inner); err != nil {
		return
	}
	var wrap struct {
		Operation string          `json:"operation"`
		Requests  []inflightEntry `json:"requests"`
		Request   *inflightEntry  `json:"request"`
		ID        string          `json:"id"`
	}
	if err := json.Unmarshal([]byte(inner), &wrap); err != nil {
		return
	}

	now := time.Now() // duration clock: keep the monotonic reading
	s.mu.Lock()
	defer s.mu.Unlock()

	// Every frame is an EDGE — an add or a remove — so any frame is
	// request activity on this cell (C10). Recorded per CELL because that
	// is the question a parked batch asks: the per-model map below goes
	// quiet the moment llama-swap TTL-unloads the model somebody was
	// using thirty seconds ago. Stamped even for a shape we cannot fold:
	// not understanding a frame is not evidence that the cell is quiet,
	// and every consumer of this stamp errs toward waiting.
	s.lastInFlightFrame[cell] = now

	set := s.inFlightReqs[cell]
	if set == nil {
		set = map[string]string{}
	}
	switch wrap.Operation {
	case "", "snapshot":
		// A full list REPLACES, so the count is len(requests) exactly as
		// it was before ids entered this fold. The positional key keeps
		// that true for an entry with no id: ids matter only across
		// frames, and a full list carries no history.
		set = make(map[string]string, len(wrap.Requests))
		for i, r := range wrap.Requests {
			key := r.ID
			if key == "" {
				key = "\x00pos" + strconv.Itoa(i)
			}
			set[key] = r.Model
		}
	case "upsert", "add":
		if wrap.Request == nil {
			s.disarmInFlightLocked(cell, wrap.Operation+" (no request)")
			return
		}
		set[wrap.Request.ID] = wrap.Request.Model
	case "remove":
		id := wrap.ID
		if id == "" && wrap.Request != nil {
			id = wrap.Request.ID
		}
		if id == "" {
			s.disarmInFlightLocked(cell, wrap.Operation+" (no id)")
			return
		}
		// An unknown id is a no-op, not a signal: a remove for a request
		// that started before this connection is exactly what a reconnect
		// produces, and it says nothing about the rest of the set.
		delete(set, id)
	default:
		s.disarmInFlightLocked(cell, wrap.Operation)
		return
	}

	s.inFlightReqs[cell] = set
	s.inFlight[cell] = observed.Known(len(set))
	delete(s.inFlightUnknownOp, cell)

	seen := map[string]bool{}
	present := make([]string, 0, len(set))
	for _, model := range set {
		if model == "" {
			continue
		}
		s.modelActivity[cell+"\x00"+model] = now
		if !seen[model] {
			seen[model] = true
			present = append(present, model)
		}
	}
	// Frames are add/remove EDGES: a request stamps activity when it
	// starts and would never stamp again, so a generation longer than
	// restore_after_idle reads as idle. Stamp the completion edge too —
	// "last activity" means started OR finished.
	for _, m := range s.lastInFlightModels[cell] {
		if !seen[m] {
			s.modelActivity[cell+"\x00"+m] = now
		}
	}
	sort.Strings(present)
	s.lastInFlightModels[cell] = present
}

// disarmInFlightLocked un-reports the cell's in-flight count after a frame
// shape this build cannot fold. It is the deliberate degradation path: the
// alternative is reporting a count derived from a frame we did not
// understand, and every wrong answer there is "idle".
func (s *Server) disarmInFlightLocked(cell, op string) {
	delete(s.inFlightReqs, cell)
	delete(s.inFlight, cell)
	s.lastInFlightModels[cell] = nil
	if s.inFlightUnknownOp[cell] != op {
		s.inFlightUnknownOp[cell] = op
		slog.Warn("fleet: unrecognised llama-swap inflight frame — in-flight is now UNREPORTED for this cell, so every busy guard will refuse rather than assume idle. Upgrade vibe to a build that understands this llama-swap.",
			"cell", cell, "operation", op)
	}
}

// clearInFlight drops everything the in-flight fold knew about a cell when
// its events stream ends. The count does not survive the connection that
// produced it: the remove edges that would have closed those requests out
// were delivered to nobody, and llama-swap re-seeds a fresh connection with
// a current-state snapshot within milliseconds. lastInFlightFrame and
// modelActivity deliberately survive — those are records of activity that
// genuinely happened, not claims about now.
func (s *Server) clearInFlight(cell string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inFlightReqs, cell)
	delete(s.inFlight, cell)
	delete(s.inFlightUnknownOp, cell)
	s.lastInFlightModels[cell] = nil
}

// modelLastActivity returns when the model last served a request on
// the cell (fleetd-side clock). It is UNKNOWN when no inflight frame has
// ever mentioned the model — callers treat that as "idle since process
// start", never as "active now", and the observed.Value is what keeps
// the second half of that sentence from being one dropped bool away
// (C20).
func (s *Server) modelLastActivity(cell, model string) observed.Value[time.Time] {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.modelActivity[cell+"\x00"+model]
	if !ok {
		return observed.Value[time.Time]{}
	}
	return observed.Known(t)
}

// trackModelStatus measures starting→ready wall time per model. modelStatus
// payloads are full-catalog snapshots, so transitions are detected by
// diffing against the last seen state; any transition out of "starting"
// other than to "ready" (failed start, unload) abandons the measurement.
func (s *Server) trackModelStatus(cell string, data json.RawMessage) {
	var inner string
	if err := json.Unmarshal(data, &inner); err != nil {
		return
	}
	var models []struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(inner), &models); err != nil {
		return
	}
	now := time.Now()
	type rec struct {
		model   string
		seconds float64
	}
	var recs []rec
	s.mu.Lock()
	for _, m := range models {
		if m.ID == "" || m.State == "" {
			continue
		}
		key := cell + "\x00" + m.ID
		if s.lastState[key] == m.State {
			continue
		}
		s.lastState[key] = m.State
		switch m.State {
		case "starting":
			s.startedAt[key] = now
		case "ready":
			if t0, ok := s.startedAt[key]; ok {
				delete(s.startedAt, key)
				recs = append(recs, rec{model: m.ID, seconds: now.Sub(t0).Seconds()})
			}
		default:
			delete(s.startedAt, key)
		}
	}
	s.mu.Unlock()
	// Record outside s.mu: each Record rewrites the history file and file
	// IO must not serialize against the event hub.
	for _, r := range recs {
		s.hist.Record(r.model, now, r.seconds)
	}
}

// setCellUp records cell reachability and publishes a synthetic
// fleet.cellUp/fleet.cellDown event on transition (first observation
// included — a subscriber should learn "the cell is down" even if it was
// never up). Transitions also persist last-seen, so a fleetd restart
// keeps an absent cell's last sighting.
func (s *Server) setCellUp(name string, up bool) {
	s.mu.Lock()
	if prev, known := s.cellUp[name]; known && prev == up {
		s.mu.Unlock()
		return
	}
	s.cellUp[name] = up
	if up {
		now := time.Now()
		s.lastSeen[name] = now.UTC()
		// Monotonic reading kept: this is a duration clock (C10's idle
		// floor), and .UTC() would strip it.
		s.cellUpSince[name] = now
	} else {
		delete(s.cellUpSince, name)
	}
	s.publishLocked(Event{Cell: name, Type: cellTransitionType(up)})
	s.mu.Unlock()
	s.persistLastSeen()
}
