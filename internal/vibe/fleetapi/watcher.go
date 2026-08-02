package fleetapi

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
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
	resp, err := s.streamClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
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
	s.publish(ev)
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
		s.lastSeen[name] = time.Now().UTC()
	}
	s.publishLocked(Event{Cell: name, Type: cellTransitionType(up)})
	s.mu.Unlock()
	s.persistLastSeen()
}
