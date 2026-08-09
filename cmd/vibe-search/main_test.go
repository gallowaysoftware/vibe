package main

import (
	"strings"
	"testing"
)

// The old default was warn-and-serve: no VIBE_SEARCH_TOKEN logged a line and
// then answered every route for anyone who could reach the port. This pins
// the replacement — refuse to start, unless the operator says --no-auth.
func TestStartupRefusesToServeWithoutATokenUnlessNoAuth(t *testing.T) {
	warn, err := startupAuthCheck("", false, "127.0.0.1:14003")
	if err == nil {
		t.Fatal("a token-less start was allowed: every route fetches URLs and spends quota for whoever reaches it")
	}
	// The message has to carry the way out, or the operator's next move is to
	// go read the source.
	for _, want := range []string{"VIBE_SEARCH_TOKEN", "--no-auth"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if warn != "" {
		t.Errorf("warn = %q, want none alongside a refusal", warn)
	}

	// A token is all it takes, on any bind.
	for _, bind := range []string{"127.0.0.1:14003", "0.0.0.0:14003"} {
		if w, err := startupAuthCheck("s3cret", false, bind); err != nil || w != "" {
			t.Errorf("startupAuthCheck(token, false, %q) = (%q, %v), want a clean start", bind, w, err)
		}
	}

	// --no-auth on loopback is the deliberate, contained case: allowed, and
	// quiet, because the operator already said it out loud.
	if w, err := startupAuthCheck("", true, "127.0.0.1:14003"); err != nil || w != "" {
		t.Errorf("--no-auth on loopback = (%q, %v), want a silent start", w, err)
	}
}

// --no-auth plus a bind past this machine is the exact combination the
// finding was written about. It still starts — one explicit flag is the
// escape hatch — but it may not do so quietly.
func TestNoAuthBeyondLoopbackIsAnnounced(t *testing.T) {
	for _, bind := range []string{"0.0.0.0:14003", ":14003", "[::]:14003", "192.168.1.10:14003", "search.internal:14003", "garbage"} {
		w, err := startupAuthCheck("", true, bind)
		if err != nil {
			t.Errorf("startupAuthCheck(\"\", true, %q) = %v, want a start with a warning", bind, err)
		}
		if w == "" {
			t.Errorf("--no-auth on %q started silently: an unauthenticated fetch proxy reachable from the LAN says nothing about itself", bind)
		}
	}
}

func TestBindsBeyondLoopback(t *testing.T) {
	local := []string{"127.0.0.1:14003", "127.0.0.5:14003", "localhost:14003", "[::1]:14003"}
	for _, bind := range local {
		if bindsBeyondLoopback(bind) {
			t.Errorf("bindsBeyondLoopback(%q) = true, want false", bind)
		}
	}
	// Unparseable and unresolvable both read as exposed on purpose: the
	// wrong answer here is the one that stays quiet about an open port.
	exposed := []string{"0.0.0.0:14003", ":14003", "[::]:14003", "10.0.0.4:14003", "search.example:14003", "", "nonsense"}
	for _, bind := range exposed {
		if !bindsBeyondLoopback(bind) {
			t.Errorf("bindsBeyondLoopback(%q) = false, want true", bind)
		}
	}
}
