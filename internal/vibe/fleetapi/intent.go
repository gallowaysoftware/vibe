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

// StopIntentReason is the reserved intent reason a cell unit's own stop
// hook writes (C24: drain where reclaim happens). It marks a record
// whose AUTHOR is the unit rather than a human or an agent: the serving
// stack stopped, that is all it knows, and in particular it does not
// know why.
//
// Reserved reason rather than a new field, for C14's reason — a stop is
// an ordinary drained entry on axis 2 and adds no state to the design's
// table. What the marker buys is four behaviours a human's declaration
// must NOT get, each of which is a bug if it is missing:
//
//  1. it is never handed back to the cell as desired_intent (announce.go).
//     The record describes a stop that already happened; sending it to
//     an announcing cell makes reconcile run cell_cmds.drain on a box
//     that just came back — the recorder becomes an actuator.
//  2. it loses to the cell's own drained echo (announce.go): a drained
//     echo is only ever produced by a DECLARED drain at the box, so the
//     stop record is redundant the moment one arrives, and leaving it
//     would stamp "stopped out of band" over the operator's own drain.
//  3. it is never "pending" (presence.go): nothing was requested of the
//     cell, so there is no ack to wait for and no residue to report.
//  4. it does not explain absence (notify.go, doctor.go). A crash stops
//     the unit exactly as a `systemctl stop` does, so an always_on cell
//     whose stack died still alarms. The record adds the WHEN and the
//     WHAT; the WHY is still missing, and every surface that cares about
//     the why behaves exactly as it did before this record existed.
const StopIntentReason = "stopped out of band"

// IsStopRecord reports whether an intent entry was written by a cell
// unit's own stop hook rather than declared by a human or an agent.
func IsStopRecord(in *Intent) bool {
	return in != nil && in.State == "drained" && in.Reason == StopIntentReason
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
	if req.State != "drained" && req.State != "serving" {
		http.Error(w, `state must be "drained" or "serving"`, http.StatusBadRequest)
		return
	}
	in, err := s.SetIntent(req.Cell, req.State, req.Reason, req.ETA)
	if err != nil {
		var uk *unknownCellError
		switch {
		case errors.As(err, &uk):
			http.Error(w, err.Error(), http.StatusBadRequest)
		default:
			slog.Warn("intent persist failed", "err", err)
			http.Error(w, "persist intent", http.StatusInternalServerError)
		}
		return
	}

	resp := intentResponse{Cell: req.Cell, State: "serving"}
	if in != nil {
		resp.Intent = in
		resp.State = in.State
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// SetIntent records (state "drained") or clears (state "serving") one
// cell's declared intent, returning the stored entry (nil when cleared).
// It is the in-process path fleetd's MCP facade uses after driving a
// drain/resume RPC — the fleetd-invoked writer of the C2 one-writer
// rule — and shares the HTTP endpoint's validation, serialization, and
// persistence: the whole clone-mutate-persist-swap runs under intentMu,
// and a failed persist leaves observable state untouched.
//
// "serving" semantics with presence (C3): for an ANNOUNCING cell the
// serving request must ride desired_intent to the cell, so it is
// STORED ({state: serving, since}) and resolved — deleted — when the
// cell echoes serving at a newer time. For never-announced cells it
// keeps the C1 meaning: delete (absence means serving).
func (s *Server) SetIntent(cell, state, reason, eta string) (*Intent, error) {
	return s.SetIntentAt(cell, state, reason, eta, time.Now().UTC())
}

// SetIntentAt is SetIntent with an explicit `since`, for the one class
// of verb whose subject stops answering: a suspend (C14).
//
// The conflict rule compares the registry request against the cell's
// echo, and the cell stamps its own `drained` while it is still running
// — i.e. AFTER fleetd issued the RPC and BEFORE fleetd could record the
// result. Stamping the record at the moment the RPC RETURNED therefore
// makes it permanently newer than the only echo that will ever exist,
// so the entry sits as "requested, awaiting ack" all night for an ack
// the box cannot give while it is asleep, and `vibe fleet doctor`'s
// intent hygiene calls it residue every morning.
//
// Passing the instant the action was ISSUED is not a fudge: that is
// genuinely when the intent was formed. The cell's newer echo then
// resolves the request through C6's complied-drain branch, which keeps
// the reason and the ETA and drops the pending flag.
func (s *Server) SetIntentAt(cell, state, reason, eta string, since time.Time) (*Intent, error) {
	known := false
	for _, c := range s.cells {
		if c.Name == cell {
			known = true
			break
		}
	}
	if !known {
		return nil, &unknownCellError{cell: cell}
	}

	s.intentMu.Lock()
	defer s.intentMu.Unlock()

	s.mu.Lock()
	next := make(map[string]Intent, len(s.intents)+1)
	for k, v := range s.intents {
		next[k] = v
	}
	announcing := false
	if p := s.presence[cell]; p != nil && p.Announcing {
		announcing = true
	}
	s.mu.Unlock()

	var stored *Intent
	since = since.UTC()
	switch state {
	case "serving":
		if reason == StopIntentReason {
			// C24, the unit's start half: retire the STOP RECORD this
			// cell's own stop hook left, and nothing else.
			//
			// Two things it must never do, both of which the ordinary
			// serving path does. It must not clear a HUMAN's declaration
			// — the operator is still gaming; the unit merely started —
			// so a record it did not write is answered with what is
			// there and left alone. And on an announcing cell it must
			// not become a serving REQUEST: a request is handed back as
			// desired_intent, where the announcer's reconcile runs
			// cell_cmds.resume. A hook that records must not be able to
			// actuate, and the way to guarantee that is to make the
			// write it performs incapable of producing a command.
			cur, had := next[cell]
			if !had {
				return nil, nil
			}
			if !IsStopRecord(&cur) {
				c := cur
				return &c, nil
			}
			delete(next, cell)
			break
		}
		if announcing {
			in := Intent{State: "serving", Since: since}
			next[cell] = in
			stored = &in
		} else {
			delete(next, cell)
		}
	case "drained":
		in := Intent{State: "drained", Reason: reason, ETA: eta, Since: since}
		next[cell] = in
		stored = &in
	default:
		return nil, fmt.Errorf("state must be \"drained\" or \"serving\"")
	}

	if err := saveIntents(s.intentPath, next); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.intents = next
	s.mu.Unlock()
	return stored, nil
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

// persistIntents is saveIntents for the RESOLUTION writers (the announce
// conflict rule, the stale-serving prune), whose clone→persist→swap must
// still swap when no store is configured. A disabled store is not a
// failed write: there is no file for memory to diverge from and no
// restart to resurrect a resolved drain, so gating the swap on a persist
// that can NEVER succeed would stop the C3 conflict rule dead — a resume
// performed at the box would leave the request pending forever, and the
// C4 warm loops read s.intents whether or not a store exists.
//
// setIntent deliberately keeps calling saveIntents directly: an operator
// POST to a disabled store must still fail loudly rather than record an
// intent nothing will remember.
func (s *Server) persistIntents(next map[string]Intent) error {
	if s.intentPath == "" {
		return nil
	}
	return saveIntents(s.intentPath, next)
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
