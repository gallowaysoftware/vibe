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

// The intent store (design doc §4 axis 2): one JSON file, almost always
// empty, written by `vibe cell drain`, the MCP drain_cell tool, or this
// endpoint directly. "serving" deletes the entry. Intent is declared by
// humans/agents, never inferred from observations, and never consulted
// for request routing.

// intentRequest is the POST /api/fleet/intent body.
type intentRequest struct {
	Cell   string `json:"cell"`
	State  string `json:"state"` // "drained" | "serving"
	Reason string `json:"reason,omitempty"`
	ETA    string `json:"eta,omitempty"`
}

// intentResponse answers a successful POST with the cell's intent as
// stored (state "serving" when the entry was deleted).
type intentResponse struct {
	Cell   string  `json:"cell"`
	Intent *Intent `json:"intent,omitempty"`
	State  string  `json:"state"`
}

// handleIntent sets or clears one cell's declared intent. Unknown cells
// and unknown states are 400s — a typo'd cell name must fail loudly, not
// record intent for a cell that doesn't exist.
//
// The whole clone-mutate-persist-swap sequence runs under intentMu: a
// concurrent pair of POSTs then serializes cleanly, the file can never be
// marshaled from a map another handler is mutating (a fatal runtime
// error), and a failed persist leaves the observable state untouched —
// the 500 and /api/fleet/state never disagree.
func (s *Server) handleIntent(w http.ResponseWriter, r *http.Request) {
	var req intentRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
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

	s.intentMu.Lock()
	defer s.intentMu.Unlock()

	s.mu.Lock()
	next := make(map[string]Intent, len(s.intents)+1)
	for k, v := range s.intents {
		next[k] = v
	}
	s.mu.Unlock()
	switch req.State {
	case "serving":
		delete(next, req.Cell)
	case "drained":
		next[req.Cell] = Intent{State: "drained", Reason: req.Reason, ETA: req.ETA, Since: time.Now().UTC()}
	default:
		http.Error(w, `state must be "drained" or "serving"`, http.StatusBadRequest)
		return
	}

	if err := saveIntents(s.intentPath, next); err != nil {
		slog.Warn("intent persist failed", "err", err)
		http.Error(w, "persist intent", http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	s.intents = next
	s.mu.Unlock()

	resp := intentResponse{Cell: req.Cell, State: "serving"}
	if req.State == "drained" {
		in := next[req.Cell]
		resp.Intent = &in
		resp.State = "drained"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// loadIntents reads the intent file; a missing file is an empty store
// (absence means serving — the safe default). A corrupt file also
// degrades to empty, but loudly: silent rot would flip a drained cell to
// DRAINED? after a fleetd restart with no explanation.
func loadIntents(path string) map[string]Intent {
	intents := map[string]Intent{}
	if path == "" {
		return intents
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return intents
	}
	if err != nil {
		slog.Warn("intent store unreadable, starting empty", "path", path, "err", err)
		return intents
	}
	if err := json.Unmarshal(data, &intents); err != nil {
		slog.Warn("intent store corrupt, starting empty", "path", path, "err", err)
	}
	return intents
}

// saveIntents writes the store atomically (unique tmp file + rename in the
// same directory) so a crash mid-write never leaves a truncated intent
// file and concurrent writers can't interleave into one tmp name.
func saveIntents(path string, intents map[string]Intent) error {
	if path == "" {
		return errors.New("intent store disabled")
	}
	data, err := json.MarshalIndent(intents, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".intent-*")
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
