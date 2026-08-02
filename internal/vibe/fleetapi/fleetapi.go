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

// Cell is one llama-swap instance in the fleet registry. A single-box
// daemon holds only {front, http://127.0.0.1:<proxy_port>}; a fleetd
// (fleet_registry: true) builds the slice from hosts.yaml and every code
// path iterates it, so remote cells were additive from day one.
type Cell struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	// Class qualifies absence semantics (always_on | opportunistic |
	// roaming, fleetcfg.Class); empty on the single-box default cell.
	Class string `json:"class,omitempty"`
	// HostProbe is an optional host:port TCP dial distinguishing "host up,
	// cell down" from "host down" in the derived display state.
	HostProbe string `json:"-"`
}

// Intent is a declared operator note about a cell (design doc §4 axis 2):
// written by `vibe cell drain`, the MCP drain_cell tool, or a bare POST.
// Absence of an entry means serving. Intent is for humans and agents
// asking WHY; it is never consulted for request routing, and never
// inferred from observations.
type Intent struct {
	State  string    `json:"state"` // "drained"; absence means serving
	Reason string    `json:"reason,omitempty"`
	ETA    string    `json:"eta,omitempty"`
	Since  time.Time `json:"since"`
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
	// AuthRejected counts bearer-auth 401s on the TCP listener since
	// daemon start — a stale-token client shows up here as a rising
	// number instead of being invisible until someone reads logs.
	AuthRejected int64 `json:"auth_rejected"`
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

// CellSnapshot is one cell's merged state in the fleet snapshot.
type CellSnapshot struct {
	Name      string       `json:"name"`
	URL       string       `json:"url"`
	Reachable bool         `json:"reachable"`
	Models    []ModelState `json:"models"`
	// HostReachable is nil when the cell has no host_probe configured —
	// without it the host/cell distinction is unknowable (OFF/AWAY?).
	HostReachable *bool      `json:"host_reachable,omitempty"`
	Class         string     `json:"class,omitempty"`
	Intent        *Intent    `json:"intent,omitempty"`
	LastSeen      *time.Time `json:"last_seen,omitempty"`
	// Display is the derived display state (design doc §4 table), computed
	// at read time: SERVING / DRAINED / DRAINED? / OFF / OFF/AWAY /
	// OFF/AWAY? / INCONSISTENT.
	Display string `json:"display"`
}

// StateSnapshot is the full /api/fleet/state document. fleetmcp serves
// the same struct to MCP clients, so the CLI, the page, and agents can
// never disagree about what the fleet looks like.
type StateSnapshot struct {
	GeneratedAt  time.Time             `json:"generated_at"`
	Cells        []CellSnapshot        `json:"cells"`
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

	// intents is the declared-intent store (axis 2), nil-safe when
	// disabled; intentPath is its backing file. lastSeen holds per-cell
	// last-sighting timestamps, persisted to lastSeenPath on transitions
	// when set.
	intents      map[string]Intent
	intentPath   string
	lastSeen     map[string]time.Time
	lastSeenPath string

	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// Options tunes a Server's fleetd-role features. The zero value is the
// single-box default: no intent store, in-memory last-seen only.
type Options struct {
	// IntentPath enables the intent store (POST /api/fleet/intent) backed
	// by this JSON file. Empty disables it (the single-box default has no
	// fleet role to record intent for).
	IntentPath string
	// LastSeenPath persists per-cell last-seen timestamps so a fleetd
	// restart doesn't forget when an absent cell was last sighted. Empty
	// keeps last-seen in memory only.
	LastSeenPath string
}

// New builds a Server over the given cell registry. historyPath is the JSON
// file backing start-duration persistence (loaded now, rewritten on every
// recorded start); a missing or corrupt file degrades to empty history.
// daemonInfo is called per snapshot so the daemon half is never stale.
func New(cells []Cell, historyPath string, daemonInfo func() DaemonInfo, opts Options) *Server {
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
		intents:      loadIntents(opts.IntentPath),
		intentPath:   opts.IntentPath,
		lastSeen:     loadLastSeen(opts.LastSeenPath),
		lastSeenPath: opts.LastSeenPath,
		done:         make(chan struct{}),
	}
}

// Register mounts the fleet endpoints on the daemon's existing mux. Auth is
// the mux wrapper's business (bearer middleware on TCP, none on unix).
// The intent endpoint exists only when the intent store is enabled
// (fleetd role) — a single-box daemon has no fleet intent to record.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/fleet/state", s.handleState)
	mux.HandleFunc("GET /api/fleet/events", s.handleEvents)
	if s.intentPath != "" {
		mux.HandleFunc("POST /api/fleet/intent", s.handleIntent)
	}
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
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.Snapshot(ctx))
}

// Snapshot probes every cell in parallel and assembles the state
// document. Exported so in-process consumers (fleetmcp) get byte-identical
// state to what /api/fleet/state serves over HTTP.
func (s *Server) Snapshot(ctx context.Context) StateSnapshot {
	resp := StateSnapshot{
		GeneratedAt:  time.Now().UTC(),
		Cells:        make([]CellSnapshot, len(s.cells)),
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
	return resp
}

// snapshotCell merges the cell's /running (live processes: state/ttl) into
// /v1/models (full catalog). A model absent from /running is "stopped" —
// llama-swap's own default in its modelStatus payloads. The cell is
// reachable when either probe answered 200: llama-swap serves both, so one
// succeeding is proof of life even if the other transiently fails.
func (s *Server) snapshotCell(ctx context.Context, c Cell) CellSnapshot {
	snap := CellSnapshot{Name: c.Name, URL: c.URL, Class: c.Class, Models: []ModelState{}}

	if c.HostProbe != "" {
		snap.HostReachable = new(bool)
		*snap.HostReachable = probeTCP(ctx, c.HostProbe)
	}

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
		// A vibe-daemon-proxy cell (the roaming class before C3) answers
		// /v1/models with the Ollama passthrough shape instead — llama.cpp
		// only advertises what it has loaded, so entries here mean ready.
		Ollama []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	runErr := s.getJSON(ctx, c.URL+"/running", &runWrap)
	modErr := s.getJSON(ctx, c.URL+"/v1/models", &modWrap)
	running, models := runWrap.Running, modWrap.Data
	snap.Reachable = runErr == nil || modErr == nil
	if snap.Reachable {
		s.noteSighting(c.Name)
	}
	s.decorate(&snap)

	idx := map[string]int{}
	for _, m := range models {
		if _, dup := idx[m.ID]; dup {
			continue
		}
		idx[m.ID] = len(snap.Models)
		snap.Models = append(snap.Models, ModelState{ID: m.ID, Name: m.Name, State: "stopped"})
	}
	// A llama.cpp-family cell (vibe daemon proxy, bare llama-server — the
	// roaming class before C3) has no /running: it failed here while the
	// catalog answered. Its ollama-shape entries ARE the residency truth —
	// llama.cpp only advertises what it has loaded. (A llama-swap cell
	// whose /running transiently failed carries no ollama shape, so its
	// data[] entries honestly stay "stopped".)
	if runErr != nil {
		for _, m := range modWrap.Ollama {
			id := m.Model
			if id == "" {
				id = m.Name
			}
			if id == "" {
				continue
			}
			if i, ok := idx[id]; ok {
				snap.Models[i].State = "ready"
				continue
			}
			idx[id] = len(snap.Models)
			snap.Models = append(snap.Models, ModelState{ID: id, Name: m.Name, State: "ready"})
		}
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
