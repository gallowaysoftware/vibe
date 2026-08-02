package fleetapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	s.mu.Lock()
	switch req.State {
	case "serving":
		delete(s.intents, req.Cell)
	case "drained":
		s.intents[req.Cell] = Intent{State: "drained", Reason: req.Reason, ETA: req.ETA, Since: time.Now().UTC()}
	default:
		s.mu.Unlock()
		http.Error(w, `state must be "drained" or "serving"`, http.StatusBadRequest)
		return
	}
	intents := s.intents
	s.mu.Unlock()

	// File IO outside the hub mutex (same discipline as the start-history
	// recorder): a slow state dir must not stall event publishing.
	if err := saveIntents(s.intentPath, intents); err != nil {
		http.Error(w, fmt.Sprintf("persist intent: %v", err), http.StatusInternalServerError)
		return
	}

	resp := intentResponse{Cell: req.Cell, State: "serving"}
	if req.State == "drained" {
		s.mu.Lock()
		in := s.intents[req.Cell]
		s.mu.Unlock()
		resp.Intent = &in
		resp.State = "drained"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// loadIntents reads the intent file; a missing or corrupt file degrades
// to an empty store (absence means serving — the safe default).
func loadIntents(path string) map[string]Intent {
	intents := map[string]Intent{}
	if path == "" {
		return intents
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return intents
	}
	_ = json.Unmarshal(data, &intents)
	return intents
}

// saveIntents writes the store atomically (tmp + rename in the same
// directory) so a crash mid-write never leaves a truncated intent file.
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
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
