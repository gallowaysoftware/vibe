package fleetapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Presence bookkeeping: per-cell last-sighting timestamps (axis 1 —
// observed, never declared) and the optional host-level TCP probe that
// distinguishes "host up, cell down" from "host down".

// hostProbeTimeout bounds the host-level TCP dial so a filtered port
// can't stall the state snapshot past snapshotTimeout.
const hostProbeTimeout = 2 * time.Second

// probeTCP reports whether addr (host:port) accepts a TCP connection.
// Any completed handshake is proof the host is up; the service behind
// the port is irrelevant (the fleet convention is an SSH port).
func probeTCP(ctx context.Context, addr string) bool {
	d := net.Dialer{Timeout: hostProbeTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// lastSeenPersistAge bounds how stale the on-disk last_seen may be. A
// per-heartbeat write would rewrite the whole file every 15s per cell
// for a value that only matters once the cell is GONE; a never-written
// one loses the sighting entirely for exactly the no-inbound-port cells
// presence exists for.
const lastSeenPersistAge = 5 * time.Minute

// noteSighting records a fresh observation of a cell being reachable,
// from any source (watcher connect, on-demand snapshot, announce).
func (s *Server) noteSighting(name string) {
	s.recordSighting(name, time.Now().UTC(), false)
}

// recordSighting updates last_seen and persists it when the on-disk copy
// has aged out, on the first sighting, or when the caller knows this is a
// transition (returned/stale/withdrawn) — the moments whose timestamp a
// human actually reads.
func (s *Server) recordSighting(name string, at time.Time, transition bool) {
	s.mu.Lock()
	s.lastSeen[name] = at
	written, known := s.lastSeenPersisted[name]
	due := transition || !known || at.Sub(written) >= lastSeenPersistAge
	if due {
		s.lastSeenPersisted[name] = at
	}
	s.mu.Unlock()
	if due {
		s.persistLastSeen()
	}
}

// persistLastSeen snapshots and writes the last-seen map outside the
// hub mutex (same discipline as the start-history recorder).
func (s *Server) persistLastSeen() {
	if s.lastSeenPath == "" {
		return
	}
	s.mu.Lock()
	snap := make(map[string]time.Time, len(s.lastSeen))
	for k, v := range s.lastSeen {
		snap[k] = v
	}
	s.mu.Unlock()
	_ = saveLastSeen(s.lastSeenPath, snap)
}

// decorate fills a fresh snapshot's declared-intent, last-seen, active
// leases, presence, and derived display state from the server's stores.
//
// Availability semantics with presence (C3): a fresh announce proves the
// box is up (beating a firewalled host_probe). The serving axis honors
// EVIDENCE over declaration: a probe that just answered is proof of life
// a drained echo can't retract (the cell is mid-stop, or its ack is a
// lie — either way INCONSISTENT nags, per the design table). The echo
// decides availability only when probes can't reach the cell (the
// no-inbound-port case presence exists for).
func (s *Server) decorate(snap *CellSnapshot) {
	s.mu.Lock()
	intent, hasIntent := s.intents[snap.Name]
	ls, hasLS := s.lastSeen[snap.Name]
	var leases []Lease
	for _, l := range s.activeLeasesLocked() {
		if l.Cell == snap.Name {
			leases = append(leases, l)
		}
	}
	// Copy presence under the lock — recordAnnounce and the staleness
	// loop mutate the same entry, and a torn copy can panic the handler
	// (slice-header swap mid-read) or serialize a half-mixed state.
	var p *Presence
	if sp := s.presence[snap.Name]; sp != nil {
		cp := *sp
		p = &cp
	}
	s.mu.Unlock()

	var echo *AnnounceIntent
	probeOK := snap.Reachable
	if p != nil && p.Announcing {
		snap.Presence = p
		echo = p.IntentEcho
		fresh := !p.Stale && !p.Withdrawn
		if fresh {
			snap.HostReachable = new(bool)
			*snap.HostReachable = true
			switch {
			case probeOK:
				// Proof of life wins: a drained echo while the stack
				// still answers renders INCONSISTENT (nag), never a
				// silent DRAINED.
				snap.Reachable = true
			case echo != nil && echo.State == "drained":
				snap.Reachable = false
			default:
				snap.Reachable = true
			}
			// The announce IS the catalog when probes can't reach the
			// cell (C3's no-inbound-port destination): serving cells
			// report their models heartbeats-over-heartbeats.
			if !probeOK && len(p.Models) > 0 {
				snap.Models = nil
				for _, m := range p.Models {
					snap.Models = append(snap.Models, ModelState{ID: m.ID, State: m.State})
				}
			}
		} else {
			// Staleness retires the ANNOUNCE as evidence — not the probe.
			// An announcer that died (crashed unit, rotated token ⇒ 401,
			// renamed cell ⇒ 400) leaves a healthy cell serving; forcing
			// Reachable=false here reported it OFF/AWAY? beside its own
			// live model list and parked `vibe cell await --up` forever.
			snap.Reachable = probeOK
			if p.Withdrawn && !probeOK {
				// A clean withdraw is the box saying goodbye: the host
				// itself is gone for our purposes. A probe that just
				// answered outranks the goodbye — it is demonstrably here.
				snap.HostReachable = new(bool)
				*snap.HostReachable = false
			}
		}
	}

	eff, hasEff, pending := resolveIntent(intent, hasIntent, echo)
	if hasEff {
		in := eff
		snap.Intent = &in
	}
	snap.IntentPending = pending
	if hasLS {
		t := ls
		snap.LastSeen = &t
	}
	snap.Leases = leases
	snap.Display = displayState(snap.HostReachable, snap.Reachable, snap.Intent)
}

// resolveIntent is the declared-intent axis in one place: the registry
// REQUEST unless the cell's echo is newer (the conflict rule — the box
// you're standing at is always right). A drained echo with no registry
// request is intent too (the cell declared it locally). A serving-state
// entry is a resume REQUEST, not a display intent (serving is the
// default state). ok=false means "serving"; pending marks a request the
// cell hasn't caught up to — requested, not truth.
//
// Shared with the C4 warm loops, which must skip a drained cell whether
// the drain was requested through fleetd or performed at the box.
func resolveIntent(intent Intent, hasIntent bool, echo *AnnounceIntent) (eff Intent, ok, pending bool) {
	effective := intent
	servingRequest := hasIntent && intent.State == "serving"
	if echo != nil && echo.State == "drained" && (!hasIntent || intent.Since.Before(echo.Since) || servingRequest) {
		eff := Intent{State: "drained", Since: echo.Since}
		if hasIntent && intent.State == "drained" {
			// The WHY is axis 2's whole point (design §4): reason and ETA
			// live only in the registry entry, so rebuilding a bare intent
			// here erased them the moment the cell acked — and again on
			// every fleetd restart of a locally-drained cell.
			eff.Reason = intent.Reason
			eff.ETA = intent.ETA
		}
		effective = eff
		hasIntent = true
		servingRequest = false
	}
	pending = hasIntent && (servingRequest || (intent.State == "drained" && (echo == nil || echo.Since.Before(intent.Since))))
	if !hasIntent || servingRequest {
		return Intent{}, false, pending
	}
	return effective, true, pending
}

// effectiveIntent resolves one cell's declared intent from the stores.
func (s *Server) effectiveIntent(cell string) (Intent, bool) {
	s.mu.Lock()
	intent, hasIntent := s.intents[cell]
	var echo *AnnounceIntent
	if p := s.presence[cell]; p != nil && p.Announcing && p.IntentEcho != nil {
		e := *p.IntentEcho
		echo = &e
	}
	s.mu.Unlock()
	eff, ok, _ := resolveIntent(intent, hasIntent, echo)
	return eff, ok
}

// loadLastSeen reads the persisted sightings; missing is empty (a cell
// then shows no last_seen until sighted — honest "unknown" rather than
// a fabricated timestamp). Corrupt degrades to empty too, but loudly.
func loadLastSeen(path string) map[string]time.Time {
	seen := map[string]time.Time{}
	if path == "" {
		return seen
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return seen
	}
	if err != nil {
		slog.Warn("last-seen store unreadable, starting empty", "path", path, "err", err)
		return seen
	}
	if err := json.Unmarshal(data, &seen); err != nil {
		slog.Warn("last-seen store corrupt, starting empty", "path", path, "err", err)
	}
	return seen
}

// saveLastSeen writes sightings atomically (unique tmp + rename).
func saveLastSeen(path string, seen map[string]time.Time) error {
	data, err := json.MarshalIndent(seen, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".lastseen-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}
