package fleetapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// fleet-control C16 — the upgrade ritual. Two questions, deliberately
// answered by different mechanisms:
//
//	front.image_pin       is the deployment PINNED (declared)
//	versions.llama_swap   what is it actually RUNNING (observed)
//
// plus the reference stack shipping a pin rather than recommending one.

func doctorCheck(t *testing.T, rep DoctorReport, id string) DoctorCheck {
	t.Helper()
	for _, c := range rep.Checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no %s check in the report (%d checks)", id, len(rep.Checks))
	return DoctorCheck{}
}

func newDoctorServer(t *testing.T, host DoctorHost, cells ...Cell) *Server {
	t.Helper()
	srv := New(cells, filepath.Join(t.TempDir(), "history.json"),
		func() DaemonInfo { return DaemonInfo{} },
		Options{IntentPath: filepath.Join(t.TempDir(), "intent.json")})
	srv.doctorHost = func() DoctorHost { return host }
	return srv
}

// TestFrontImagePin_LevelsAndReasons pins every branch of the declared
// half. The WARN branch is the incident: a floating tag is how a routine
// `docker compose pull` moved the fleet onto a llama-swap whose in-flight
// wire every busy guard misread.
func TestFrontImagePin_LevelsAndReasons(t *testing.T) {
	for _, tc := range []struct {
		name  string
		image string
		want  Level
		says  string
	}{
		{"digest pinned", "ghcr.io/mostlygeek/llama-swap:v239-cpu-b9994@sha256:6bae869ec09085", LevelOK, "digest-pinned"},
		{"digest only", "ghcr.io/mostlygeek/llama-swap@sha256:6bae869ec09085", LevelOK, "digest-pinned"},
		{"floating tag", "ghcr.io/mostlygeek/llama-swap:cpu", LevelWarn, "floating tag"},
		{"bare repo", "ghcr.io/mostlygeek/llama-swap", LevelWarn, "floating tag"},
		{"unmanaged", UnmanagedFrontImage, LevelOK, "not to run from a container image"},
		{"undeclared", "", LevelUnknown, "nothing declares"},
		{"control characters", "ghcr.io/x:cpu\nfake: ok", LevelFail, "control characters"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newDoctorServer(t, DoctorHost{FrontImage: tc.image})
			var rep DoctorReport
			srv.checkFrontImage(&rep, DoctorHost{FrontImage: tc.image})
			got := doctorCheck(t, rep, "front.image_pin")
			if got.Level != tc.want {
				t.Errorf("level = %q, want %q (%+v)", got.Level, tc.want, got)
			}
			if !strings.Contains(got.Summary+" "+got.Detail, tc.says) {
				t.Errorf("summary/detail %q + %q does not say %q", got.Summary, got.Detail, tc.says)
			}
		})
	}
}

// TestFrontImagePin_UndeclaredIsNotOK is the rule this repo keeps
// relearning stated once more: absent evidence may not read as health. An
// operator who has never wired the declaration must not see a green line
// about their floating tag.
func TestFrontImagePin_UndeclaredIsNotOK(t *testing.T) {
	srv := newDoctorServer(t, DoctorHost{})
	var rep DoctorReport
	srv.checkFrontImage(&rep, DoctorHost{})
	if got := doctorCheck(t, rep, "front.image_pin").Level; got == LevelOK {
		t.Fatalf("an undeclared front image scored %q; a check nobody configured must not read as a pinned deployment", got)
	}
}

// TestUngatedSwapVersions_NamesOnlyWhatNothingReplays covers the
// observed half's discriminator. A version with a recording is gated; one
// without is the state the fleet was in on 2026-08-05, with nothing
// anywhere saying so.
func TestUngatedSwapVersions_NamesOnlyWhatNothingReplays(t *testing.T) {
	got := ungatedSwapVersions(map[string][]string{
		"v239":            {"alpha"},
		"v247":            {"bravo"},
		"v239 (dd81801)":  {"echo"}, // a build string still names a gated release
		"v251":            {"charlie"},
		"v260 (deadbeef)": {"delta"},
	})
	// Reported VERBATIM, matched after normalisation: the operator needs
	// the string the cell actually said.
	want := []string{"v251", "v260 (deadbeef)"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ungatedSwapVersions = %v, want %v", got, want)
	}
	if len(ungatedSwapVersions(map[string][]string{"v239": {"alpha"}})) != 0 {
		t.Fatal("a recorded version was reported as ungated")
	}
}

// TestDoctor_UngatedVersionOutranksPlainDivergence drives the branch
// through the REAL report. It is ordered ahead of the divergence branch
// on purpose: a fleet uniformly on a version nothing here replays is not
// a mid-upgrade, it is an upgrade that skipped the ritual — and that is
// invisible to every other check, because a wire either parses or reads
// as an idle cell.
func TestDoctor_UngatedVersionOutranksPlainDivergence(t *testing.T) {
	// Uniform, but ungated: still a warning.
	s := versionFleet(t, map[string]*AnnounceVersions{
		"a": {LlamaSwap: "v260"},
		"b": {LlamaSwap: "v260"},
	})
	got := mustCheck(t, s.Doctor(context.Background()), "versions.llama_swap", "")
	if got.Level != LevelWarn {
		t.Fatalf("a uniform fleet on an ungated version → %s, want warn", got.Level)
	}
	if !strings.Contains(got.Summary, "v260") || !strings.Contains(got.Summary, "no conformance recording") {
		t.Errorf("summary = %q, want the ungated version named", got.Summary)
	}

	// Mixed, with one ungated: the ungated fact is the one to report.
	s2 := versionFleet(t, map[string]*AnnounceVersions{
		"a": {LlamaSwap: "v239"},
		"b": {LlamaSwap: "v260"},
	})
	got2 := mustCheck(t, s2.Doctor(context.Background()), "versions.llama_swap", "")
	if !strings.Contains(got2.Summary, "no conformance recording") {
		t.Errorf("summary = %q, want the ungated branch to outrank plain divergence", got2.Summary)
	}
	if strings.Contains(got2.Summary, "v239") {
		t.Errorf("summary = %q names a gated version as ungated", got2.Summary)
	}
}

// TestFrontSwapVersion_ObservesTheOneCellThatCannotAnnounce closes the
// structural hole: the front runs no announcer by design, and the front is
// the box the incident happened to.
func TestFrontSwapVersion_ObservesTheOneCellThatCannotAnnounce(t *testing.T) {
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/version" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"version":"v247","commit":"40027d6","build_date":"2026-08-04T05:36:51Z"}`))
	}))
	defer front.Close()

	srv := newDoctorServer(t, DoctorHost{}, Cell{Name: "front", URL: front.URL})
	if got := srv.frontSwapVersion(context.Background()); got != "v247" {
		t.Fatalf("frontSwapVersion = %q, want v247", got)
	}
}

// TestFrontSwapVersion_FailureIsAbsenceNotAgreement: an unreachable or
// nonsense answer yields "", which the matrix renders as a missing row.
// The alternative — inventing a value — would make one cell agree with
// whatever the others said.
func TestFrontSwapVersion_FailureIsAbsenceNotAgreement(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    http.HandlerFunc
	}{
		{"404", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }},
		{"not json", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("v247")) }},
		{"control characters", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("{\"version\":\"v2\\n47\"}"))
		}},
		{"absurdly long", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"version":"` + strings.Repeat("v", 500) + `"}`))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			front := httptest.NewServer(tc.h)
			defer front.Close()
			srv := newDoctorServer(t, DoctorHost{}, Cell{Name: "front", URL: front.URL})
			if got := srv.frontSwapVersion(context.Background()); got != "" {
				t.Fatalf("frontSwapVersion = %q, want empty", got)
			}
		})
	}
	// No front cell at all is also an absence, not a panic.
	srv := newDoctorServer(t, DoctorHost{}, Cell{Name: "alpha", URL: "http://127.0.0.1:1"})
	if got := srv.frontSwapVersion(context.Background()); got != "" {
		t.Fatalf("frontSwapVersion with no front cell = %q, want empty", got)
	}
}

// TestReferenceFrontStackShipsADigestPin is the whole first half of this
// phase, mechanically. deploy/front/README.md RECOMMENDED pinning while
// docker-compose.yaml floated the tag, and advice that the shipped default
// contradicts is advice nobody follows.
//
// Offline by construction: it reads the checked-in files and asserts the
// SHAPE of the default. CI has no network and cannot resolve a digest.
func TestReferenceFrontStackShipsADigestPin(t *testing.T) {
	root := repoRoot(t)
	// The compose default — what a deployment with no .env override runs.
	compose, err := os.ReadFile(filepath.Join(root, "deploy", "front", "docker-compose.yaml"))
	if err != nil {
		t.Fatalf("read compose: %v", err)
	}
	m := regexp.MustCompile(`(?m)^\s*image:\s*\$\{FRONT_IMAGE:-([^}]+)\}`).FindSubmatch(compose)
	if m == nil {
		t.Fatal("deploy/front/docker-compose.yaml has no ${FRONT_IMAGE:-default} image line")
	}
	if !digestPinned(string(m[1])) {
		t.Errorf("the reference front's DEFAULT image is %q, which floats. "+
			"A `docker compose pull` then decides which llama-swap the fleet runs, which is exactly "+
			"how v247 arrived on a fleet gated on v239.", m[1])
	}

	// And the .env the operator actually edits.
	env, err := os.ReadFile(filepath.Join(root, "deploy", "front", ".env.example"))
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}
	found := false
	for _, line := range strings.Split(string(env), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		val, ok := strings.CutPrefix(trimmed, "FRONT_IMAGE=")
		if !ok {
			continue
		}
		found = true
		if !digestPinned(val) {
			t.Errorf("deploy/front/.env.example sets FRONT_IMAGE=%q, which floats", val)
		}
	}
	if !found {
		t.Error("deploy/front/.env.example no longer sets FRONT_IMAGE; the example is what gets copied")
	}
}

// TestUpgradeRitualIsRunnable: the ritual is a checked-in script, not
// prose. Futures item 13 asked for the six-client gate to stay "a
// checked-in runnable script"; this is the same requirement one level up.
func TestUpgradeRitualIsRunnable(t *testing.T) {
	path := filepath.Join(repoRoot(t), "scripts", "upgrade", "ritual.sh")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("scripts/upgrade/ritual.sh: %v", err)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("%s is not executable (%s)", path, fi.Mode())
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Each step must actually invoke the rig it claims to compose;
	// a ritual that only prints instructions is the prose it replaced.
	for _, want := range []string{
		"TestSwapContract",              // the wire
		"TestSwapBehaviour",             // the keepalive + SIGTERM behaviours
		"TestRecord",                    // the new version's fixtures
		"scripts/fleetlab",              // a real multi-cell fleet on the candidate
		"scripts/smoke/llama-swap",      // the six-client SSE cold-start gate
		"ghcr.io/mostlygeek/llama-swap", // the digest it prints
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("ritual.sh never mentions %q; the step it belongs to is prose, not a command", want)
		}
	}
}
