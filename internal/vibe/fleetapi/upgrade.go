package fleetapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"unicode"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
)

// The upgrade ritual's two doctor checks (fleet-control C16), and the one
// fact they are both measured against.
//
// On 2026-08-05 the front's floating `:cpu` tag was found serving v247
// against a fleet gated on v239. v240+ changed the /api/events in-flight
// wire; vibe read the new shape as a REPORTED ZERO, and a reported zero is
// what disarms `drain --wait`, C14's suspend, C8's probe guard and both
// warm loops. The trigger was a routine `docker compose pull`.
//
// So the fleet gets both halves, because neither answers the other's
// question:
//
//   - front.image_pin is a DECLARATION. Only the deployment knows whether
//     the image reference it resolves carries a digest, and a pinned v239
//     and a floating tag that happens to be v239 today look identical from
//     the outside.
//   - versions.llama_swap is an OBSERVATION. Only the cells know what they
//     are actually running, and a pin declared in a config file nobody
//     applied looks exactly like a pin that was.

// UnmanagedFrontImage is the declaration that the front is not deployed
// from a container image at all — a systemd llama-swap on the front box is
// a supported deployment and has no tag to float. A closed vocabulary
// rather than a silent empty, so "the operator decided" and "nobody told
// fleetd" stay different answers (C12's AccessUndecided discipline).
const UnmanagedFrontImage = "unmanaged"

// digestPinned reports whether an image reference resolves to exactly one
// build. `repo:tag@sha256:…` is pinned — docker resolves the digest and
// ignores the tag, which is why the reference stack keeps both: the tag
// names the build for a human, the digest is the guarantee.
//
// The digest half must be NON-EMPTY: a trailing `@sha256:` is a truncated
// paste that docker refuses to pull, and reporting it as a pinned
// deployment is the one answer that is wrong in both directions.
func digestPinned(ref string) bool {
	_, digest, ok := strings.Cut(ref, "@sha256:")
	return ok && strings.TrimSpace(digest) != ""
}

// checkFrontImage reports whether the front's image reference is pinned.
//
// The value is DECLARED (fleet.front_image), and the check is named for
// what it proves — that the declaration carries a digest — because fleetd
// is a separate container with no docker socket and cannot observe the
// front's image. What catches a declaration that drifted from the
// deployment is the observed version matrix, one check over.
func (s *Server) checkFrontImage(rep *DoctorReport, host DoctorHost) {
	ref := strings.TrimSpace(host.FrontImage)
	switch {
	case ref == "":
		rep.Add(DoctorCheck{ID: "front.image_pin", Level: LevelUnknown,
			Summary: "nothing declares which image the front runs",
			Detail: "an unpinned front is how a `docker compose pull` moves the fleet onto a llama-swap this build has " +
				"never gated — the 2026-08-05 in-flight-wire incident, whose symptom was every busy guard reading a " +
				"reported zero.",
			Fix: "set fleet.front_image to the reference deploy/front resolves (repo:tag@sha256:…), or to " +
				UnmanagedFrontImage + " if the front is not run from a container image."})
	case !printableOneLine(ref):
		// It is rendered into a JSON report and a terminal. A local config
		// value is not hostile input, but a stray newline turns one check
		// into two lines that look like two checks.
		rep.Add(DoctorCheck{ID: "front.image_pin", Level: LevelFail,
			Summary: "fleet.front_image contains control characters",
			Fix:     "fix the value in fleetd's config.yaml."})
	case ref == UnmanagedFrontImage:
		rep.Add(DoctorCheck{ID: "front.image_pin", Level: LevelOK,
			Summary: "the front is declared not to run from a container image",
			Detail:  "there is no tag to float; whatever installs its llama-swap owns the version."})
	case digestPinned(ref):
		rep.Add(DoctorCheck{ID: "front.image_pin", Level: LevelOK,
			Summary: "front image is digest-pinned",
			Detail:  ref})
	default:
		rep.Add(DoctorCheck{ID: "front.image_pin", Level: LevelWarn,
			Summary: "front image is a floating tag: " + ref,
			Detail: "a `docker compose pull` on the front host can change which llama-swap the whole fleet talks to, " +
				"with no change to this repo and no event anywhere. That is exactly how v247 arrived on 2026-08-05.",
			Fix: "run scripts/upgrade/ritual.sh and paste the digest it prints into FRONT_IMAGE."})
	}
}

// MaxSwapVersionLen bounds a llama-swap version string. fleetd's announce
// hygiene bounds an announced one again at ingest (announce.go's clean,
// 256 bytes, printable only); this is the tighter rule both READERS apply
// before the value exists at all.
const MaxSwapVersionLen = 64

// ReadSwapVersion reads a llama-swap's own version off GET /api/version.
//
// ONE reader, because there are two producers and they must not drift:
// each cell reads its own llama-swap for the announce block, and fleetd
// reads the FRONT's directly (the front runs no announcer by design, and
// it is precisely the box the 2026-08-05 incident happened to). The first
// cut had two copies with two different hygiene rules — the front's
// REJECTED an oversized answer while the cell's TRUNCATED it to 64 bytes,
// and only the front's rejected control characters. Both directions were
// wrong: a truncated version is a guess wearing a plausible shape, and an
// announced control character makes fleetd's `clean` refuse the WHOLE
// heartbeat, taking presence, the intent echo, usage and probes down with
// a cosmetic field.
//
// So every failure — transport, status, JSON, hygiene — returns "". The
// caller must render that as an ABSENCE and never as agreement.
func ReadSwapVersion(ctx context.Context, client *http.Client, baseURL string) string {
	if client == nil || strings.TrimSpace(baseURL) == "" {
		return ""
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/api/version", nil)
	if err != nil {
		return ""
	}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var body struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body); err != nil {
		return ""
	}
	v := strings.TrimSpace(body.Version)
	// Upstream software answering on a LAN address is held to the same
	// hygiene as an announce field (announce.go's clean).
	if len(v) > MaxSwapVersionLen || !printableOneLine(v) {
		return ""
	}
	return v
}

// frontSwapVersion reads the FRONT's llama-swap version directly, on the
// address fleetd already probes — no announcer invented.
//
// Failure returns "", and checkVersions is responsible for SAYING SO: a
// front the fleet could not read must not vanish into a verdict about the
// cells that answered.
func (s *Server) frontSwapVersion(ctx context.Context) string {
	var url string
	for _, c := range s.cells {
		if c.Name == fleetcfg.FrontCell {
			url = c.URL
			break
		}
	}
	return ReadSwapVersion(ctx, s.snapClient, url)
}

// hasFrontCell reports whether the registry carries a front with an
// address to read. Without it "the front did not answer" and "there is no
// front to ask" would be spelled the same way.
func (s *Server) hasFrontCell() bool {
	for _, c := range s.cells {
		if c.Name == fleetcfg.FrontCell && strings.TrimSpace(c.URL) != "" {
			return true
		}
	}
	return false
}

func printableOneLine(s string) bool {
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// GatedSwapVersions names the llama-swap versions this build's conformance
// suite actually covers: the recorded wires in internal/swaptest/fixtures
// and the ci.yml matrix that replays them against real binaries.
//
// It is a hand-written list in a package the daemon already links rather
// than a read of the fixture tree, because production has no business
// importing a test double. TestGatedSwapVersionsMatchesRecordings (in
// internal/swaptest, which imports both) fails when the two drift, so
// adding a recording without adding it here is red.
func GatedSwapVersions() []string { return []string{"v239", "v247"} }

// ungatedSwapVersions returns the reported llama-swap versions that this
// build has no recording for, sorted.
//
// Reporting them is the difference between the ritual having happened and
// the ritual having been skipped: a cell running a version nothing here
// has ever replayed is exactly the state the incident was, and it is
// invisible to every other check — the wire either parses or reads as an
// idle cell.
func ungatedSwapVersions(reported map[string][]string) []string {
	gated := map[string]bool{}
	for _, v := range GatedSwapVersions() {
		gated[v] = true
	}
	var out []string
	for v := range reported {
		if !gated[normalizeSwapVersion(v)] {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// normalizeSwapVersion trims what a cell announces down to the release tag
// the recordings are named for. Cells announce llama-swap's own
// `/api/version` string, and a build carries a commit beside the version;
// the recordings are directories named v239 and v247.
func normalizeSwapVersion(v string) string {
	v = strings.TrimSpace(v)
	if i := strings.IndexAny(v, " \t("); i >= 0 {
		v = v[:i]
	}
	return v
}
