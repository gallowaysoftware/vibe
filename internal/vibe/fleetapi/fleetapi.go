// Package fleetapi is the daemon's fleet observability surface — the
// substrate the future web UI consumes (docs/design/router-lifecycle.md §11).
// It aggregates N llama-swap "cells" (today: just the local front cell on the
// proxy port) into one state snapshot (GET /api/fleet/state) and one
// multiplexed SSE event stream (GET /api/fleet/events), and records
// starting→ready wall time per model as the honest-ETA source for cold-start
// progress bars.
//
// Mounted on the daemon's existing control-plane mux, so the unix socket gets
// it unauthenticated (0600 socket perms) and the TCP listener gets it behind
// the same bearer-token middleware as the Connect handlers.
package fleetapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Cell is one llama-swap instance in the fleet registry. Today the registry
// holds only {front, http://127.0.0.1:<proxy_port>}, but every code path
// iterates the slice so remote cells are additive.
type Cell struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// ServiceInfo mirrors the daemon's service-mode status for the snapshot.
type ServiceInfo struct {
	Name  string `json:"name"`
	Ready bool   `json:"ready"`
	Addr  string `json:"addr,omitempty"`
	Pid   int    `json:"pid,omitempty"`
}

// DaemonInfo is the daemon-owned half of the state snapshot.
type DaemonInfo struct {
	ActiveProfile string        `json:"active_profile"`
	Services      []ServiceInfo `json:"services"`
}

// Event is one frame on /api/fleet/events: an upstream llama-swap message
// wrapped with its cell of origin, or a synthetic fleet.cellUp/fleet.cellDown
// transition (Data omitted). For upstream messages Type/Data are the
// llama-swap envelope fields verbatim (Data stays a JSON string containing
// the payload, matching llama-swap's own wire shape).
type Event struct {
	Cell string          `json:"cell"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// ModelState is one merged /running + /v1/models entry in the snapshot.
type ModelState struct {
	ID    string `json:"id"`
	State string `json:"state"`
	TTL   int    `json:"ttl,omitempty"`
	Name  string `json:"name,omitempty"`
}

type cellSnapshot struct {
	Name      string       `json:"name"`
	URL       string       `json:"url"`
	Reachable bool         `json:"reachable"`
	Models    []ModelState `json:"models"`
}

type stateResponse struct {
	GeneratedAt  time.Time             `json:"generated_at"`
	Cells        []cellSnapshot        `json:"cells"`
	Daemon       DaemonInfo            `json:"daemon"`
	StartHistory map[string]StartStats `json:"start_history"`
}

// snapshotTimeout bounds the per-cell /running + /v1/models probes so one
// dead cell can't stall the whole state response.
const snapshotTimeout = 3 * time.Second

// Server aggregates the cell registry, the event hub, and the start-duration
// history. Construct with New, mount with Register, then Start the per-cell
// watchers; Close is idempotent and required for the SSE handlers to unblock
// at daemon shutdown.
type Server struct {
	cells      []Cell
	daemonInfo func() DaemonInfo
	hist       *history

	snapClient   *http.Client
	streamClient *http.Client

	// baseBackoff/maxBackoff shape the watcher reconnect loop; tests dial
	// them down so cellDown→cellUp transitions resolve in milliseconds.
	baseBackoff time.Duration
	maxBackoff  time.Duration

	mu     sync.Mutex
	subs   map[chan Event]struct{}
	cellUp map[string]bool
	// lastState/startedAt key on cell+"\x00"+model so one map serves N
	// cells without nesting.
	lastState map[string]string
	startedAt map[string]time.Time

	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// New builds a Server over the given cell registry. historyPath is the JSON
// file backing start-duration persistence (loaded now, rewritten on every
// recorded start); a missing or corrupt file degrades to empty history.
// daemonInfo is called per snapshot so the daemon half is never stale.
func New(cells []Cell, historyPath string, daemonInfo func() DaemonInfo) *Server {
	return &Server{
		cells:        cells,
		daemonInfo:   daemonInfo,
		hist:         loadHistory(historyPath),
		snapClient:   &http.Client{Timeout: snapshotTimeout},
		streamClient: &http.Client{},
		baseBackoff:  500 * time.Millisecond,
		maxBackoff:   30 * time.Second,
		subs:         map[chan Event]struct{}{},
		cellUp:       map[string]bool{},
		lastState:    map[string]string{},
		startedAt:    map[string]time.Time{},
		done:         make(chan struct{}),
	}
}

// Register mounts the fleet endpoints on the daemon's existing mux. Auth is
// the mux wrapper's business (bearer middleware on TCP, none on unix).
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/fleet/state", s.handleState)
	mux.HandleFunc("GET /api/fleet/events", s.handleEvents)
}

// Start launches one watcher goroutine per cell.
func (s *Server) Start() {
	for _, c := range s.cells {
		s.wg.Add(1)
		go s.watchCell(c)
	}
}

// Close stops the watchers and unblocks every open SSE response. Idempotent.
func (s *Server) Close() {
	s.closeOnce.Do(func() { close(s.done) })
	s.wg.Wait()
}

// ─── state snapshot ─────────────────────────────────────────────────────────

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), snapshotTimeout)
	defer cancel()

	resp := stateResponse{
		GeneratedAt:  time.Now().UTC(),
		Cells:        make([]cellSnapshot, len(s.cells)),
		Daemon:       s.daemonInfo(),
		StartHistory: s.hist.Stats(),
	}
	var wg sync.WaitGroup
	for i, c := range s.cells {
		wg.Add(1)
		go func(i int, c Cell) {
			defer wg.Done()
			resp.Cells[i] = s.snapshotCell(ctx, c)
		}(i, c)
	}
	wg.Wait()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// snapshotCell merges the cell's /running (live processes: state/ttl) into
// /v1/models (full catalog). A model absent from /running is "stopped" —
// llama-swap's own default in its modelStatus payloads. The cell is
// reachable when either probe answered 200: llama-swap serves both, so one
// succeeding is proof of life even if the other transiently fails.
func (s *Server) snapshotCell(ctx context.Context, c Cell) cellSnapshot {
	snap := cellSnapshot{Name: c.Name, URL: c.URL, Models: []ModelState{}}

	var runWrap struct {
		Running []struct {
			Model string `json:"model"`
			State string `json:"state"`
			TTL   int    `json:"ttl"`
			Name  string `json:"name"`
		} `json:"running"`
	}
	var modWrap struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	runErr := s.getJSON(ctx, c.URL+"/running", &runWrap)
	modErr := s.getJSON(ctx, c.URL+"/v1/models", &modWrap)
	running, models := runWrap.Running, modWrap.Data
	snap.Reachable = runErr == nil || modErr == nil

	idx := map[string]int{}
	for _, m := range models {
		if _, dup := idx[m.ID]; dup {
			continue
		}
		idx[m.ID] = len(snap.Models)
		snap.Models = append(snap.Models, ModelState{ID: m.ID, Name: m.Name, State: "stopped"})
	}
	for _, r := range running {
		if i, ok := idx[r.Model]; ok {
			snap.Models[i].State = r.State
			snap.Models[i].TTL = r.TTL
			if snap.Models[i].Name == "" {
				snap.Models[i].Name = r.Name
			}
			continue
		}
		idx[r.Model] = len(snap.Models)
		snap.Models = append(snap.Models, ModelState{ID: r.Model, State: r.State, TTL: r.TTL, Name: r.Name})
	}
	sort.Slice(snap.Models, func(i, j int) bool { return snap.Models[i].ID < snap.Models[j].ID })
	return snap
}

func (s *Server) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := s.snapClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ─── event stream ───────────────────────────────────────────────────────────

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := make(chan Event, 256)
	// Register + snapshot reachability under one lock so the initial
	// synthetic events and subsequent live transitions are consistent (no
	// gap where a transition is neither replayed nor delivered).
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	initial := make([]Event, 0, len(s.cells))
	for _, c := range s.cells {
		if up, known := s.cellUp[c.Name]; known {
			initial = append(initial, Event{Cell: c.Name, Type: cellTransitionType(up)})
		}
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}()

	for _, ev := range initial {
		if err := writeEvent(w, ev); err != nil {
			return
		}
	}
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		case ev := <-ch:
			if err := writeEvent(w, ev); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeEvent frames one Event the same way llama-swap frames its own stream.
func writeEvent(w io.Writer, ev Event) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event:message\ndata:%s\n\n", data)
	return err
}

func cellTransitionType(up bool) string {
	if up {
		return "fleet.cellUp"
	}
	return "fleet.cellDown"
}

func (s *Server) publish(ev Event) {
	s.mu.Lock()
	s.publishLocked(ev)
	s.mu.Unlock()
}

// publishLocked drops events for slow subscribers instead of blocking the
// watcher: a stalled web-UI tab must not back-pressure the whole hub.
func (s *Server) publishLocked(ev Event) {
	for ch := range s.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}
