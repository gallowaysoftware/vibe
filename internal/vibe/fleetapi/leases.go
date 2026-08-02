package fleetapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Advisory leases (fleet-control C2 §3): a batch consumer registers "I
// hold <model> on <cell>: mid-batch, N rows left" with a TTL, and the
// pre-drain report answers "would I strand a 19-hour job?" at drain time.
// Leases are ADVISORY ONLY — they appear in the pre-drain report and in
// fleet_status; they never block anything. TTL expiry is enforced at read
// time, so a crashed consumer cannot haunt the fleet.

// Lease is one advisory hold, keyed by (cell, model, holder) with
// last-write-wins per key (re-POST to refresh).
type Lease struct {
	Cell      string    `json:"cell"`
	Model     string    `json:"model"`
	Holder    string    `json:"holder"`
	Note      string    `json:"note,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}

// leaseRequest is the POST /api/fleet/lease body; the same three-field
// key (no ttl) is the DELETE body.
type leaseRequest struct {
	Cell   string `json:"cell"`
	Model  string `json:"model"`
	Holder string `json:"holder"`
	Note   string `json:"note,omitempty"`
	TTL    string `json:"ttl,omitempty"` // Go duration string, e.g. "2h"
}

func leaseKey(cell, model, holder string) string { return cell + "\x00" + model + "\x00" + holder }

// activeLeasesLocked returns unexpired leases; callers hold s.mu or
// accept a slightly stale read (expiry races are benign — a just-expired
// lease may appear once).
func (s *Server) activeLeasesLocked() []Lease {
	now := time.Now()
	var out []Lease
	for _, l := range s.leases {
		if l.ExpiresAt.After(now) {
			out = append(out, l)
		}
	}
	return out
}

// handleLeases serves GET /api/fleet/leases (optional ?cell= filter).
func (s *Server) handleLeases(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	active := s.activeLeasesLocked()
	s.mu.Unlock()
	filter := r.URL.Query().Get("cell")
	out := make([]Lease, 0, len(active))
	for _, l := range active {
		if filter != "" && l.Cell != filter {
			continue
		}
		out = append(out, l)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"leases": out})
}

// handleLeaseMutate serves POST /api/fleet/lease (upsert with TTL) and
// DELETE /api/fleet/lease (three-field key in the body). The
// clone-mutate-persist-swap sequence serializes under leaseMu, same
// discipline as the intent store.
func (s *Server) handleLeaseMutate(w http.ResponseWriter, r *http.Request) {
	var req leaseRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Cell == "" || req.Model == "" || req.Holder == "" {
		http.Error(w, "cell, model, and holder are required", http.StatusBadRequest)
		return
	}
	known := false
	for _, c := range s.cells {
		if c.Name == req.Cell {
			known = true
			break
		}
	}
	if !known {
		http.Error(w, fmt.Sprintf("unknown cell %q (not in the registry)", req.Cell), http.StatusBadRequest)
		return
	}

	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()

	s.mu.Lock()
	next := make(map[string]Lease, len(s.leases)+1)
	for k, v := range s.leases {
		next[k] = v
	}
	s.mu.Unlock()

	var respLease *Lease
	switch r.Method {
	case http.MethodPost:
		ttl, err := time.ParseDuration(req.TTL)
		if err != nil || ttl <= 0 {
			http.Error(w, `ttl must be a positive Go duration string (e.g. "2h")`, http.StatusBadRequest)
			return
		}
		l := Lease{Cell: req.Cell, Model: req.Model, Holder: req.Holder, Note: req.Note,
			ExpiresAt: time.Now().UTC().Add(ttl)}
		next[leaseKey(l.Cell, l.Model, l.Holder)] = l
		respLease = &l
	case http.MethodDelete:
		delete(next, leaseKey(req.Cell, req.Model, req.Holder))
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Prune expired entries on every mutation so the file can't grow
	// forever under crashed consumers.
	now := time.Now()
	for k, l := range next {
		if !l.ExpiresAt.After(now) {
			delete(next, k)
		}
	}

	if err := saveLeases(s.leasePath, next); err != nil {
		slog.Warn("lease persist failed", "err", err)
		http.Error(w, "persist lease", http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	s.leases = next
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if respLease != nil {
		_ = json.NewEncoder(w).Encode(respLease)
	} else {
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
	}
}

// loadLeases reads the lease file; missing is empty, corrupt is empty
// but loud (same discipline as the intent store).
func loadLeases(path string) map[string]Lease {
	leases := map[string]Lease{}
	if path == "" {
		return leases
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return leases
	}
	if err != nil {
		slog.Warn("lease store unreadable, starting empty", "path", path, "err", err)
		return leases
	}
	if err := json.Unmarshal(data, &leases); err != nil {
		slog.Warn("lease store corrupt, starting empty", "path", path, "err", err)
	}
	return leases
}

// saveLeases writes leases atomically (unique tmp + rename).
func saveLeases(path string, leases map[string]Lease) error {
	if path == "" {
		return errors.New("lease store disabled")
	}
	data, err := json.MarshalIndent(leases, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".leases-*")
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
