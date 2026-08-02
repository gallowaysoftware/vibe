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

// decorate fills a fresh snapshot's declared-intent, last-seen, and
// derived display state from the server's stores.
func (s *Server) decorate(snap *CellSnapshot) {
	s.mu.Lock()
	intent, hasIntent := s.intents[snap.Name]
	ls, hasLS := s.lastSeen[snap.Name]
	s.mu.Unlock()
	if hasIntent {
		in := intent
		snap.Intent = &in
	}
	if hasLS {
		t := ls
		snap.LastSeen = &t
	}
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
	return os.Rename(tmpPath, path)
}
