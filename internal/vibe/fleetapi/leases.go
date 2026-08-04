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

// Bounds on the lease store. A lease is a HOLD a running consumer
// refreshes, so a week is already generous; the count cap keeps a
// runaway producer from turning an advisory note into an unbounded file
// that every status surface then renders.
const (
	maxLeaseTTL = 168 * time.Hour
	maxLeases   = 512
)

// Lease is one advisory hold, keyed by (cell, model, holder) with
// last-write-wins per key (re-POST to refresh).
type Lease struct {
	Cell      string    `json:"cell"`
	Model     string    `json:"model"`
	Holder    string    `json:"holder"`
	Note      string    `json:"note,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
	// Hold marks this lease as a C11 hold: the operator's declaration
	// that fleetd's own warm policy must not act on the cell until it
	// expires. Every other lease property is identical — the flag is what
	// separates a note about work in progress from a policy override.
	Hold bool `json:"hold,omitempty"`
}

// leaseRequest is the POST /api/fleet/lease body; the same three-field
// key (no ttl) is the DELETE body.
type leaseRequest struct {
	Cell   string `json:"cell"`
	Model  string `json:"model"`
	Holder string `json:"holder"`
	Note   string `json:"note,omitempty"`
	TTL    string `json:"ttl,omitempty"` // Go duration string, e.g. "2h"
	Hold   bool   `json:"hold,omitempty"`
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
	for label, v := range map[string]string{"model": req.Model, "holder": req.Holder, "note": req.Note} {
		if err := clean(label, v); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
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

	switch r.Method {
	case http.MethodPost:
		ttl, err := time.ParseDuration(req.TTL)
		if err != nil || ttl <= 0 {
			http.Error(w, `ttl must be a positive Go duration string (e.g. "2h")`, http.StatusBadRequest)
			return
		}
		if ttl > maxLeaseTTL {
			http.Error(w, fmt.Sprintf("ttl exceeds the %s bound (a lease is a hold, not a reservation)", maxLeaseTTL), http.StatusBadRequest)
			return
		}
		// C11: the hold flag rides the same body, and its rules are the
		// hold's, not the lease store's — the reserved-holder pairing and
		// the tighter TTL bound.
		if err := validateHoldRequest(req.Hold, req.Holder, ttl); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Hold {
			if err := s.checkHoldTarget(req.Cell); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		l := Lease{Cell: req.Cell, Model: req.Model, Holder: req.Holder, Note: req.Note,
			Hold: req.Hold, ExpiresAt: time.Now().UTC().Add(ttl)}
		if err := s.putLease(l); err != nil {
			if errors.Is(err, errLeaseStoreFull) {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			slog.Warn("lease persist failed", "err", err)
			http.Error(w, "persist lease", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(l)
	case http.MethodDelete:
		if _, err := s.dropLease(req.Cell, req.Model, req.Holder); err != nil {
			slog.Warn("lease persist failed", "err", err)
			http.Error(w, "persist lease", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// errLeaseStoreFull is the cap refusal, typed so the HTTP endpoint can
// map it to 409 while the in-process verbs report it as an error like
// any other.
var errLeaseStoreFull = errors.New("lease store is full")

// putLease upserts one lease: clone, prune, cap, persist, swap. It is
// the ONLY mutation path into the store (with dropLease), shared by the
// HTTP endpoint and C11's in-process hold verbs — two writers with two
// copies of the prune-and-cap rules is how they drift apart.
func (s *Server) putLease(l Lease) error {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()

	next := s.cloneLeases()
	next[leaseKey(l.Cell, l.Model, l.Holder)] = l
	pruneExpired(next)
	// The cap applies AFTER the prune, so expired entries never count
	// against a live holder. A store this size is a runaway producer, not
	// a fleet. It gates GROWTH only: refusing a DELETE because the store
	// is full contradicts its own message and leaves an over-cap file
	// (a downgrade, a hand edit) with no way back down.
	if len(next) > maxLeases {
		return fmt.Errorf("%w (%d active); expire or delete some first", errLeaseStoreFull, maxLeases)
	}
	return s.commitLeases(next)
}

// dropLease removes one lease by key. The bool reports whether the key
// was actually present — release is idempotent, but a caller must be
// able to say "there was no hold" rather than claim it undid one.
func (s *Server) dropLease(cell, model, holder string) (bool, error) {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()

	next := s.cloneLeases()
	key := leaseKey(cell, model, holder)
	_, existed := next[key]
	delete(next, key)
	pruneExpired(next)
	if err := s.commitLeases(next); err != nil {
		return false, err
	}
	return existed, nil
}

// cloneLeases snapshots the store for a mutation. Callers hold leaseMu.
func (s *Server) cloneLeases() map[string]Lease {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]Lease, len(s.leases)+1)
	for k, v := range s.leases {
		next[k] = v
	}
	return next
}

// commitLeases persists then swaps: a failed write leaves the observable
// map untouched, so the caller's error is the whole truth.
func (s *Server) commitLeases(next map[string]Lease) error {
	if err := saveLeases(s.leasePath, next); err != nil {
		return err
	}
	s.mu.Lock()
	s.leases = next
	s.mu.Unlock()
	return nil
}

// pruneExpired drops expired entries on every mutation so the file can't
// grow forever under crashed consumers.
func pruneExpired(leases map[string]Lease) {
	now := time.Now()
	for k, l := range leases {
		if !l.ExpiresAt.After(now) {
			delete(leases, k)
		}
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
