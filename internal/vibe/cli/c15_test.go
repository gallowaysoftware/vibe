package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
)

// fleet-control C15: the CLI's DEGRADED path dials cells directly, and it
// is the path someone is on precisely when fleetd is already broken. A
// keyed cell that refuses this box's credential must not be rendered as a
// cell that is down.

func c15Hosts(t *testing.T, url, keyPath string) *fleetcfg.File {
	t.Helper()
	body := "cells:\n  front:\n    url: \"" + url + "\"\n    class: always_on\n"
	if keyPath != "" {
		body += "    swap_key_file: \"" + keyPath + "\"\n"
	}
	p := filepath.Join(t.TempDir(), "hosts.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := fleetcfg.LoadFrom(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return f
}

func keyedCell(t *testing.T, key string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if key != "" && r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, `{"error":"unauthorized: invalid or missing API key"}`, http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"lab-chat"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestProbeCellDirect_CarriesTheSwapKey(t *testing.T) {
	srv := keyedCell(t, "cli-key")
	dir := t.TempDir()
	kp := filepath.Join(dir, "k")
	if err := os.WriteFile(kp, []byte("cli-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	up, models, note := probeCellDirect(c15Hosts(t, srv.URL, kp), fleetcfg.FrontCell)
	if !up || models != "lab-chat" || note != "" {
		t.Fatalf("probe = (%v, %q, %q), want an authenticated read of the catalog", up, models, note)
	}
}

func TestProbeCellDirect_401IsNotDown(t *testing.T) {
	srv := keyedCell(t, "cli-key")
	up, _, note := probeCellDirect(c15Hosts(t, srv.URL, ""), fleetcfg.FrontCell)
	if up {
		t.Fatal("a 401 read as a successful probe")
	}
	if !strings.Contains(note, "swap_key_file") {
		t.Fatalf("note = %q — a refused credential must not be rendered as a dead cell", note)
	}
}

func TestProbeCellDirect_UnreadableKeyFailsClosed(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	missing := filepath.Join(t.TempDir(), "gone.key")
	up, _, note := probeCellDirect(c15Hosts(t, srv.URL, missing), fleetcfg.FrontCell)
	if up || !strings.Contains(note, "unreadable") {
		t.Fatalf("probe = (%v, %q), want a closed failure naming the key file", up, note)
	}
	if requests != 0 {
		t.Fatalf("%d request(s) went out unauthenticated", requests)
	}
}
