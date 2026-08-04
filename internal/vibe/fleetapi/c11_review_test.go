package fleetapi

// C11 adversarial review. Three gaps between what a hold PROMISES and
// what the substrate does with it: the piggyback queue delivers a warm
// the hold was declared to prevent, a release reports success it did not
// earn, and the remaining-time string had two implementations.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestHold_DropsQueuedPolicyWarmsAtDeliveryKeepsOperatorVerbs: the warm
// loops check the hold when they DECIDE, but the piggyback queue is
// at-least-once and delivers later. A restore queued one tick before the
// operator declared the hold would still land on the next announce and
// evict the model the hold exists to protect — the sending side guards,
// the receiving side does not, which is this repo's most repeated defect.
// `unload` is an operator's verb and still rides through.
func TestHold_DropsQueuedPolicyWarmsAtDeliveryKeepsOperatorVerbs(t *testing.T) {
	s, _, _ := holdWarmTarget(t, "heavy", "default-model", time.Millisecond)
	presenceOf(s, "heavy",
		AnnounceModel{ID: "default-model", State: "stopped"},
		AnnounceModel{ID: "challenger", State: "ready"})

	// Positive control: with no hold, a queued warm is delivered. Without
	// it, dropping the guard would leave a test that passes against a
	// queue that never delivers anything.
	if err := s.QueueCommand("heavy", AnnounceCommand{Verb: "warm", Model: "default-model"}); err != nil {
		t.Fatalf("QueueCommand: %v", err)
	}
	if got := s.drainCommands("heavy", 1); len(got) != 1 || got[0].Verb != "warm" {
		t.Fatalf("commands = %+v, want the warm delivered with no hold", got)
	}

	if err := s.QueueCommand("heavy", AnnounceCommand{Verb: "warm", Model: "default-model"}); err != nil {
		t.Fatalf("QueueCommand warm: %v", err)
	}
	if err := s.QueueCommand("heavy", AnnounceCommand{Verb: "unload", Model: "challenger"}); err != nil {
		t.Fatalf("QueueCommand unload: %v", err)
	}
	if _, err := s.SetHold("heavy", "challenger", "evaluating", time.Hour); err != nil {
		t.Fatalf("SetHold: %v", err)
	}
	got := s.drainCommands("heavy", 2)
	for _, c := range got {
		if c.Verb == "warm" {
			t.Fatalf("a queued warm was delivered to a HELD cell — the eviction the hold forbids: %+v", got)
		}
	}
	if len(got) != 1 || got[0].Verb != "unload" {
		t.Fatalf("commands = %+v, want the operator's unload still delivered", got)
	}

	// The at-least-once in-flight slot is the same hazard one step later:
	// a batch handed out BEFORE the hold is redelivered until an announce
	// with a HIGHER seq retires it, so the drop has to reach that slot too.
	if _, err := s.ReleaseHold("heavy", "challenger"); err != nil {
		t.Fatalf("ReleaseHold: %v", err)
	}
	if err := s.QueueCommand("heavy", AnnounceCommand{Verb: "warm", Model: "default-model"}); err != nil {
		t.Fatalf("QueueCommand: %v", err)
	}
	if got := s.drainCommands("heavy", 3); len(got) != 1 {
		t.Fatalf("commands = %+v, want the warm handed out while unheld", got)
	}
	if _, err := s.SetHold("heavy", "challenger", "evaluating", time.Hour); err != nil {
		t.Fatalf("SetHold: %v", err)
	}
	if got := s.drainCommands("heavy", 3); len(got) != 0 {
		t.Fatalf("redelivered a warm to a held cell from the in-flight slot: %+v", got)
	}
}

// TestHold_ReleaseReportsWhetherAHoldWasThere: DELETE /api/fleet/lease is
// idempotent, but "deleted" after a mistyped MODEL tells an operator the
// warm policy is running again while the real hold keeps suppressing it.
// dropLease always knew; only the wire did not say.
func TestHold_ReleaseReportsWhetherAHoldWasThere(t *testing.T) {
	s, ts, _ := newFleetdServer(t, []Cell{
		{Name: "front", URL: "http://127.0.0.1:1"},
		{Name: "heavy", URL: "http://127.0.0.1:2", Class: "always_on"},
	})
	if _, err := s.SetHold("heavy", "challenger", "evaluating", time.Hour); err != nil {
		t.Fatalf("SetHold: %v", err)
	}

	del := func(model string) bool {
		t.Helper()
		req, err := http.NewRequest(http.MethodDelete, ts.URL+"/api/fleet/lease",
			strings.NewReader(`{"cell":"heavy","model":"`+model+`","holder":"`+HoldHolder+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("DELETE = %d: %s", resp.StatusCode, body)
		}
		var out struct {
			Status  string `json:"status"`
			Existed bool   `json:"existed"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		if out.Status != "deleted" {
			t.Errorf("status = %q, want deleted (the shape C2 clients read)", out.Status)
		}
		return out.Existed
	}

	if del("challengerr") {
		t.Error("a typo'd model reported that it deleted something")
	}
	if _, held := s.HoldOn("heavy"); !held {
		t.Fatal("the typo'd release removed the real hold")
	}
	if !del("challenger") {
		t.Error("releasing the real hold reported existed=false")
	}
	if _, held := s.HoldOn("heavy"); held {
		t.Error("the hold survived its release")
	}
}

// TestHoldLeft_ReadsInMinutesAndNeverGoesNegative: one implementation of
// the remaining-time string, because the CLI column, the warm status and
// the MCP reply all say it and three copies is three ways to disagree.
func TestHoldLeft_ReadsInMinutesAndNeverGoesNegative(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{2 * time.Hour, "2h0m left"},
		{90 * time.Minute, "1h30m left"},
		{45 * time.Minute, "45m left"},
		{30 * time.Second, "30s left"},
		{-time.Hour, "0s left"},
	} {
		if got := HoldLeft(now.Add(tc.in)); got != tc.want {
			t.Errorf("HoldLeft(now%+s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
