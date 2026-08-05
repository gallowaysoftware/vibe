package cli

// C13 coverage for the desk path: the exit codes MEAN something, the
// renderer puts the finding first, `--json` is the same document the
// route serves, and a doctor run against a dead fleetd still tells the
// operator something — that last one matters because killing fleetd is
// step one of the fire drill this command exists for.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
)

func doctorFleetd(t *testing.T, rep fleetapi.DoctorReport) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/fleet/doctor", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rep)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func runFleetDoctor(t *testing.T, api string, args ...string) (string, error) {
	t.Helper()
	cmd := fleetDoctorCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append([]string{"--api", api}, args...))
	err := cmd.Execute()
	return out.String(), err
}

func reportWith(levels ...fleetapi.Level) fleetapi.DoctorReport {
	rep := fleetapi.DoctorReport{}
	for i, l := range levels {
		rep.Add(fleetapi.DoctorCheck{ID: "check", Cell: string(rune('a' + i)), Level: l, Summary: "s"})
	}
	return rep
}

// TestFleetDoctorExitCodesMeanSomething. 3 is deliberately not success:
// a fleet with a laptop out of the house produces an INCOMPLETE report,
// and the code says so instead of pretending the questions were asked.
func TestFleetDoctorExitCodesMeanSomething(t *testing.T) {
	t.Setenv("VIBE_TOKEN", "")
	for _, tc := range []struct {
		name string
		rep  fleetapi.DoctorReport
		want int
	}{
		{"all ok", reportWith(fleetapi.LevelOK, fleetapi.LevelOK), 0},
		{"a fail outranks everything", reportWith(fleetapi.LevelOK, fleetapi.LevelWarn, fleetapi.LevelUnknown, fleetapi.LevelFail), 1},
		{"a warn without a fail", reportWith(fleetapi.LevelOK, fleetapi.LevelUnknown, fleetapi.LevelWarn), 2},
		{"only unknowns: the report is incomplete", reportWith(fleetapi.LevelOK, fleetapi.LevelUnknown), 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := doctorFleetd(t, tc.rep)
			_, err := runFleetDoctor(t, ts.URL)
			if got := ExitCode(err); got != tc.want {
				t.Fatalf("exit = %d (err %v), want %d", got, err, tc.want)
			}
			if tc.want == 0 && err != nil {
				t.Fatalf("clean report returned %v", err)
			}
		})
	}
}

// TestFleetDoctorExitZeroSuppressesTheCode: for a wrapper that reads the
// output itself.
func TestFleetDoctorExitZeroSuppressesTheCode(t *testing.T) {
	t.Setenv("VIBE_TOKEN", "")
	ts := doctorFleetd(t, reportWith(fleetapi.LevelFail))
	if _, err := runFleetDoctor(t, ts.URL, "--exit-zero"); err != nil {
		t.Fatalf("--exit-zero returned %v", err)
	}
}

// TestFleetDoctorRendersFindingsFirstAndKeepsTheOKBlock: a tired human
// reads top-down, and "I checked these things and they are fine" is half
// of what a sit-down command is for.
func TestFleetDoctorRendersFindingsFirstAndKeepsTheOKBlock(t *testing.T) {
	t.Setenv("VIBE_TOKEN", "")
	rep := fleetapi.DoctorReport{}
	rep.Add(fleetapi.DoctorCheck{ID: "auth.inbound", Cell: "mac", Level: fleetapi.LevelOK, Summary: "fine"})
	rep.Add(fleetapi.DoctorCheck{ID: "auth.outbound", Cell: "gpu", Level: fleetapi.LevelFail,
		Summary: "the cell daemon REFUSED", Detail: "source: cells.gpu.token_file", Fix: "fix me"})
	ts := doctorFleetd(t, rep)

	out, err := runFleetDoctor(t, ts.URL)
	if ExitCode(err) != 1 {
		t.Fatalf("exit = %d", ExitCode(err))
	}
	failAt, okAt := strings.Index(out, "auth.outbound"), strings.Index(out, "auth.inbound")
	if failAt < 0 || okAt < 0 || failAt > okAt {
		t.Fatalf("FAIL must render before OK:\n%s", out)
	}
	for _, want := range []string{"FAIL", "token_file", "fix: fix me", "1 FAIL"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	quiet, _ := runFleetDoctor(t, ts.URL, "--problems-only")
	if strings.Contains(quiet, "auth.inbound") {
		t.Errorf("--problems-only still printed the OK block:\n%s", quiet)
	}
}

// TestFleetDoctorJSONIsTheServedDocument: one shape for humans, agents
// and scripts (C9's rule that every surface renders one document).
func TestFleetDoctorJSONIsTheServedDocument(t *testing.T) {
	t.Setenv("VIBE_TOKEN", "")
	rep := fleetapi.DoctorReport{}
	rep.Add(fleetapi.DoctorCheck{ID: "defs.parity", Level: fleetapi.LevelWarn, Summary: "cells disagree"})
	ts := doctorFleetd(t, rep)

	out, err := runFleetDoctor(t, ts.URL, "--json")
	if ExitCode(err) != 2 {
		t.Fatalf("exit = %d", ExitCode(err))
	}
	var back fleetapi.DoctorReport
	if err := json.Unmarshal([]byte(out), &back); err != nil {
		t.Fatalf("--json did not emit the report: %v\n%s", err, out)
	}
	// The client adds its own check to the server's report, so the
	// document that lands is the union — which is the point: one
	// document, whichever half of the boundary a check came from.
	ids := map[string]bool{}
	for _, c := range back.Checks {
		ids[c.ID] = true
	}
	if !ids["defs.parity"] || !ids["auth.client_env"] || back.Summary.Warn != 1 {
		t.Errorf("round trip = %+v", back)
	}
	if back.Fleetd != ts.URL {
		t.Errorf("fleetd = %q, want the address that answered", back.Fleetd)
	}
}

// TestFleetDoctorDegradesWhenFleetdIsDown is the fire drill's first
// step: a doctor that fails blind exactly when fleetd dies is the wrong
// tool. fleetd unreachable is a FAIL, every fleet-side check is UNKNOWN
// with the reason, and the client-side checks still run.
func TestFleetDoctorDegradesWhenFleetdIsDown(t *testing.T) {
	t.Setenv("VIBE_TOKEN", "")
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := dead.URL
	dead.Close()

	writeHosts(t, `
cells:
  front:    { url: "http://127.0.0.1:1", class: always_on }
  gpu-cell: { url: "http://127.0.0.1:1", class: opportunistic }
`)
	out, err := runFleetDoctor(t, url)
	if ExitCode(err) != 1 {
		t.Fatalf("exit = %d, want 1 (fleetd down is a FAIL)", ExitCode(err))
	}
	for _, want := range []string{"fleetd.reachable", "fleetd.checks", "UNKNOWN", "hosts.yaml", "auth.client_env"} {
		if !strings.Contains(out, want) {
			t.Errorf("degraded report missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "read-and-request-only") {
		t.Errorf("the degraded report must say serving is unaffected:\n%s", out)
	}
}

// TestFleetDoctorClientEnvCheckSurfacesTheLiveHalfOfC6sFinding: the
// CLI's precedence is env-first BY DESIGN, so an exported token in this
// shell silently replaces every cells.X.token_file for every command run
// from it. fleetd's half was fixed in C6; this half is still live and
// only a doctor can tell you.
func TestFleetDoctorClientEnvCheckSurfacesTheLiveHalfOfC6sFinding(t *testing.T) {
	writeHosts(t, `
cells:
  front:    { url: "http://127.0.0.1:1", class: always_on }
  gpu-cell: { url: "http://127.0.0.1:1", class: opportunistic,
              daemon_url: "http://127.0.0.1:1", token_file: "/tmp/nope-c13" }
`)
	t.Setenv("VIBE_TOKEN", "shadowing-everything")
	rep := &fleetapi.DoctorReport{}
	clientDoctorChecks(rep)
	var got fleetapi.DoctorCheck
	for _, c := range rep.Checks {
		if c.ID == "auth.client_env" {
			got = c
		}
	}
	if got.Level != fleetapi.LevelWarn {
		t.Fatalf("level = %q, want warn: $VIBE_TOKEN is overriding a declared token_file", got.Level)
	}
	if !strings.Contains(got.Detail, "gpu-cell") {
		t.Errorf("detail = %q, want the shadowed cell named", got.Detail)
	}

	t.Setenv("VIBE_TOKEN", "")
	rep2 := &fleetapi.DoctorReport{}
	clientDoctorChecks(rep2)
	for _, c := range rep2.Checks {
		if c.ID == "auth.client_env" && c.Level != fleetapi.LevelOK {
			t.Errorf("no env token → %q, want ok", c.Level)
		}
	}
}

// ─── review-pass regressions ────────────────────────────────────────────────

// TestFleetDoctorFetchIgnoresTheStateSizedTimeout pins REV-4, the one
// misdiagnosis this command must not make. The shared fleetd client's
// 10s timeout is sized for /api/fleet/state; the doctor report fans out
// to every cell and every https endpoint and is bounded server-side at
// 20s, so reusing that client reports a SLOW fleetd as a DEAD one.
func TestFleetDoctorFetchIgnoresTheStateSizedTimeout(t *testing.T) {
	t.Setenv("VIBE_TOKEN", "")
	rep := fleetapi.DoctorReport{}
	rep.Add(fleetapi.DoctorCheck{ID: "slow.but.alive", Level: fleetapi.LevelOK, Summary: "answered"})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/fleet/doctor", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(120 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(rep)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	target, err := resolveFleetd(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	// Stand in for "the report outlives the state-sized budget".
	target.hc = &http.Client{Timeout: 20 * time.Millisecond, Transport: target.hc.Transport}

	got, err := target.fetchDoctor(t.Context())
	if err != nil {
		t.Fatalf("fetchDoctor honoured the client timeout and reported a live fleetd as dead: %v", err)
	}
	if len(got.Checks) != 1 || got.Checks[0].ID != "slow.but.alive" {
		t.Errorf("report = %+v", got)
	}
}

// TestFleetDoctorNamesTheLocalSocketRatherThanItsPlaceholderHost pins
// REV-5: the unix-socket target carries a placeholder host so the URL
// parses, and printing it as an address invites someone to curl it.
func TestFleetDoctorNamesTheLocalSocketRatherThanItsPlaceholderHost(t *testing.T) {
	rep := &fleetapi.DoctorReport{Fleetd: "http://vibe.local"}
	rep.Add(fleetapi.DoctorCheck{ID: "x", Level: fleetapi.LevelOK, Summary: "s"})
	var out bytes.Buffer
	renderDoctor(&out, rep, false)
	if strings.Contains(out.String(), "vibe.local") {
		t.Errorf("rendered the placeholder host as an address:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "unix socket") {
		t.Errorf("output does not name the local socket:\n%s", out.String())
	}
}
