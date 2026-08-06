package fleetapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
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

// ── C15 composition: the version read is a fleetd→llama-swap call ───────
//
// This phase's reader landed on a branch cut before C15 and merged after
// it, and the fleetd half went out with no credential. C15's structural
// scan caught it; these four state what the scan cannot — what the wire
// carries, what happens when it cannot be built, and where the refusal is
// recorded. The helpers are C15's (keyedSwap, hostsWith*), deliberately:
// a second llama-swap double with a second idea of what apiKeys means is
// how the two halves would drift again.

// TestFrontSwapVersion_CarriesTheFrontsCredential is the regression. A
// keyed front 401s /api/version exactly as it 401s /running, and this
// reader renders a 401 as "" — so an unauthenticated read makes the
// version matrix go quiet about the ONE box C16 exists for, on a fleet
// whose only fault was setting a key.
func TestFrontSwapVersion_CarriesTheFrontsCredential(t *testing.T) {
	front := newKeyedSwap(t, labSwapKey)
	s := newC15Server(t, front, hostsWithKey(t, front.srv.URL, labSwapKey))
	if got := s.frontSwapVersion(t.Context()); got != "v239" {
		t.Fatalf("frontSwapVersion through a keyed front = %q, want v239", got)
	}
	req := <-front.seen
	if req.URL.Path != "/api/version" {
		t.Fatalf("first request was %s, want /api/version", req.URL.Path)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer "+labSwapKey {
		t.Fatalf("Authorization = %q, want the front's declared key as a bearer", got)
	}
}

// TestFrontSwapVersion_UnresolvableCredentialSendsNothing: AuthorizeSwap's
// contract is that a DECLARED key which will not resolve means no request
// is sent at all. Sending it unauthenticated instead would turn an
// operator typo into an opaque 401 from a box across the house — and here
// it would additionally read as "the front did not answer", which is a
// sentence about the front rather than about hosts.yaml.
func TestFrontSwapVersion_UnresolvableCredentialSendsNothing(t *testing.T) {
	front := newKeyedSwap(t, labSwapKey)
	missing := filepath.Join(t.TempDir(), "gone.key")
	s := newC15Server(t, front, hostsWithKeyPath(t, front.srv.URL, missing))

	if got := s.frontSwapVersion(t.Context()); got != "" {
		t.Fatalf("frontSwapVersion = %q, want absence", got)
	}
	if n := front.requests(); n != 0 {
		t.Fatalf("the front saw %d request(s) with an unresolvable credential declared, want 0 — fail CLOSED", n)
	}
	f, bad := s.swapAuthState(fleetcfg.FrontCell)
	if !bad || f.Kind != SwapAuthUnresolvable {
		t.Fatalf("swap auth state = %+v (recorded=%v), want %s", f, bad, SwapAuthUnresolvable)
	}
}

// TestFrontSwapVersion_UnkeyedFleetIsUntouched: the reference fleet
// declares no key anywhere, llama-swap runs without apiKeys, and the read
// must work with no header at all. A credential half of a feature that
// breaks the posture 99% of this fleet runs is not a fix.
func TestFrontSwapVersion_UnkeyedFleetIsUntouched(t *testing.T) {
	front := newKeyedSwap(t, "")
	s := newC15Server(t, front, hostsWithKeyPath(t, front.srv.URL, ""))
	if got := s.frontSwapVersion(t.Context()); got != "v239" {
		t.Fatalf("frontSwapVersion on an unkeyed fleet = %q, want v239", got)
	}
	req := <-front.seen
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization = %q on a fleet that declares no key, want none", got)
	}
	if _, bad := s.swapAuthState(fleetcfg.FrontCell); bad {
		t.Fatal("a successful unkeyed read recorded a credential failure")
	}
}

// TestFrontSwapVersion_401FeedsTheCredentialMachinery: the status folds
// into the cell's record the same way every other producer's does
// (getJSON, streamCell, warmViaFront all call NoteSwapStatus after the
// response). Without it the version read is the one fleetd→llama-swap
// call whose 401 teaches the fleet nothing: doctor's swap.credential goes
// on saying the front is fine, and the sticky refusal that stops the warm
// loops from 401ing 5,760 times a day never arms.
func TestFrontSwapVersion_401FeedsTheCredentialMachinery(t *testing.T) {
	front := newKeyedSwap(t, labSwapKey)
	s := newC15Server(t, front, hostsWithKeyPath(t, front.srv.URL, ""))

	if got := s.frontSwapVersion(t.Context()); got != "" {
		t.Fatalf("frontSwapVersion against a front that refused = %q, want absence", got)
	}
	f, bad := s.swapAuthState(fleetcfg.FrontCell)
	if !bad || f.Kind != SwapAuthUnauthorized {
		t.Fatalf("swap auth state = %+v (recorded=%v), want %s", f, bad, SwapAuthUnauthorized)
	}
	why, blocked := s.SwapAuthRefusal(fleetcfg.FrontCell)
	if !blocked || !strings.Contains(why, "swap_key_file") {
		t.Fatalf("SwapAuthRefusal = (%q, %v), want the no-retry-loop rule armed and naming the config", why, blocked)
	}

	// And a later accepted call retires it, so a fixed key file is not
	// reported as broken forever.
	s2 := newC15Server(t, front, hostsWithKey(t, front.srv.URL, labSwapKey))
	if got := s2.frontSwapVersion(t.Context()); got != "v239" {
		t.Fatalf("frontSwapVersion after the key was fixed = %q, want v239", got)
	}
	if _, bad := s2.swapAuthState(fleetcfg.FrontCell); bad {
		t.Fatal("an accepted read left a credential failure recorded")
	}
}

// TestReadOwnSwapVersion_PresentsNoCredential pins the OTHER posture, so
// that "the cell side is unauthenticated" is a tested fact with a reason
// rather than an oversight nobody wrote down. C15 §8 scopes the cell-side
// dialers out — they take `--llama-swap <url>` on the CLI and a slim
// announcer's box may hold no hosts.yaml — and closing that gap is C15's
// recorded futures item, not something this phase invents a second
// credential surface for.
func TestReadOwnSwapVersion_PresentsNoCredential(t *testing.T) {
	swap := newKeyedSwap(t, "")
	if got := ReadOwnSwapVersion(t.Context(), swap.srv.Client(), swap.srv.URL); got != "v239" {
		t.Fatalf("ReadOwnSwapVersion = %q, want v239", got)
	}
	if h := (<-swap.seen).Header.Get("Authorization"); h != "" {
		t.Fatalf("the cell-side reader sent %q; it has no hosts.yaml to resolve a credential from", h)
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

// ─── adversarial review ─────────────────────────────────────────────────────

// versionFleetWithFront is versionFleet's shape with a real front address,
// so the direct /api/version read is exercised rather than refused by a
// dead port.
func versionFleetWithFront(t *testing.T, frontURL string, versions map[string]*AnnounceVersions) *Server {
	t.Helper()
	cells := []Cell{{Name: fleetcfg.FrontCell, URL: frontURL}}
	names := make([]string, 0, len(versions))
	for n := range versions {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		cells = append(cells, Cell{Name: n, URL: "http://127.0.0.1:1", Class: "always_on"})
	}
	s, _ := doctorServer(t, cells...)
	for _, n := range names {
		s.recordAnnounce(&AnnounceRequest{V: AnnounceVersion, Cell: n, Seq: 1,
			Intent:   &AnnounceIntent{State: "serving", Since: time.Now().UTC()},
			Versions: versions[n]})
	}
	return s
}

// TestDoctor_VersionMatrixNamesWhoDidNotAnswer is the review's headline
// finding. `versions.llama_swap` reported
//
//	ok — every reporting cell runs llama-swap v239
//
// on a fleet where the FRONT had not answered at all and a second cell
// announced no version. Both are the absent-evidence shape this repo keeps
// relearning, and here they are the two silences the phase exists for: the
// front is the box the 2026-08-05 incident happened to and the ONLY one
// the direct read covers, and a cell on a vibe build older than C16's
// producer is the normal state of a fleet mid-upgrade. Neither appears in
// any other absence list on the report — defs.parity's absentNote covers
// defs SHAs, not this field.
func TestDoctor_VersionMatrixNamesWhoDidNotAnswer(t *testing.T) {
	// A front that is up but has no /api/version (an older llama-swap, or
	// a gated admin API): the read fails, the box vanishes from the matrix.
	front := httptest.NewServer(http.HandlerFunc(http.NotFound))
	defer front.Close()

	s := versionFleetWithFront(t, front.URL, map[string]*AnnounceVersions{
		"alpha": {LlamaSwap: "v239", Vibe: "v1", DefsSHA: "abc123"},
		"bravo": {Vibe: "v0", DefsSHA: "abc123"}, // pre-C16 announcer
	})
	got := mustCheck(t, s.Doctor(context.Background()), "versions.llama_swap", "")
	all := got.Summary + " " + got.Detail
	if !strings.Contains(got.Summary, "FRONT did not answer") {
		t.Errorf("summary = %q — a verdict that excludes the front must say so where an operator reads it; "+
			"the front is the box this check was added for and appears in no other absence list", got.Summary)
	}
	if !strings.Contains(all, "bravo") {
		t.Errorf("check = %q / %q — a cell that announces but reports no llama-swap version is invisible, "+
			"so a fleet mid-upgrade reads as uniform", got.Summary, got.Detail)
	}
	if strings.Contains(all, "alpha") && !strings.Contains(all, "v239") {
		t.Errorf("check = %q / %q — the cells that DID answer should still be named", got.Summary, got.Detail)
	}
}

// TestDoctor_VersionMatrixNamesTheSilentFrontOnEveryBranch: the
// qualification is not an OK-branch decoration. A divergent, ungated or
// empty matrix hides the same box, and defs.parity's rule ("which cells
// this verdict does NOT cover belongs on EVERY branch") is the precedent.
func TestDoctor_VersionMatrixNamesTheSilentFrontOnEveryBranch(t *testing.T) {
	front := httptest.NewServer(http.HandlerFunc(http.NotFound))
	defer front.Close()

	for _, tc := range []struct {
		name  string
		cells map[string]*AnnounceVersions
	}{
		{"empty", map[string]*AnnounceVersions{"alpha": {Vibe: "v0"}}},
		{"divergent", map[string]*AnnounceVersions{"alpha": {LlamaSwap: "v239"}, "bravo": {LlamaSwap: "v247"}}},
		{"ungated", map[string]*AnnounceVersions{"alpha": {LlamaSwap: "v260"}}},
		{"agreed", map[string]*AnnounceVersions{"alpha": {LlamaSwap: "v239"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := versionFleetWithFront(t, front.URL, tc.cells)
			got := mustCheck(t, s.Doctor(context.Background()), "versions.llama_swap", "")
			if !strings.Contains(got.Summary+" "+got.Detail, "FRONT did not answer") {
				t.Errorf("%s branch = %q / %q — the front's silence is dropped here", tc.name, got.Summary, got.Detail)
			}
		})
	}
}

// TestDoctor_VersionMatrixStaysQuietWhenEveryoneAnswered: the note may not
// become a permanent qualification on a healthy fleet (C13's rule about a
// level nobody reads).
func TestDoctor_VersionMatrixStaysQuietWhenEveryoneAnswered(t *testing.T) {
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/version" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"version":"v239"}`))
	}))
	defer front.Close()

	s := versionFleetWithFront(t, front.URL, map[string]*AnnounceVersions{
		"alpha": {LlamaSwap: "v239", DefsSHA: "abc123"},
	})
	got := mustCheck(t, s.Doctor(context.Background()), "versions.llama_swap", "")
	if got.Level != LevelOK {
		t.Fatalf("level = %q, want ok", got.Level)
	}
	if strings.Contains(got.Summary+" "+got.Detail, "did not answer") {
		t.Errorf("check = %q / %q — every box answered; a permanent qualification teaches an operator to skip the line",
			got.Summary, got.Detail)
	}
	if !strings.Contains(got.Summary, "v239") {
		t.Errorf("summary = %q, want the version named", got.Summary)
	}
}

// TestDoctor_OneReleaseIsNotDivergence: gating normalises a build string
// off the release tag and the divergence branch did not, so `v239` beside
// `v239 (dd81801)` — the exact pair `ungatedSwapVersions`' own table calls
// one gated release — read as two versions of llama-swap.
func TestDoctor_OneReleaseIsNotDivergence(t *testing.T) {
	s := versionFleet(t, map[string]*AnnounceVersions{
		"alpha": {LlamaSwap: "v239"},
		"bravo": {LlamaSwap: "v239 (dd81801)"},
	})
	got := mustCheck(t, s.Doctor(context.Background()), "versions.llama_swap", "")
	if got.Level != LevelOK {
		t.Fatalf("two spellings of one release → %s (%q), want ok: the gating half already normalises them",
			got.Level, got.Summary)
	}
}

// TestDigestPinned_EmptyDigestIsNotAPin: `repo:tag@sha256:` is a truncated
// paste docker refuses to pull, and calling it pinned is wrong in both
// directions — the operator is told the deployment is safe AND the
// deployment does not start.
func TestDigestPinned_EmptyDigestIsNotAPin(t *testing.T) {
	if digestPinned("ghcr.io/mostlygeek/llama-swap:v239-cpu-b9994@sha256:") {
		t.Error("an empty digest was reported as a pin")
	}
	if digestPinned("ghcr.io/mostlygeek/llama-swap:v239-cpu-b9994@sha256:   ") {
		t.Error("a whitespace digest was reported as a pin")
	}
	if !digestPinned("ghcr.io/mostlygeek/llama-swap:v239-cpu-b9994@sha256:6bae869") {
		t.Error("a real pin stopped being recognised")
	}
}

// TestReadSwapVersion_RejectsRatherThanTruncates: the two readers were two
// copies with two rules. The cell-side one truncated an over-long answer
// to 64 bytes — a guess wearing a plausible shape, which then enters the
// matrix as its own version and can raise a false ungated-version WARN —
// while the front-side one rejected it. One reader now, and it rejects.
func TestReadSwapVersion_RejectsRatherThanTruncates(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"over long", `{"version":"` + strings.Repeat("v", 500) + `"}`},
		{"just over the bound", `{"version":"` + strings.Repeat("v", MaxSwapVersionLen+1) + `"}`},
		{"control character", "{\"version\":\"v2\\n39\"}"},
		{"empty", `{"version":""}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			if got := ReadOwnSwapVersion(context.Background(), srv.Client(), srv.URL); got != "" {
				t.Fatalf("ReadOwnSwapVersion = %q, want absence — a truncated or unprintable version is a guess, "+
					"and an unprintable one makes fleetd's clean() refuse the whole announce", got)
			}
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":" v239 ","commit":"dd81801"}`))
	}))
	defer srv.Close()
	if got := ReadOwnSwapVersion(context.Background(), srv.Client(), srv.URL); got != "v239" {
		t.Fatalf("ReadOwnSwapVersion = %q, want v239", got)
	}
	if got := ReadOwnSwapVersion(context.Background(), srv.Client(), ""); got != "" {
		t.Fatalf("ReadOwnSwapVersion with no base URL = %q, want empty", got)
	}
}

// TestAnnounce_ControlCharVersionCostsTheWholeHeartbeat states, in this
// package, the consequence the cell-side reader's hygiene exists to avoid.
// It is not a change — it is C3's rule working exactly as designed — but
// without it the daemon-side test's reason lives only in a comment.
func TestAnnounce_ControlCharVersionCostsTheWholeHeartbeat(t *testing.T) {
	req := &AnnounceRequest{V: AnnounceVersion, Cell: "alpha", Seq: 1,
		Intent:   &AnnounceIntent{State: "serving", Since: time.Now().UTC()},
		Models:   []AnnounceModel{{ID: "m", State: "ready"}},
		Versions: &AnnounceVersions{LlamaSwap: "v2\n39", Vibe: "v1", DefsSHA: "abc123"},
	}
	if err := validateAnnounce(req); err == nil {
		t.Fatal("fleetd accepted a control character in versions.llama_swap")
	}
	// The point: the refusal is of the ANNOUNCE, so the intent echo, the
	// models and the usage feed go with it. A producer that lets such a
	// value onto the wire silences the cell, not just the field.
	req.Versions.LlamaSwap = "v239"
	if err := validateAnnounce(req); err != nil {
		t.Fatalf("a clean version was refused: %v", err)
	}
}
