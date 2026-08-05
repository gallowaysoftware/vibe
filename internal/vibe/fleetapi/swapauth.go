package fleetapi

import (
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
)

// The llama-swap credential (fleet-control C15).
//
// llama-swap's `apiKeys:` gates every route fleetd uses. Verified against
// a real v239 binary: `/health` is the ONE exempt route; `/v1/models`,
// `/running`, `/api/events`, `/api/metrics/activity`, `/api/models/unload/*`
// and `/v1/chat/completions` all answer 401 without a key, and a WRONG key
// gets the same 401 (never a 403). Both `Authorization: Bearer <key>` and
// `X-Api-Key: <key>` are accepted; this package sends the former, because
// it is also what a reverse proxy in front of a cell would honour.
//
// So a fleet that sets apiKeys did not merely lose its warms — before this
// phase it lost every probe, the whole events stream (and with it in-flight
// evidence, per-model activity and the idle windows the warm policy is
// built on), the front catalog check and the unload verb. Only the
// announce path kept working, because cells dial OUT.
//
// Three rules hold this together:
//
//   - The key is a CREDENTIAL. It is never logged, never in an error
//     string, never in fleet_status, never in an event payload and never
//     on the page. Only its ORIGIN (`cells.<name>.swap_key_file`) and the
//     FACT of a failure travel. Mirrors C9's webhook-URL redaction.
//   - A declared key that will not resolve fails CLOSED. fleetd does not
//     "try it unauthenticated" — that converts an operator typo into an
//     opaque 401 from a box across the house.
//   - A 401 stops the automated warm producers, it does not feed them.
//     Every warm loop is a ticker, so an unguarded 401 is a 15-second
//     retry loop that runs until someone reads a log. The refusal is
//     sticky per cell, self-clearing on the first accepted call, and
//     re-armed after swapAuthRetry so a rotated key recovers without a
//     fleetd restart.

// Swap credential failure kinds. The three are deliberately distinct
// because the fix is different for each, and "warm failed: HTTP 401" —
// what this path said before C15 — named none of them.
const (
	// SwapAuthUnauthorized: the cell's llama-swap demands a key and
	// hosts.yaml declares none for it. A misconfiguration.
	SwapAuthUnauthorized = "unauthorized"
	// SwapAuthRejected: fleetd presented the declared key and llama-swap
	// refused it. A rotation, or a key that never matched.
	SwapAuthRejected = "rejected"
	// SwapAuthUnresolvable: a swap_key_file is declared but cannot be read
	// into a usable key. Local, definitely fleetd's side, and no request
	// was sent.
	SwapAuthUnresolvable = "unresolvable"
)

// swapAuthRetry is how long an automated producer stays off a cell's
// llama-swap after a credential failure. Long enough that a broken config
// costs one request per five minutes instead of one per tick; short
// enough that fixing the key file brings the fleet back without a
// restart.
const swapAuthRetry = 5 * time.Minute

// swapAuthFailure is one cell's current credential problem.
type swapAuthFailure struct {
	Kind   string
	Since  time.Time
	Last   time.Time
	Detail string
}

// swapAuthStatus is the fleet_status block. It exists so a fleet whose
// warms have gone quiet says WHY on the same screen that shows the warm
// rows, instead of only in a log nobody is tailing.
type swapAuthStatus struct {
	Cells []swapAuthCell `json:"cells,omitempty"`
}

// swapAuthCell carries the failure KIND, when it started, and a sentence
// naming the config key to fix. It carries no key value and no path: the
// path is in the doctor report and the daemon's log, both token-only
// surfaces, while this document is guest-readable (C12's route grant is
// not a field grant).
type swapAuthCell struct {
	Cell       string    `json:"cell"`
	Kind       string    `json:"kind"`
	Since      time.Time `json:"since"`
	RetryAfter time.Time `json:"retry_after"`
	Detail     string    `json:"detail"`
}

// swapCredential resolves one cell's llama-swap key and records an
// unresolvable declaration as a failure, so a key file deleted at 03:00
// shows up in fleet_status and the doctor rather than only in whichever
// caller happened to try first.
func (s *Server) swapCredential(cell string) (fleetcfg.SwapCredential, error) {
	cred, err := s.hostsFile().SwapCredentialFor(cell)
	if err != nil {
		s.recordSwapAuth(cell, SwapAuthUnresolvable, fmt.Sprintf(
			"fleetd cannot read the llama-swap API key declared for %s, so no request is sent to its llama-swap at all: %v", cell, err))
		return cred, err
	}
	return cred, nil
}

// hostsFile is nil-safe access to the parsed hosts.yaml. A single-box
// daemon has none, and every cell there resolves to "no key declared".
func (s *Server) hostsFile() *fleetcfg.File { return s.hosts }

// AuthorizeSwap stamps the cell's llama-swap credential onto req.
// Exported because fleetmcp issues three of the seven fleetd→llama-swap
// calls (warm_model's completion, its catalog check, unload_model) and a
// second resolver in that package is how the two drift apart.
//
// A cell that declares no key leaves the request untouched — that is the
// reference fleet, and llama-swap without apiKeys ignores the header
// either way. A declared key that will not resolve returns an error and
// the caller MUST NOT send the request.
func (s *Server) AuthorizeSwap(req *http.Request, cell string) error {
	cred, err := s.swapCredential(cell)
	if err != nil {
		return err
	}
	if !cred.Configured {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+cred.Key)
	return nil
}

// NoteSwapStatus folds one llama-swap response status into the cell's
// credential record: 401 (llama-swap's only auth status — v239 sends 401
// for both a missing and a wrong key) and 403 record a failure, and
// ANY other status clears it, because a far side that answered anything
// else accepted the credential it was given. A 404 from a chat completion
// is llama-swap saying "no such model" to an authenticated caller.
//
// Exported for the same reason AuthorizeSwap is.
func (s *Server) NoteSwapStatus(cell string, status int) {
	if status != http.StatusUnauthorized && status != http.StatusForbidden {
		s.clearSwapAuth(cell)
		return
	}
	// Resolved rather than passed in: the CONFIGURED bit is what picks the
	// sentence, and a producer that had to remember to pass it is a
	// producer that will one day pass the wrong one.
	cred, err := s.hostsFile().SwapCredentialFor(cell)
	switch {
	case err != nil:
		s.recordSwapAuth(cell, SwapAuthUnresolvable, fmt.Sprintf(
			"%s's llama-swap refused fleetd (HTTP %d) and the key declared for it cannot be read: %v", cell, status, err))
	case cred.Configured:
		s.recordSwapAuth(cell, SwapAuthRejected, fmt.Sprintf(
			"%s's llama-swap REJECTED the key from cells.%s.swap_key_file (HTTP %d): that value is not in its apiKeys list "+
				"(rotated on one side only?)", cell, cell, status))
	default:
		s.recordSwapAuth(cell, SwapAuthUnauthorized, fmt.Sprintf(
			"%s's llama-swap requires an API key (HTTP %d) and hosts.yaml declares no cells.%s.swap_key_file: warms, catalog "+
				"reads, /running probes and the events stream to it all fail until one is set", cell, status, cell))
	}
}

// recordSwapAuth stores the failure, preserving Since across repeats so
// the status shows how long it has been broken rather than how long ago
// the last attempt was.
func (s *Server) recordSwapAuth(cell, kind, detail string) {
	now := time.Now()
	s.mu.Lock()
	prev, had := s.swapAuth[cell]
	f := swapAuthFailure{Kind: kind, Since: now, Last: now, Detail: detail}
	if had && prev.Kind == kind {
		f.Since = prev.Since
	}
	s.swapAuth[cell] = f
	s.mu.Unlock()
	if !had || prev.Kind != kind {
		// One line per transition, not per attempt: this is reached from a
		// 15s ticker and a 3s probe round.
		slog.Error("llama-swap credential failure", "cell", cell, "kind", kind, "detail", detail)
	}
}

// clearSwapAuth retires a cell's record on the first accepted call.
func (s *Server) clearSwapAuth(cell string) {
	s.mu.Lock()
	_, had := s.swapAuth[cell]
	delete(s.swapAuth, cell)
	s.mu.Unlock()
	if had {
		slog.Info("llama-swap credential accepted again", "cell", cell)
	}
}

// SwapAuthRefusal reports why an AUTOMATED call to this cell's llama-swap
// must not be made right now, and true when it must not.
//
// It is the no-retry-loop rule. The warm-target restore fires from a 15s
// ticker whose precondition ("the default is not resident") a 401 can
// never satisfy, so without this the fleet answers a misconfiguration
// with 5,760 identical 401s a day. After swapAuthRetry one call is
// allowed through — enough to notice a fixed key file, not enough to be a
// loop.
//
// It deliberately does NOT gate an operator's own verb: `warm_model`
// asking is not fleetd guessing, and the operator is the person who can
// read the answer.
func (s *Server) SwapAuthRefusal(cell string) (string, bool) {
	s.mu.Lock()
	f, ok := s.swapAuth[cell]
	s.mu.Unlock()
	if !ok {
		return "", false
	}
	if time.Since(f.Last) >= swapAuthRetry {
		return "", false
	}
	return f.Detail, true
}

// swapAuthReport builds the fleet_status block.
func (s *Server) swapAuthReport() *swapAuthStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.swapAuth) == 0 {
		return nil
	}
	rep := &swapAuthStatus{}
	for cell, f := range s.swapAuth {
		rep.Cells = append(rep.Cells, swapAuthCell{
			Cell:       cell,
			Kind:       f.Kind,
			Since:      f.Since.UTC(),
			RetryAfter: f.Last.Add(swapAuthRetry).UTC(),
			Detail:     f.Detail,
		})
	}
	sort.Slice(rep.Cells, func(i, j int) bool { return rep.Cells[i].Cell < rep.Cells[j].Cell })
	return rep
}

// swapAuthState exposes one cell's record to the doctor without handing
// it the map.
func (s *Server) swapAuthState(cell string) (swapAuthFailure, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.swapAuth[cell]
	return f, ok
}
