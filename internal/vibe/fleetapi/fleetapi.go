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
	"sync/atomic"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/prices"
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
	// Wake is the cell's Wake-on-LAN config (nil = no wake configured).
	// Kept out of the state JSON: the MAC is config, not status.
	Wake *WakeSpec `json:"-"`
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
	// GuestEnabled reports whether the C12 guest read-only bearer is
	// configured on this daemon. The flag, never the value: the token is
	// a credential and appears in no status document, no log line and no
	// error string.
	GuestEnabled bool `json:"guest_enabled,omitempty"`
	// GuestRejected counts requests that presented a VALID guest token on
	// a route the guest allowlist does not name. Deliberately NOT folded
	// into AuthRejected: that counter answers "some client holds a stale
	// token", and a guest reaching past their grant is a different fact
	// that would otherwise turn a working signal into noise on the day
	// someone shares a link.
	GuestRejected int64 `json:"guest_rejected,omitempty"`
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
	// Probe is the cell's latest throughput-health block for this model
	// (C8). Health is a per-MODEL property and lives ONLY here: it never
	// touches CellSnapshot.Reachable, .Display or .Intent, because those
	// are the availability and intent axes and a slow model is neither an
	// absent cell nor a declared one.
	Probe *AnnounceProbe `json:"probe,omitempty"`
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
	// Leases are the active advisory holds naming this cell (C2) — the
	// pre-drain "would I strand a batch job?" answer, advisory only.
	Leases []Lease `json:"leases,omitempty"`
	// IntentPending marks a registry intent REQUEST the cell hasn't
	// echoed yet (C3): surfaces show "drain requested, awaiting cell
	// ack" — never treated as truth. The cell's echo always wins.
	IntentPending bool `json:"intent_pending,omitempty"`
	// Presence is the cell's announce-derived state (C3), present once
	// the cell has announced at least once.
	Presence *Presence `json:"presence,omitempty"`
	// Activity is the cell's request-activity evidence (C10): what
	// fleetd can actually vouch for about how long this cell has been
	// quiet. Always present, including when the answer is "no evidence" —
	// an omitted field is how missing evidence gets read as idleness.
	Activity *CellActivity `json:"activity,omitempty"`
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
	// FrontRenders counts presence-driven front-config writes this
	// process has done (C3) — a flap storm shows up here as a rising
	// number instead of silently churning the catalog.
	FrontRenders int `json:"front_renders"`
	// Warm is the C4 warm policy status (targets + schedule), present
	// when either is configured.
	Warm *warmStatus `json:"warm,omitempty"`
	// Probe is the C8 throughput-probe status (scheduler state + the
	// currently-degraded models), present when either exists.
	Probe *probeStatus `json:"probe,omitempty"`
	// Notify is the C9 alarm-notifier status: the declared away/home
	// scope, every alarm the tracker holds (including ones whose delivery
	// away suppressed), and the delivery counters.
	Notify *notifyStatus `json:"notify,omitempty"`
}

// snapshotTimeout bounds the per-cell /running + /v1/models probes so one
// dead cell can't stall the whole state response.
const snapshotTimeout = 3 * time.Second

// Drain wait-status values carried in CellDrainResponse.wait_status.
// They live here rather than in the daemon because the CLI and the MCP
// facade both render them and neither may import the daemon.
const (
	DrainWaitNotRequested      = "not_requested"
	DrainWaitWaited            = "waited"
	DrainWaitSkippedNoInflight = "skipped_no_inflight_data"
)

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
	// cellUpSince is when the current /api/events connection to the cell
	// was established (C10). It is the floor on any idle claim: fleetd
	// cannot vouch for silence from before it was watching, and a
	// reconnect is exactly when a long-running request is invisible.
	cellUpSince map[string]time.Time
	// lastState/startedAt key on cell+"\x00"+model so one map serves N
	// cells without nesting.
	lastState map[string]string
	startedAt map[string]time.Time

	// intents is the declared-intent store (axis 2), nil-safe when
	// disabled; intentPath is its backing file. lastSeen holds per-cell
	// last-sighting timestamps, persisted to lastSeenPath on transitions
	// when set. intentMu serializes the intent endpoint's
	// clone-mutate-persist-swap so concurrent POSTs can't race the file
	// or the observable map.
	intents      map[string]Intent
	intentPath   string
	intentMu     sync.Mutex
	lastSeen     map[string]time.Time
	lastSeenPath string
	// lastSeenPersisted mirrors what the file already carries, so the
	// age gate on writes needs no stat.
	lastSeenPersisted map[string]time.Time

	// leases is the advisory-lease store (C2): keyed by
	// cell\x00model\x00holder, TTL-filtered at read. leaseMu serializes
	// mutations against persist, same discipline as intentMu.
	leases    map[string]Lease
	leasePath string
	leaseMu   sync.Mutex

	// flight is the in-flight Snapshot shared by concurrent callers —
	// one probe round serves every simultaneous /state consumer instead
	// of a goroutine fan-out per request.
	flightMu sync.Mutex
	flight   *snapshotFlight

	// presence is the announce-derived state per cell (C3): availability
	// and last_seen come from here once a cell announces, probes stay
	// the fallback. commands queues piggyback verbs for the announce
	// response. renderTrigger coalesces membership transitions for the
	// presence-derived render loop.
	presence map[string]*Presence
	commands map[string][]AnnounceCommand
	// cmdInflight holds each cell's handed-over-but-unacked command
	// batch (at-least-once delivery, retired by a higher announce seq).
	cmdInflight   map[string]inflightCommands
	renderTrigger chan string
	// stalenessTick paces stalenessLoop. Injectable so tests drive the
	// REAL loop instead of re-implementing its predicate — a test-file
	// copy of that predicate hid a disabled production one.
	stalenessTick time.Duration
	// renderWrites counts the presence-derived render loop's front-config
	// writes (fleet_status's flap-storm signal).
	renderWrites atomic.Int64

	// inFlight tracks each cell's current in-flight request count as
	// reported by llama-swap's inflight SSE frames. The bool in the
	// accessor distinguishes "reported zero" from "no frame seen yet" —
	// callers must not invent a count for a cell that never reported.
	inFlight     map[string]int
	inFlightSeen map[string]bool
	// modelActivity records per-model last-request timestamps from the
	// same frames (key cell+"\x00"+model), fleetd-side clock. The warm
	// targets' idle windows key off these.
	modelActivity map[string]time.Time
	// lastInFlightModels is the previous frame's model list per cell, so
	// a model that DISAPPEARS from the list gets a completion stamp —
	// without it a long generation produces one stamp at start and reads
	// as idle for its whole duration.
	lastInFlightModels map[string][]string
	// lastInFlightFrame is the cell-level analog of modelActivity (C10):
	// every inflight frame is an EDGE, so any frame is request activity
	// on that cell whether or not it names a model still resident. It
	// dominates every per-model stamp on the cell by construction.
	lastInFlightFrame map[string]time.Time

	// usage is the C7a token ledger, nil outside the fleetd role (a
	// single-box daemon has no announces to fold and no fleet to account
	// for).
	usage *usageLedger

	// hosts is the parsed hosts.yaml — read here for C7b's pricing,
	// power and capital config only. Cell MEMBERSHIP still travels
	// through s.cells; this is not a second cell list.
	hosts *fleetcfg.File
	// prices overrides the embedded price table (tests only). nil uses
	// the vendored artifact.
	prices *prices.Table

	// warmStates and schedStates are the C4 warm loops' status surfaces
	// (fleet_status's warm block). Pointers: the loop goroutines mutate
	// the entries in place under s.mu.
	warmStates  []*warmTargetState
	schedStates []*warmScheduleState

	// probeStates is the C8 probe scheduler's status surface, same
	// discipline as the warm states.
	probeStates []*probeTargetState

	// notify is the C9 alarm notifier (nil when unconfigured);
	// notifyScope is the declared away/home fleet scope. notifyMu guards
	// both plus the tracker, which is stepped by the evaluation loop and
	// read by every status render. It is NEVER held across a Snapshot:
	// the snapshot renders the notify block and takes it itself.
	notifyMu        sync.Mutex
	notify          *notifyRunner
	notifyScope     *NotifyScope
	notifyScopePath string

	// fpMismatch is the render loop's current fingerprint-mismatch set,
	// keyed cell+"\x00"+model — the state the C9 persistence threshold is
	// measured against, because the mismatch EVENT fires once and then
	// goes silent. renderLoopOn records whether the pass that maintains
	// it is running at all.
	fpMismatch   map[string]FingerprintMismatch
	renderLoopOn bool

	// started is this process's boot instant, immutable after New. The
	// warm policy uses it as the idle floor for a model no inflight frame
	// has ever mentioned: fleetd must never claim more silence than it
	// was running to observe.
	started time.Time

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
	// LeasePath enables the advisory lease store (POST/DELETE
	// /api/fleet/lease, GET /api/fleet/leases) backed by this JSON file.
	LeasePath string
	// UsagePath enables the C7a token ledger (GET /api/fleet/usage)
	// backed by this append-only JSONL file. Empty disables it.
	UsagePath string
	// Timezone is the explicit Location the ledger's day boundaries are
	// computed in. Nil means the process zone — which in fleetd's own
	// container is UTC, splitting an evening session across two days.
	// Declare it.
	Timezone *time.Location
	// Hosts is the parsed hosts.yaml, for C7b's pricing/power/capital
	// blocks. Nil leaves the savings screen priceless: tokens render,
	// money does not.
	Hosts *fleetcfg.File
	// Prices overrides the embedded price table (tests only).
	Prices *prices.Table
	// NotifyScopePath backs the declared away/home fleet scope (C9).
	// Empty keeps the scope in memory only.
	NotifyScopePath string
}

// New builds a Server over the given cell registry. historyPath is the JSON
// file backing start-duration persistence (loaded now, rewritten on every
// recorded start); a missing or corrupt file degrades to empty history.
// daemonInfo is called per snapshot so the daemon half is never stale.
func New(cells []Cell, historyPath string, daemonInfo func() DaemonInfo, opts Options) *Server {
	var usage *usageLedger
	if opts.UsagePath != "" {
		usage = newUsageLedger(opts.UsagePath, opts.Timezone)
	}
	return &Server{
		cells:              cells,
		daemonInfo:         daemonInfo,
		hist:               loadHistory(historyPath),
		snapClient:         &http.Client{Timeout: snapshotTimeout},
		streamClient:       &http.Client{},
		baseBackoff:        500 * time.Millisecond,
		maxBackoff:         30 * time.Second,
		subs:               map[chan Event]struct{}{},
		cellUp:             map[string]bool{},
		cellUpSince:        map[string]time.Time{},
		lastState:          map[string]string{},
		startedAt:          map[string]time.Time{},
		inFlight:           map[string]int{},
		inFlightSeen:       map[string]bool{},
		modelActivity:      map[string]time.Time{},
		lastInFlightModels: map[string][]string{},
		lastInFlightFrame:  map[string]time.Time{},
		started:            time.Now(),
		presence:           map[string]*Presence{},
		commands:           map[string][]AnnounceCommand{},
		cmdInflight:        map[string]inflightCommands{},
		renderTrigger:      make(chan string, 64),
		stalenessTick:      5 * time.Second,
		intents:            loadIntents(opts.IntentPath),
		intentPath:         opts.IntentPath,
		lastSeen:           loadLastSeen(opts.LastSeenPath),
		lastSeenPath:       opts.LastSeenPath,
		lastSeenPersisted:  map[string]time.Time{},
		leases:             loadLeases(opts.LeasePath),
		leasePath:          opts.LeasePath,
		usage:              usage,
		hosts:              opts.Hosts,
		prices:             opts.Prices,
		fpMismatch:         map[string]FingerprintMismatch{},
		notifyScope:        loadNotifyScope(opts.NotifyScopePath),
		notifyScopePath:    opts.NotifyScopePath,
		done:               make(chan struct{}),
	}
}

// Start launches one watcher goroutine per cell plus the announce
// staleness loop.
func (s *Server) Start() {
	for _, c := range s.cells {
		s.wg.Add(1)
		go s.watchCell(c)
	}
	// Add before the go, like every sibling loop: adding inside the
	// goroutine races Close's Wait, which can return while the loop still
	// touches the hub.
	s.wg.Add(1)
	go s.stalenessLoop()
	if s.usage != nil {
		s.wg.Add(1)
		go s.usageFlushLoop()
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

// snapshotFlight is one in-flight probe round shared by every concurrent
// Snapshot caller.
type snapshotFlight struct {
	done chan struct{}
	snap StateSnapshot
}

// Snapshot probes every cell in parallel and assembles the state
// document. Exported so in-process consumers (fleetmcp) get byte-identical
// state to what /api/fleet/state serves over HTTP. Concurrent callers
// share one probe round rather than fanning out per request.
func (s *Server) Snapshot(ctx context.Context) StateSnapshot {
	s.flightMu.Lock()
	if f := s.flight; f != nil {
		s.flightMu.Unlock()
		<-f.done
		return f.snap
	}
	f := &snapshotFlight{done: make(chan struct{})}
	s.flight = f
	s.flightMu.Unlock()

	defer func() {
		s.flightMu.Lock()
		s.flight = nil
		s.flightMu.Unlock()
		close(f.done)
	}()

	// Detach the probe round from the leader's cancellation: a client
	// disconnecting mid-round must not poison every follower's snapshot
	// with a degraded all-unreachable result. The round still completes
	// (bounded by its own deadline) and followers share the result.
	probe, cancel := context.WithTimeout(context.WithoutCancel(ctx), snapshotTimeout)
	defer cancel()
	f.snap = s.probeSnapshot(probe)
	return f.snap
}

// probeSnapshot runs one probe round: every cell in parallel, bounded by
// the caller's context.
func (s *Server) probeSnapshot(ctx context.Context) StateSnapshot {
	resp := StateSnapshot{
		GeneratedAt:  time.Now().UTC(),
		Cells:        make([]CellSnapshot, len(s.cells)),
		Daemon:       s.daemonInfo(),
		StartHistory: s.hist.Stats(),
		FrontRenders: s.RenderCount(),
		Warm:         s.warmReport(),
		Probe:        s.probeReport(),
		Notify:       s.notifyReport(),
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

// StartStats returns the recorded start-duration stats for one model —
// the warm path's honest cold-start ETA without a probe round.
func (s *Server) StartStats(model string) (StartStats, bool) {
	stats := s.hist.Stats()
	st, ok := stats[model]
	return st, ok
}

// InFlight returns the cell's last reported in-flight request count.
// The bool is false until llama-swap's events stream has sent one
// inflight frame — an unreported count must stay distinguishable from a
// reported zero (the pre-drain report omits the field rather than
// inventing it).
func (s *Server) InFlight(cell string) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.inFlightSeen[cell] {
		return 0, false
	}
	return s.inFlight[cell], true
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
	// After the model list is complete, whichever way it was built (probe
	// merge above, or presence in decorate when the cell answers nothing).
	s.attachProbes(&snap)
	snap.Activity = s.activityFor(c.Name)
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
