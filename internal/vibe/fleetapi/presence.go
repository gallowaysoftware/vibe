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

// noteSighting records a fresh observation of a cell being reachable,
// from any source (watcher connect, on-demand snapshot). Persisted only
// on transitions and first sightings — the value that matters is the
// last sighting of a cell that is now absent, and a steadily-up cell's
// "last seen" is always "now" anyway.
func (s *Server) noteSighting(name string) {
	s.mu.Lock()
	_, known := s.lastSeen[name]
	s.lastSeen[name] = time.Now().UTC()
	s.mu.Unlock()
	if !known {
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
// Availability semantics with presence (C3): once a cell announces, its
// heartbeat is truth — a fresh announce proves the box is up (beating a
// firewalled host_probe), and the intent ECHO decides the serving axis
// (serving-with-no-loaded-models is up-cold, drained is stack-down).
// Stale/withdrawn presence reads as down. Never-announced cells keep
// the C1 probe behavior (host_probe + cell probes).
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
	p := s.presence[snap.Name]
	s.mu.Unlock()

	var echo *AnnounceIntent
	probeOK := snap.Reachable
	if p != nil && p.Announcing {
		cp := *p
		snap.Presence = &cp
		echo = p.IntentEcho
		fresh := !p.Stale && !p.Withdrawn
		if fresh {
			snap.HostReachable = new(bool)
			*snap.HostReachable = true
			snap.Reachable = echo == nil || echo.State != "drained"
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
			snap.Reachable = false
			if p.Withdrawn {
				// A clean withdraw is the box saying goodbye: the host
				// itself is gone for our purposes.
				snap.HostReachable = new(bool)
				*snap.HostReachable = false
			}
		}
	}

	// Effective intent: the registry REQUEST unless the cell's echo is
	// newer (the conflict rule — the box you're standing at is always
	// right). A drained echo with no registry request is intent too
	// (the cell declared it locally).
	effective := intent
	if echo != nil && echo.State == "drained" && (!hasIntent || intent.Since.Before(echo.Since)) {
		effective = Intent{State: "drained", Since: echo.Since}
		hasIntent = true
	}
	if hasIntent {
		in := effective
		snap.Intent = &in
		// Pending: a drained request the cell hasn't caught up to —
		// requested, not truth.
		if intent.State == "drained" && (echo == nil || echo.Since.Before(intent.Since)) {
			snap.IntentPending = true
		}
	}
	if hasLS {
		t := ls
		snap.LastSeen = &t
	}
	snap.Leases = leases
	snap.Display = displayState(snap.HostReachable, snap.Reachable, snap.Intent)
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
