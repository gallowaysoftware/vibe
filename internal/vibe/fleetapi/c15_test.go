package fleetapi

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/astscan"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
	"github.com/gallowaysoftware/vibe/internal/vibe/router"
)

// fleet-control C15: the warm credential.
//
// Every test here drives the PRODUCTION path — the real warm loops, the
// real snapshot probes, the real watcher — because the defect this phase
// fixes was invisible for eleven phases precisely by being a header that
// nothing asserted on.

const labSwapKey = "swap-key-c15"

// keyedSwap is a llama-swap stand-in with apiKeys set: /health exempt,
// everything else 401 without the exact bearer. Modelled on a real v239
// binary, whose behaviour was measured before this phase was designed —
// including that a WRONG key gets 401 (never 403) and that the body names
// no configuration at all.
type keyedSwap struct {
	srv  *httptest.Server
	key  string // "" = no apiKeys configured on this llama-swap
	seen chan *http.Request
}

func newKeyedSwap(t *testing.T, key string) *keyedSwap {
	t.Helper()
	k := &keyedSwap{key: key, seen: make(chan *http.Request, 64)}
	k.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case k.seen <- r.Clone(context.Background()):
		default:
		}
		if k.key != "" && r.URL.Path != "/health" && r.Header.Get("Authorization") != "Bearer "+k.key {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized: invalid or missing API key","src":"llama-swap"}`))
			return
		}
		switch r.URL.Path {
		case "/running":
			_, _ = w.Write([]byte(`{"running":[{"model":"default-model","state":"ready","ttl":300}]}`))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"default-model"}]}`))
		case "/api/version":
			// Gated by apiKeys like everything except /health — the branch
			// above is what a keyed v239 answers an uncredentialed reader,
			// and it is why C16's version read had to join this scan.
			_, _ = w.Write([]byte(`{"version":"v239","commit":"dd81801","build_date":"2026-01-01T00:00:00Z"}`))
		case "/api/events":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-r.Context().Done()
		default:
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
		}
	}))
	t.Cleanup(k.srv.Close)
	return k
}

func (k *keyedSwap) requests() int { return len(k.seen) }

// hostsWithKey writes a hosts.yaml carrying a front whose llama-swap key
// lives in a file, and returns the parsed file. Real parse, real file
// read: the resolver's job is turning config into a header, and a
// hand-built struct would skip the half that has ever been wrong.
func hostsWithKey(t *testing.T, frontURL, key string) *fleetcfg.File {
	t.Helper()
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "front-swap.key")
	if err := os.WriteFile(keyPath, []byte(key+"\n"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return hostsWithKeyPath(t, frontURL, keyPath)
}

func hostsWithKeyPath(t *testing.T, frontURL, keyPath string) *fleetcfg.File {
	t.Helper()
	dir := t.TempDir()
	hosts := filepath.Join(dir, "hosts.yaml")
	body := "cells:\n  front:\n    url: \"" + frontURL + "\"\n    class: always_on\n"
	if keyPath != "" {
		body += "    swap_key_file: \"" + keyPath + "\"\n"
	}
	if err := os.WriteFile(hosts, []byte(body), 0o600); err != nil {
		t.Fatalf("write hosts.yaml: %v", err)
	}
	f, err := fleetcfg.LoadFrom(hosts)
	if err != nil {
		t.Fatalf("load hosts.yaml: %v", err)
	}
	return f
}

func newC15Server(t *testing.T, front *keyedSwap, hosts *fleetcfg.File) *Server {
	t.Helper()
	s := New([]Cell{{Name: fleetcfg.FrontCell, URL: front.srv.URL, Class: "always_on"}},
		filepath.Join(t.TempDir(), "hist.json"), testDaemonInfo, Options{Hosts: hosts})
	s.baseBackoff = 10 * time.Millisecond
	s.maxBackoff = 20 * time.Millisecond
	t.Cleanup(s.Close)
	return s
}

// ── U1: the warm carries the credential ─────────────────────────────────

func TestWarmViaFront_CarriesTheDeclaredSwapKey(t *testing.T) {
	front := newKeyedSwap(t, labSwapKey)
	s := newC15Server(t, front, hostsWithKey(t, front.srv.URL, labSwapKey))
	if err := s.warmViaFront(t.Context(), front.srv.URL, "default-model"); err != nil {
		t.Fatalf("warm through a keyed front failed: %v", err)
	}
	req := <-front.seen
	if got := req.Header.Get("Authorization"); got != "Bearer "+labSwapKey {
		t.Fatalf("Authorization = %q, want the declared key as a bearer", got)
	}
}

func TestWarmViaFront_UnkeyedFrontSendsNoHeader(t *testing.T) {
	front := newKeyedSwap(t, "")
	s := newC15Server(t, front, hostsWithKeyPath(t, front.srv.URL, ""))
	if err := s.warmViaFront(t.Context(), front.srv.URL, "default-model"); err != nil {
		t.Fatalf("warm through an unkeyed front failed: %v", err)
	}
	req := <-front.seen
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization = %q on a fleet that declares no key, want none", got)
	}
}

// ── U2: the two 401s are different sentences ────────────────────────────

func TestWarmViaFront_401Diagnoses_MisconfigurationVsRejection(t *testing.T) {
	t.Run("no key declared and the front demands one", func(t *testing.T) {
		front := newKeyedSwap(t, labSwapKey)
		s := newC15Server(t, front, hostsWithKeyPath(t, front.srv.URL, ""))
		err := s.warmViaFront(t.Context(), front.srv.URL, "default-model")
		if err == nil {
			t.Fatal("a 401 warm reported success")
		}
		if !strings.Contains(err.Error(), "cells.front.swap_key_file") {
			t.Errorf("error does not name the config to set: %v", err)
		}
		if strings.Contains(err.Error(), "REJECTED") {
			t.Errorf("a missing credential was reported as a rejected one: %v", err)
		}
		if f, ok := s.swapAuthState(fleetcfg.FrontCell); !ok || f.Kind != SwapAuthUnauthorized {
			t.Errorf("kind = %+v, want %s", f, SwapAuthUnauthorized)
		}
	})

	t.Run("a declared key the front refuses", func(t *testing.T) {
		front := newKeyedSwap(t, labSwapKey)
		s := newC15Server(t, front, hostsWithKey(t, front.srv.URL, "the-old-rotated-key"))
		err := s.warmViaFront(t.Context(), front.srv.URL, "default-model")
		if err == nil {
			t.Fatal("a 401 warm reported success")
		}
		if !strings.Contains(err.Error(), "REJECTED") {
			t.Errorf("a rejected credential was not reported as one: %v", err)
		}
		if f, ok := s.swapAuthState(fleetcfg.FrontCell); !ok || f.Kind != SwapAuthRejected {
			t.Errorf("kind = %+v, want %s", f, SwapAuthRejected)
		}
	})
}

// TestWarmViaFront_401StaysDefinitiveAndIsNeverQueued pins that C6's 4xx
// rule survives the new wrapping: an auth failure is the front ANSWERING,
// and queueing it to the cell would route around a broken credential.
func TestWarmViaFront_401StaysDefinitiveAndIsNeverQueued(t *testing.T) {
	front := newKeyedSwap(t, labSwapKey)
	s := newC15Server(t, front, hostsWithKeyPath(t, front.srv.URL, ""))
	err := s.warmViaFront(t.Context(), front.srv.URL, "default-model")
	if !definitiveWarmRefusal(err) {
		t.Fatalf("a 401 was not read as the front answering: %v", err)
	}
	presenceOf(s, fleetcfg.FrontCell, AnnounceModel{ID: "default-model", State: "ready"})
	if _, qerr := s.queueWarm(fleetcfg.FrontCell, "default-model", err); qerr == nil {
		t.Fatal("a 401 warm was queued to the cell — the credential failure would be hidden behind a promise")
	}
}

// ── U3: fail CLOSED on an unresolvable declaration ──────────────────────

func TestSwapKey_UnresolvableDeclarationSendsNothing(t *testing.T) {
	front := newKeyedSwap(t, labSwapKey)
	missing := filepath.Join(t.TempDir(), "never-written.key")
	s := newC15Server(t, front, hostsWithKeyPath(t, front.srv.URL, missing))

	err := s.warmViaFront(t.Context(), front.srv.URL, "default-model")
	if err == nil {
		t.Fatal("warm succeeded with an unreadable key file")
	}
	// The sentence names the CONFIG KEY, and deliberately not the path:
	// this error propagates into the warm status rows, which ride
	// `/api/fleet/state` — C12's one guest-readable document. The path is
	// reachable through Unwrap for the token-only surfaces.
	if !strings.Contains(err.Error(), "cells.front.swap_key_file") {
		t.Errorf("error does not name the config to fix: %v", err)
	}
	if strings.Contains(err.Error(), missing) {
		t.Errorf("the credential error carries the key file's PATH, and this string reaches a guest: %v", err)
	}
	var ske *fleetcfg.SwapKeyError
	if !errors.As(err, &ske) || ske.Path != missing {
		t.Errorf("the path is not recoverable for the token-only surfaces: %v", err)
	}
	if front.requests() != 0 {
		t.Fatalf("%d request(s) reached the front; a declared-but-unreadable key must fail CLOSED", front.requests())
	}
	if f, ok := s.swapAuthState(fleetcfg.FrontCell); !ok || f.Kind != SwapAuthUnresolvable {
		t.Errorf("kind = %+v, want %s", f, SwapAuthUnresolvable)
	}
}

func TestSwapCredentialFor_RejectsUnusableKeys(t *testing.T) {
	dir := t.TempDir()
	for name, tc := range map[string]struct{ body, want string }{
		"empty":            {"   \n", "empty file"},
		"embedded-newline": {"abc\ndef\n", "control character"},
		"tab":              {"abc\tdef", "control character"},
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(dir, name+".key")
			if err := os.WriteFile(p, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			f := hostsWithKeyPath(t, "http://127.0.0.1:1", p)
			cred, err := f.SwapCredentialFor(fleetcfg.FrontCell)
			if err == nil {
				t.Fatalf("resolved an unusable key file")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
			if !cred.Configured {
				t.Error("a failed resolution lost the Configured bit, so the diagnosis would read as 'no key declared'")
			}
			if strings.Contains(err.Error(), "abc") {
				t.Errorf("the error string leaked key bytes: %v", err)
			}
			// Public() is what a guest-readable surface may render: the
			// hosts.yaml key and the reason, never the path.
			var ske *fleetcfg.SwapKeyError
			if !errors.As(err, &ske) {
				t.Fatalf("not a *SwapKeyError, so no surface can tell what it may disclose: %v", err)
			}
			if strings.Contains(ske.Public(), p) || !strings.Contains(ske.Public(), "cells.front.swap_key_file") {
				t.Errorf("Public() = %q, want the config key and no path", ske.Public())
			}
		})
	}
}

// ── U4: no silent retry loop ────────────────────────────────────────────

// TestWarmTargetRestore_StopsAfterTheFrontRefusesTheCredential drives the
// REAL warm-target loop against a REAL keyed front. The precondition for
// the restore ("nothing resident") is one a 401 can never satisfy, so an
// unguarded loop issues one warm per tick forever.
func TestWarmTargetRestore_StopsAfterTheFrontRefusesTheCredential(t *testing.T) {
	front := newKeyedSwap(t, labSwapKey)
	s := newC15Server(t, front, hostsWithKeyPath(t, front.srv.URL, ""))
	s.cells = append(s.cells, Cell{Name: "heavy", URL: "http://127.0.0.1:1", Class: "always_on"})
	s.startWarmLoopWithConfig(warmLoopConfig{
		targets:    []WarmTarget{{Cell: "heavy", Model: "default-model", RestoreAfterIdle: time.Millisecond}},
		frontURL:   front.srv.URL,
		tick:       10 * time.Millisecond,
		emptyGrace: time.Millisecond,
	})
	// A cell that announces nothing resident: the restore's own trigger.
	presenceOf(s, "heavy")

	// Wait for the ONE attempt the policy is entitled to (a generous
	// deadline: a loaded CI box may not schedule the tick promptly, and
	// "the loop never ran" must not read as "the loop was suppressed").
	firstBy := time.Now().Add(10 * time.Second)
	for front.requests() == 0 && time.Now().Before(firstBy) {
		time.Sleep(5 * time.Millisecond)
	}
	if front.requests() == 0 {
		t.Fatal("the warm loop never attempted a restore, so nothing here proves suppression")
	}
	// Then hold it there across many more ticks than an unguarded loop
	// would need: at a 10ms tick, 50 ticks.
	for range 50 {
		if front.requests() > 1 {
			t.Fatalf("the warm loop sent %d warms at a front that 401s — a retry loop against a credential failure", front.requests())
		}
		time.Sleep(10 * time.Millisecond)
	}
	st := warmTargetStateOf(s, 0)
	if st.State != "skipped" {
		t.Fatalf("state = %s (%s), want skipped", st.State, st.Detail)
	}
	if !strings.Contains(st.Detail, "cells.front.swap_key_file") {
		t.Errorf("the warm row does not name the config to fix: %q", st.Detail)
	}
	if st.LastRestore != nil {
		t.Error("a refused warm stamped last_restore")
	}
}

// TestRestoreItselfHoldsTheCredentialRung pins the FIRE-time half
// separately from the eval-time one. The rung lives in two places on
// purpose (the shape warmClassRefusal uses) and a single test covering
// only the loop would leave the second one free to be deleted with the
// suite still green — the exact defect C5's addendum names.
func TestRestoreItselfHoldsTheCredentialRung(t *testing.T) {
	front := newKeyedSwap(t, labSwapKey)
	s := newC15Server(t, front, hostsWithKeyPath(t, front.srv.URL, ""))
	s.cells = append(s.cells, Cell{Name: "heavy", URL: "http://127.0.0.1:1", Class: "always_on"})
	s.NoteSwapStatus(fleetcfg.FrontCell, http.StatusUnauthorized)

	st := &warmTargetState{Cell: "heavy", Model: "default-model"}
	s.mu.Lock()
	s.warmStates = append(s.warmStates, st)
	s.mu.Unlock()
	s.restore(WarmTarget{Cell: "heavy", Model: "default-model"}, st,
		warmLoopConfig{frontURL: front.srv.URL, warmFn: s.warmViaFront}, "nothing resident")

	if front.requests() != 0 {
		t.Fatalf("restore sent %d warms at a front known to be refusing the credential", front.requests())
	}
	if got := warmTargetStateOf(s, 0); got.State != "skipped" || !strings.Contains(got.Detail, "swap_key_file") {
		t.Fatalf("state = %s (%s), want skipped naming the config", got.State, got.Detail)
	}
}

func TestSwapAuthRefusal_ReArmsAfterTheRetryWindow(t *testing.T) {
	s := newWarmServer(t, nil)
	s.recordSwapAuth(fleetcfg.FrontCell, SwapAuthUnauthorized, "detail")
	if _, blocked := s.SwapAuthRefusal(fleetcfg.FrontCell); !blocked {
		t.Fatal("a fresh credential failure did not suppress automated calls")
	}
	// Age the LAST attempt past the window; Since must survive, because
	// the status answers "how long has this been broken".
	s.mu.Lock()
	f := s.swapAuth[fleetcfg.FrontCell]
	since := f.Since
	f.Last = time.Now().Add(-swapAuthRetry - time.Second)
	s.swapAuth[fleetcfg.FrontCell] = f
	s.mu.Unlock()
	if _, blocked := s.SwapAuthRefusal(fleetcfg.FrontCell); blocked {
		t.Fatal("the suppression never re-arms; a fixed key file would need a fleetd restart")
	}
	s.recordSwapAuth(fleetcfg.FrontCell, SwapAuthUnauthorized, "detail")
	if got := s.swapAuthReport(); got == nil || len(got.Cells) != 1 || !got.Cells[0].Since.Equal(since.UTC()) {
		t.Errorf("Since was reset by a repeat of the same failure: %+v", got)
	}
}

func TestSwapAuth_ClearsOnTheFirstAcceptedCall(t *testing.T) {
	s := newWarmServer(t, nil)
	s.recordSwapAuth("heavy", SwapAuthRejected, "detail")
	s.NoteSwapStatus("heavy", http.StatusOK)
	if _, ok := s.swapAuthState("heavy"); ok {
		t.Fatal("an accepted call did not clear the credential failure")
	}
	// A 404 is llama-swap answering an AUTHENTICATED caller "no such
	// model": it clears too, or a transient bad model id would pin the
	// fleet into a permanent credential alarm.
	s.recordSwapAuth("heavy", SwapAuthRejected, "detail")
	s.NoteSwapStatus("heavy", http.StatusNotFound)
	if _, ok := s.swapAuthState("heavy"); ok {
		t.Fatal("a 404 (the far side answering) was read as a credential failure")
	}
}

// ── U5: the other automated producers ───────────────────────────────────

func TestWarmSchedule_SkipsWhenTheFrontRefusesTheCredential(t *testing.T) {
	front := newKeyedSwap(t, labSwapKey)
	s := newC15Server(t, front, hostsWithKeyPath(t, front.srv.URL, ""))
	// Recorded the way production records it — from a real 401 — so the
	// note under test is the real diagnosis and not a string this test
	// wrote for itself.
	s.NoteSwapStatus(fleetcfg.FrontCell, http.StatusUnauthorized)

	st := &warmScheduleState{Cron: "* * * * *", Model: "default-model"}
	now := time.Now()
	st.NextFire = &now
	s.evalScheduleEntry(WarmScheduleEntry{Cron: "* * * * *", Model: "default-model"}, cronSpec{}, st,
		func(string) (string, error) { return "heavy", nil }, time.UTC, s.warmViaFront, front.srv.URL, now.Add(time.Second))

	if front.requests() != 0 {
		t.Fatalf("a scheduled warm fired at a front known to be refusing the credential (%d requests)", front.requests())
	}
	s.mu.Lock()
	note := st.LastNote
	fired := st.LastFire
	s.mu.Unlock()
	if !strings.Contains(note, "swap_key_file") {
		t.Errorf("last_note = %q, want the credential diagnosis", note)
	}
	if fired != nil {
		t.Error("a skipped schedule stamped last_fire")
	}
}

func TestWakeWarm_SkipsWhenTheFrontRefusesTheCredential(t *testing.T) {
	front := newKeyedSwap(t, labSwapKey)
	s := newC15Server(t, front, hostsWithKeyPath(t, front.srv.URL, ""))
	s.cells = append(s.cells, Cell{Name: "heavy", URL: "http://127.0.0.1:1", Class: "opportunistic"})
	s.NoteSwapStatus(fleetcfg.FrontCell, http.StatusUnauthorized)

	note := s.wakeWarm(t.Context(), "heavy", "default-model", sleepLoopConfig{frontURL: front.srv.URL, warmFn: s.warmViaFront})
	if front.requests() != 0 {
		t.Fatalf("a post-wake warm fired at a front known to be refusing the credential (%d requests)", front.requests())
	}
	if !strings.Contains(note, "swap_key_file") {
		t.Errorf("wake note = %q, want the credential diagnosis", note)
	}
}

// ── U6: the probes and the events stream ────────────────────────────────

func TestSnapshotProbesCarryTheSwapKey(t *testing.T) {
	front := newKeyedSwap(t, labSwapKey)
	s := newC15Server(t, front, hostsWithKey(t, front.srv.URL, labSwapKey))
	snap := s.Snapshot(t.Context())
	if len(snap.Cells) != 1 || !snap.Cells[0].Reachable {
		t.Fatalf("keyed cell read as unreachable: %+v", snap.Cells)
	}
	if snap.SwapAuth != nil {
		t.Fatalf("a working credential produced a swap_auth block: %+v", snap.SwapAuth)
	}
	if len(snap.Cells[0].Models) == 0 {
		t.Fatal("the catalog came back empty through an authenticated probe")
	}
}

func TestSnapshotProbes_401IsReportedNotJustUnreachable(t *testing.T) {
	front := newKeyedSwap(t, labSwapKey)
	s := newC15Server(t, front, hostsWithKeyPath(t, front.srv.URL, ""))
	snap := s.Snapshot(t.Context())
	if snap.Cells[0].Reachable {
		t.Fatal("a cell that 401s every route reported reachable")
	}
	if snap.SwapAuth == nil || len(snap.SwapAuth.Cells) != 1 {
		t.Fatalf("the state document does not say WHY the cell is unreadable: %+v", snap.SwapAuth)
	}
	got := snap.SwapAuth.Cells[0]
	if got.Kind != SwapAuthUnauthorized || !strings.Contains(got.Detail, "swap_key_file") {
		t.Errorf("swap_auth row = %+v, want an unauthorized row naming the config", got)
	}
}

func TestEventsStreamCarriesTheSwapKey(t *testing.T) {
	front := newKeyedSwap(t, labSwapKey)
	s := newC15Server(t, front, hostsWithKey(t, front.srv.URL, labSwapKey))
	s.Start()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		up := s.cellUp[fleetcfg.FrontCell]
		s.mu.Unlock()
		if up {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the events stream never connected to a keyed llama-swap — in-flight evidence and every idle window built on it would be lost")
}

// ── U7: the credential never leaks ──────────────────────────────────────

// TestSwapKeyNeverAppearsInAnySurface is the redaction pin. It exercises
// every surface that could carry the value — the state document, the
// doctor report, the warm rows and the error strings from a rejection —
// and greps the rendered bytes for the key.
func TestSwapKeyNeverAppearsInAnySurface(t *testing.T) {
	const secret = "TOPSECRETKEYVALUE9999"
	front := newKeyedSwap(t, "a-different-key")
	s := newC15Server(t, front, hostsWithKey(t, front.srv.URL, secret))
	s.intentPath = filepath.Join(t.TempDir(), "intent.json")

	warmErr := s.warmViaFront(t.Context(), front.srv.URL, "default-model")
	if warmErr == nil {
		t.Fatal("expected the front to reject the key")
	}
	snap := s.Snapshot(t.Context())
	stateJSON, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	doctorJSON, err := json.Marshal(s.Doctor(t.Context()))
	if err != nil {
		t.Fatal(err)
	}
	for name, blob := range map[string]string{
		"warm error":     warmErr.Error(),
		"state document": string(stateJSON),
		"doctor report":  string(doctorJSON),
	} {
		if strings.Contains(blob, secret) {
			t.Errorf("%s carries the llama-swap API key", name)
		}
	}
	// ...and the diagnosis is still USEFUL: naming the config key is the
	// whole point, and a redaction that redacted everything would pass the
	// grep above while telling an operator nothing.
	if !strings.Contains(string(stateJSON), "swap_key_file") {
		t.Error("the state document redacted the diagnosis along with the key")
	}
}

// ── U8: every producer, enforced structurally ───────────────────────────

// TestEveryLlamaSwapRequestIsAuthorized is this phase's "a guard in one
// of four call paths is not a guard" enforcement. Every
// http.NewRequestWithContext in fleetapi builds a request to a cell's
// llama-swap, so every enclosing function must also call the one
// authorizer. A producer added later without the credential fails here
// rather than 401ing silently on the day someone sets apiKeys.
//
// fleetmcp's three producers are pinned by the twin of this test in that
// package.
//
// It caught C16's `/api/version` reader on the merge forward — a genuine
// cross-phase composition failure, found mechanically. That reader serves
// two postures (fleetd→front, and a cell reading its own localhost
// llama-swap), so it takes its authorizer as a nilable parameter and this
// scan sees the call; TestOnlyTheCellSideReaderSkipsSwapAuth below is
// what keeps `nil` from becoming a way around the scan.
func TestEveryLlamaSwapRequestIsAuthorized(t *testing.T) {
	// Four producers today: getJSON, readSwapVersion, warmViaFront,
	// streamCell. The floor is what makes a rename or a deletion visible
	// (C20) — raise it when a producer is added, never lower it to make a
	// removal quiet.
	assertRequestsAuthorized(t, ".", "AuthorizeSwap", 4)
}

// TestOnlyTheCellSideReaderSkipsSwapAuth guards the ONE exemption above.
//
// `readSwapVersion` authorizes when it is handed a Server and does not
// when it is handed nil, which is correct — a cell reading its own
// 127.0.0.1 llama-swap has no hosts.yaml to resolve a credential from
// (C15 §8) — and is also the shape of an escape hatch: a fleetd producer
// could satisfy the scan above by taking the same parameter and always
// receiving nil. So the nil is allowed in exactly one function, by name,
// and any other caller that passes it fails here.
//
// And the exemption is EXPORTED, which is a second door onto the same
// room: a fleetd producer in this package could reach the unauthenticated
// posture without writing `nil` at all, just by calling ReadOwnSwapVersion
// — invisible to the scan above (it builds no request of its own) and to
// the nil check (it names a different identifier). So this package must
// contain NO call to it: its callers are the cell-side ones, in other
// packages, and fleetd's path is frontSwapVersion.
func TestOnlyTheCellSideReaderSkipsSwapAuth(t *testing.T) {
	const reader, exempt = "readSwapVersion", "ReadOwnSwapVersion"
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read .: %v", err)
	}
	fset := token.NewFileSet()
	calls, unauthorized := 0, 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}
			ast.Inspect(fn.Body, func(m ast.Node) bool {
				call, ok := m.(*ast.CallExpr)
				if !ok {
					return true
				}
				id, ok := call.Fun.(*ast.Ident)
				if !ok {
					return true
				}
				if id.Name == exempt {
					t.Errorf("%s: %s calls %s. Nothing in this package may — it is the UNAUTHENTICATED posture, and a "+
						"fleetd producer reaching it drops the cell's credential without ever writing nil or building a "+
						"request the C15 scan can see. Read a cell's version through the authorizing path.", name, fn.Name.Name, exempt)
					return true
				}
				if id.Name != reader {
					return true
				}
				calls++
				if len(call.Args) < 2 {
					t.Errorf("%s: %s calls %s with %d args — the authorizer is the second one; "+
						"this scan cannot read the new shape", name, fn.Name.Name, reader, len(call.Args))
					return true
				}
				arg, ok := call.Args[1].(*ast.Ident)
				if !ok || arg.Name != "nil" {
					return true
				}
				unauthorized++
				if fn.Name.Name != exempt {
					t.Errorf("%s: %s reads a llama-swap's version with a nil authorizer. Only %s may — it is a box "+
						"reading its OWN localhost llama-swap (C15 §8). A fleetd→cell read passes the Server, or the "+
						"credential is silently dropped on the day someone sets apiKeys.", name, fn.Name.Name, exempt)
				}
				return true
			})
			return true
		})
	}
	// Inertness guards, both directions: a renamed reader would make this
	// scan pass over nothing, and an exemption that stopped existing would
	// make it pass over code that no longer needs it.
	if calls < 2 {
		t.Fatalf("found %d call(s) to %s — expected the fleetd path and the cell-side one; the scan is inert", calls, reader)
	}
	if unauthorized != 1 {
		t.Fatalf("found %d unauthorized %s call(s), want exactly 1 (%s)", unauthorized, reader, exempt)
	}
}

// assertRequestsAuthorized is C15's scan, expressed on C20's reusable
// rule engine (internal/astscan). Two things changed and neither weakens
// it: the inertness floor is now the ACTUAL producer count rather than
// "more than zero", so deleting three of the four producers is caught
// too; and the same declaration now serves fleetmcp's twin and the warm
// class rule below. The scan itself is mutation-pinned in
// internal/mutation's registry, in both packages.
func assertRequestsAuthorized(t *testing.T, dir, authorizer string, minProducers int) {
	t.Helper()
	r := astscan.Rule{
		Name:    "every fleetd→llama-swap request carries the cell's credential (C15)",
		Dir:     dir,
		Trigger: []string{"NewRequestWithContext", "NewRequest"},
		Require: []string{authorizer},
		// The nil-authorizer escape hatch is guarded separately and by
		// name in TestOnlyTheCellSideReaderSkipsSwapAuth: this rule sees
		// the CALL, that one sees what was passed.
		MinProducers: minProducers,
		Because: "every fleetd→llama-swap call carries the cell's credential, or it 401s silently " +
			"on the day someone sets apiKeys (C15).",
	}
	res, err := r.Check()
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Err(res); err != nil {
		t.Error(err)
	}
}

// ── U9: the doctor ──────────────────────────────────────────────────────

func TestDoctorSwapCredential(t *testing.T) {
	t.Run("declared and accepted", func(t *testing.T) {
		front := newKeyedSwap(t, labSwapKey)
		s := newC15Server(t, front, hostsWithKey(t, front.srv.URL, labSwapKey))
		c := doctorCheckByID(t, s.Doctor(t.Context()), "swap.credential")
		if c.Level != LevelOK {
			t.Fatalf("level = %s (%s)", c.Level, c.Summary)
		}
	})
	t.Run("demanded and undeclared is a FAIL", func(t *testing.T) {
		front := newKeyedSwap(t, labSwapKey)
		s := newC15Server(t, front, hostsWithKeyPath(t, front.srv.URL, ""))
		c := doctorCheckByID(t, s.Doctor(t.Context()), "swap.credential")
		if c.Level != LevelFail {
			t.Fatalf("level = %s (%s), want fail", c.Level, c.Summary)
		}
		if !strings.Contains(c.Fix, "swap_key_file") {
			t.Errorf("fix does not name the config: %q", c.Fix)
		}
	})
	t.Run("declared but unreadable is a FAIL naming the file", func(t *testing.T) {
		front := newKeyedSwap(t, "")
		missing := filepath.Join(t.TempDir(), "gone.key")
		s := newC15Server(t, front, hostsWithKeyPath(t, front.srv.URL, missing))
		c := doctorCheckByID(t, s.Doctor(t.Context()), "swap.credential")
		if c.Level != LevelFail || !strings.Contains(c.Detail, missing) {
			t.Fatalf("check = %+v, want a FAIL naming %s", c, missing)
		}
	})
	t.Run("unkeyed and answering is OK, not UNKNOWN", func(t *testing.T) {
		front := newKeyedSwap(t, "")
		s := newC15Server(t, front, hostsWithKeyPath(t, front.srv.URL, ""))
		c := doctorCheckByID(t, s.Doctor(t.Context()), "swap.credential")
		if c.Level != LevelOK {
			t.Fatalf("the reference posture (no apiKeys anywhere) scored %s: %s", c.Level, c.Summary)
		}
	})
	t.Run("silent and unkeyed is UNKNOWN, never OK", func(t *testing.T) {
		s := New([]Cell{{Name: fleetcfg.FrontCell, URL: "http://127.0.0.1:1", Class: "always_on"}},
			filepath.Join(t.TempDir(), "hist.json"), testDaemonInfo, Options{
				Hosts:      hostsWithKeyPath(t, "http://127.0.0.1:1", ""),
				IntentPath: filepath.Join(t.TempDir(), "intent.json"),
			})
		t.Cleanup(s.Close)
		c := doctorCheckByID(t, s.Doctor(t.Context()), "swap.credential")
		if c.Level != LevelUnknown {
			t.Fatalf("level = %s (%s), want unknown — an unreachable llama-swap and one refusing every route look identical", c.Level, c.Summary)
		}
	})
}

func doctorCheckByID(t *testing.T, rep DoctorReport, id string) DoctorCheck {
	t.Helper()
	for _, c := range rep.Checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no %s check in the report", id)
	return DoctorCheck{}
}

// ── U10: the render must not delete the credential ──────────────────────

// TestFrontRenderPreservesTheOperatorsExtras: the front's config is a
// derived artifact fleetd rewrites on every membership transition, and
// the renderer emits nothing it did not derive. Without the extras merge
// the first presence change deletes the front's apiKeys — a credential
// fleetd erases is not a credential.
func TestFrontRenderPreservesTheOperatorsExtras(t *testing.T) {
	dir := t.TempDir()
	extras := filepath.Join(dir, "front-extras.yaml")
	if err := os.WriteFile(extras, []byte("apiKeys:\n  - the-front-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	front := newKeyedSwap(t, labSwapKey)
	s := newC15Server(t, front, hostsWithKey(t, front.srv.URL, labSwapKey))

	var gotExtras string
	rl := &renderLoop{srv: s, cfg: RenderLoopConfig{
		FrontExtras:     extras,
		FrontConfigPath: filepath.Join(dir, "config.yaml"),
		LoadDefs:        func(string) ([]*profile.BackendDef, error) { return nil, nil },
		Render: func(_ []*profile.BackendDef, opts router.Options) (string, error) {
			gotExtras = opts.ExtrasPath
			return "models: {}\n", nil
		},
		WriteFile: func(string, []byte) error { return nil },
	}}
	if err := rl.renderPass(); err != nil {
		t.Fatalf("render pass: %v", err)
	}
	if gotExtras != extras {
		t.Fatalf("the render was given ExtrasPath %q, want %q — the front's apiKeys would be erased", gotExtras, extras)
	}
}

func TestDoctorFrontExtras(t *testing.T) {
	front := newKeyedSwap(t, labSwapKey)
	keyed := hostsWithKey(t, front.srv.URL, labSwapKey)
	unkeyed := hostsWithKeyPath(t, front.srv.URL, "")

	realExtras := filepath.Join(t.TempDir(), "front-extras.yaml")
	if err := os.WriteFile(realExtras, []byte("apiKeys:\n  - k\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ghost := filepath.Join(t.TempDir(), "not-mounted.yaml")

	for name, tc := range map[string]struct {
		hosts *fleetcfg.File
		host  DoctorHost
		want  Level
	}{
		"keyed front, rendered config, no extras": {keyed, DoctorHost{FrontConfig: "/front/config.yaml"}, LevelFail},
		"keyed front, rendered config, extras":    {keyed, DoctorHost{FrontConfig: "/front/config.yaml", FrontExtras: realExtras}, LevelOK},
		"keyed front, no render mount":            {keyed, DoctorHost{}, LevelOK},
		"unkeyed front, rendered config":          {unkeyed, DoctorHost{FrontConfig: "/front/config.yaml"}, LevelOK},
		// DECLARED is not MERGED: router.mergeExtras maps a missing file
		// to "no extras, no error", so a typo'd path erases the front's
		// apiKeys exactly as an unset front_extras would. Scoring it OK
		// made the check name something it did not prove.
		"keyed front, extras path that does not exist":   {keyed, DoctorHost{FrontConfig: "/front/config.yaml", FrontExtras: ghost}, LevelFail},
		"unkeyed front, extras path that does not exist": {unkeyed, DoctorHost{FrontConfig: "/front/config.yaml", FrontExtras: ghost}, LevelWarn},
	} {
		t.Run(name, func(t *testing.T) {
			s := newC15Server(t, front, tc.hosts)
			rep := DoctorReport{}
			s.checkFrontExtras(&rep, tc.host)
			got := doctorCheckByID(t, rep, "front.extras")
			if got.Level != tc.want {
				t.Fatalf("level = %s (%s), want %s", got.Level, got.Summary, tc.want)
			}
		})
	}
}

// ── U11: the page says a credential failure out loud ────────────────────

// TestFleetPageShowsTheCredentialFailure: a suppressed warm renders as
// "skipped" like every other skip, and the page is the surface an
// operator actually watches. The KIND and the cell belong there; the key
// and the file path do not (C12's rule that everything on the state
// document is guest-visible).
func TestFleetPageShowsTheCredentialFailure(t *testing.T) {
	page := fleetPageSource(t)
	for _, want := range []string{"st.swap_auth", "llama-swap credential"} {
		if !strings.Contains(page, want) {
			t.Errorf("fleet.html does not render %q — a fleet whose warms stopped says nothing on the page", want)
		}
	}
	// The page carries the KIND, not the sentence: the detail names
	// `cells.<name>.swap_key_file`, which is house configuration on a
	// surface anyone with the URL can read.
	if strings.Contains(page, "swap_auth") && strings.Contains(page, "c.detail") {
		t.Error("fleet.html renders the swap_auth detail sentence; the page gets the kind, fleet_status and doctor get the sentence")
	}
}
