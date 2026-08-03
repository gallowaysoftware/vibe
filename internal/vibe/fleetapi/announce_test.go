package fleetapi

// C3 announce-endpoint coverage: schema validation, presence upsert and
// transitions (returned/stale/withdrawn), the conflict rule (echo newer
// than the request wins; newer request is handed back as desired), and
// command drain.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func postAnnounce(t *testing.T, ts *httptest.Server, body string) (int, AnnounceResponse) {
	t.Helper()
	resp, err := http.Post(ts.URL+"/api/fleet/announce", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST announce: %v", err)
	}
	defer resp.Body.Close()
	var out AnnounceResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func newAnnounceServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	cells := []Cell{
		{Name: "front", URL: "http://127.0.0.1:1", Class: "always_on"},
		{Name: "laptop", URL: "http://127.0.0.1:1", Class: "roaming"},
	}
	_, ts, _ := newFleetdServer(t, cells)
	return nil, ts
}

func TestAnnounceValidation(t *testing.T) {
	_, ts := newAnnounceServer(t)

	code, _ := postAnnounce(t, ts, `{"v":2,"cell":"laptop","seq":1}`)
	if code != http.StatusBadRequest {
		t.Errorf("v=2: HTTP %d, want 400", code)
	}
	code, _ = postAnnounce(t, ts, `{"v":1,"cell":"typo","seq":1}`)
	if code != http.StatusBadRequest {
		t.Errorf("unknown cell: HTTP %d, want 400", code)
	}
	// Unknown fields tolerated (the version-skew rule).
	code, _ = postAnnounce(t, ts, `{"v":1,"cell":"laptop","seq":1,"future_field":{"nested":true}}`)
	if code != http.StatusOK {
		t.Errorf("unknown fields must be tolerated: HTTP %d", code)
	}
}

func TestAnnouncePresenceTransitions(t *testing.T) {
	s, ts := newAnnounceServerWithFleet(t)

	code, _ := postAnnounce(t, ts, `{"v":1,"cell":"laptop","seq":1,"intent":{"state":"serving"},"models":[{"id":"qwen","state":"ready"}]}`)
	if code != http.StatusOK {
		t.Fatalf("announce: HTTP %d", code)
	}
	p := s.presenceFor("laptop")
	if p == nil || !p.Announcing || p.Stale || p.Withdrawn {
		t.Fatalf("presence = %+v", p)
	}
	if p.HealthyStreak != 1 || len(p.Models) != 1 {
		t.Errorf("presence = %+v", p)
	}

	// Stale after the interval lapses (test clock: back-date received_at
	// and let the REAL loop run at a test cadence).
	s.stalenessTick = 10 * time.Millisecond
	s.Start()
	s.mu.Lock()
	s.presence["laptop"].ReceivedAt = time.Now().Add(-time.Hour)
	s.mu.Unlock()
	waitStale(t, s, "laptop")
	p = s.presenceFor("laptop")
	if !p.Stale || p.HealthyStreak != 0 {
		t.Errorf("after stale window: %+v", p)
	}

	// Return restores it and bumps the streak from zero.
	code, _ = postAnnounce(t, ts, `{"v":1,"cell":"laptop","seq":2,"intent":{"state":"serving"}}`)
	if code != http.StatusOK {
		t.Fatal("re-announce failed")
	}
	p = s.presenceFor("laptop")
	if p.Stale || p.HealthyStreak != 1 {
		t.Errorf("after return: %+v", p)
	}

	// Clean withdraw.
	postAnnounce(t, ts, `{"v":1,"cell":"laptop","seq":3,"intent":{"state":"withdrawing"}}`)
	p = s.presenceFor("laptop")
	if !p.Withdrawn {
		t.Errorf("withdrawing intent must mark withdrawn: %+v", p)
	}
}

func TestAnnounceConflictRule(t *testing.T) {
	s, ts := newAnnounceServerWithFleet(t)

	// Registry has a drained request NEWER than the cell's echo: handed
	// back as desired_intent.
	if _, err := s.SetIntent("laptop", "drained", "via MCP", "23:00"); err != nil {
		t.Fatal(err)
	}
	_, resp := postAnnounce(t, ts, `{"v":1,"cell":"laptop","seq":1,"intent":{"state":"serving","since":"2020-01-01T00:00:00Z"}}`)
	if resp.DesiredIntent == nil || resp.DesiredIntent.State != "drained" {
		t.Errorf("desired = %+v, want the drained request handed back", resp.DesiredIntent)
	}

	// The cell echoes drained NEWER than the request: request satisfied,
	// no desired in the response.
	s.mu.Lock()
	s.intents["laptop"] = Intent{State: "drained", Reason: "via MCP", Since: time.Now().UTC().Add(-time.Minute)}
	s.mu.Unlock()
	now := time.Now().UTC()
	body := `{"v":1,"cell":"laptop","seq":2,"intent":{"state":"drained","since":"` + now.Format(time.RFC3339Nano) + `"}}`
	_, resp = postAnnounce(t, ts, body)
	if resp.DesiredIntent != nil {
		t.Errorf("desired after echo = %+v, want nil (request satisfied)", resp.DesiredIntent)
	}

	// A NEWER local change at the cell (human resumed it): the request
	// is dropped registry-side — the box you're standing at wins.
	if _, err := s.SetIntent("laptop", "drained", "stale request", ""); err != nil {
		t.Fatal(err)
	}
	newer := time.Now().UTC().Add(time.Minute)
	body = `{"v":1,"cell":"laptop","seq":3,"intent":{"state":"serving","since":"` + newer.Format(time.RFC3339Nano) + `"}}`
	_, resp = postAnnounce(t, ts, body)
	if resp.DesiredIntent != nil {
		t.Errorf("stale request survived a newer echo: %+v", resp.DesiredIntent)
	}
	s.mu.Lock()
	_, stillThere := s.intents["laptop"]
	s.mu.Unlock()
	if stillThere {
		t.Error("stale request not dropped after newer echo")
	}
}

func TestAnnounceCommandsDrained(t *testing.T) {
	s, ts := newAnnounceServerWithFleet(t)
	s.queueCommand("laptop", AnnounceCommand{Verb: "unload", Model: "qwen"})
	s.queueCommand("laptop", AnnounceCommand{Verb: "warm", Model: "gemma"})

	_, resp := postAnnounce(t, ts, `{"v":1,"cell":"laptop","seq":1}`)
	if len(resp.Commands) != 2 {
		t.Fatalf("commands = %+v", resp.Commands)
	}
	// Drained: the next announce gets an empty queue.
	_, resp = postAnnounce(t, ts, `{"v":1,"cell":"laptop","seq":2}`)
	if len(resp.Commands) != 0 {
		t.Errorf("commands not drained: %+v", resp.Commands)
	}
}

// newAnnounceServerWithFleet returns the fleet server too (the tests
// poke presence directly).
func newAnnounceServerWithFleet(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	cells := []Cell{
		{Name: "front", URL: "http://127.0.0.1:1", Class: "always_on"},
		{Name: "laptop", URL: "http://127.0.0.1:1", Class: "roaming"},
	}
	s, ts, _ := newFleetdServer(t, cells)
	return s, ts
}
