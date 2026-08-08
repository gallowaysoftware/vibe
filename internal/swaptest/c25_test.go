package swaptest

import (
	"io/fs"

	"strings"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/astscan"
)

// The gates that keep a real prompt out of this public repository
// (fleet-control C25, U8 and U9).
//
// This is the phase's largest risk and it is not the scorer. The fixture
// recorder talks to a real llama-swap on the operator's own box and writes
// what it reads into git. Its two existing redactions work because the
// sensitive part is a substring of something still worth keeping; a
// capture's sensitive part is the whole object, so the only correct
// handling is refusal — enforced here as three independent mechanisms:
//
//	U8a  the refusal function itself, in both directions
//	U8b  every fetch in the recorder goes THROUGH it (AST, not review)
//	U8c  the recorder's endpoint list names no capture route
//	U9   no committed fixture contains a capture, whatever wrote it

// ── U8a: the refusal, in both directions ────────────────────────────────

func TestRefuseCaptureEndpoint_RefusesEveryFormOfTheRoute(t *testing.T) {
	refused := []string{
		"/api/captures/1",
		"/api/captures/93371",
		"/api/captures/",
		"/api/captures",
		"http://127.0.0.1:9000/api/captures/12",
		"https://cell.example/api/captures/12?pretty=1",
		"http://127.0.0.1:9000/api/captures/12#frag",
		// Case: llama-swap's own mux is case-sensitive, but a guard that
		// only matches the exact spelling is one typo away from useless,
		// and over-refusing costs a fixture nobody needed.
		"/API/Captures/12",
		"http://host/upstream/model/api/captures/9",
	}
	for _, u := range refused {
		if why := RefuseCaptureEndpoint(u); why == "" {
			t.Errorf("RefuseCaptureEndpoint(%q) allowed it; a capture holds the verbatim prompt and completion of a real request", u)
		} else if !strings.Contains(why, CaptureRoutePrefix) {
			t.Errorf("RefuseCaptureEndpoint(%q) refused without naming the route: %s", u, why)
		}
	}

	// The other half, which is what stops the guard being "return a
	// reason, always": every endpoint the recorder legitimately reads must
	// pass. A refusal that refuses everything is a broken recorder, and a
	// broken recorder gets deleted rather than fixed.
	allowed := []string{
		"/running",
		"/v1/models",
		"/api/metrics/activity?sort=id&order=desc&limit=5&page=1",
		"/api/events",
		"/api/version",
		"http://127.0.0.1:9000/v1/chat/completions",
		// A query string that merely MENTIONS the word must not refuse:
		// the guard reads the path, not the URL.
		"http://127.0.0.1:9000/api/metrics/activity?note=captures",
	}
	for _, u := range allowed {
		if why := RefuseCaptureEndpoint(u); why != "" {
			t.Errorf("RefuseCaptureEndpoint(%q) refused a legitimate endpoint: %s", u, why)
		}
	}
}

// ── U8b: every fetch goes through the guard ─────────────────────────────

// TestRecorderFetchesOnlyThroughTheCaptureRefusal is the mechanism, as
// opposed to the intention. Three functions in the recorder build an HTTP
// request; each must call the refusal first. A fourth added by a future
// phase fails this scan on the commit that adds it, which is the only
// point at which anybody is looking.
func TestRecorderFetchesOnlyThroughTheCaptureRefusal(t *testing.T) {
	rule := astscan.Rule{
		Name:         "swaptest-recorder-refuses-captures",
		Dir:          ".",
		IncludeTests: true,
		// The RECORDER only. The conformance suite in this directory also
		// talks to a real llama-swap, but it writes nothing to git — the
		// risk this rule addresses is a FIXTURE COMMIT, and the recorder is
		// the only thing here that performs one.
		Files: []string{"record_test.go"},
		// The trigger set is what BUILDS a request; the requirement is the
		// one builder that guards the URL it is handed. guardedRequest
		// satisfies it directly, and every other producer satisfies it by
		// going through guardedRequest.
		Trigger:      []string{"NewRequestWithContext", "NewRequest", "Get", "Post", "guardedRequest"},
		Require:      []string{"RefuseCaptureEndpoint", "guardedRequest"},
		MinProducers: 4,
		Exempt:       map[string]string{},
		Because: "internal/swaptest's recorder reads a REAL llama-swap on the operator's box and writes what it " +
			"reads into a public repository. GET /api/captures/{id} answers the verbatim prompt, system prompt, " +
			"tool definitions and completion of a real request, and no redaction leaves a useful capture fixture " +
			"behind. Every fetch must pass swaptest.RefuseCaptureEndpoint first (fleet-control C25 §1e).",
	}
	res, err := rule.Check()
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if err := rule.Err(res); err != nil {
		t.Fatal(err)
	}
	t.Logf("checked %d fetch site(s): %s", len(res.Producers), strings.Join(res.Producers, ", "))
}

// ── U8c: the endpoint list names no capture route ───────────────────────

func TestRecordedEndpointsNameNoCaptureRoute(t *testing.T) {
	if len(recordedEndpoints) == 0 {
		t.Fatal("recordedEndpoints is empty; this gate would pass over anything")
	}
	for _, g := range recordedEndpoints {
		if why := RefuseCaptureEndpoint(g.path); why != "" {
			t.Errorf("the recorder's endpoint list names %s (%s): %s", g.name, g.path, why)
		}
	}
}

// ── U9: no committed fixture contains a capture ─────────────────────────

// captureMarkers are the field names of llama-swap's ReqRespCapture
// (server/captures.go, byte-identical on v239 and v247). Their presence in
// a committed file means somebody wrote a capture into this repository,
// whether the recorder did it or a hand-edit did.
var captureMarkers = []string{`"req_body"`, `"resp_body"`, `"req_headers"`, `"resp_headers"`}

func TestNoFixtureContainsACapture(t *testing.T) {
	root := "fixtures"
	files := 0
	// The walk is over the EMBEDDED tree, so the gate covers what is
	// compiled into every consumer rather than what happens to be on this
	// disk at this moment.
	err := fs.WalkDir(fixtures, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		files++
		b, err := fixtures.ReadFile(path)
		if err != nil {
			return err
		}
		body := string(b)
		for _, m := range captureMarkers {
			if strings.Contains(body, m) {
				t.Errorf("%s contains %s — that is a llama-swap capture, i.e. the verbatim prompt and completion "+
					"of a real request on the recording box, committed to a PUBLIC repository. Delete the fixture; "+
					"there is no redaction that leaves a useful one behind (fleet-control C25 §1e).", path, m)
			}
		}
		if strings.Contains(strings.ToLower(d.Name()), "capture") {
			t.Errorf("%s is named for captures; no recorded fixture may be one", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	// The floor. A walk that examined nothing passes silently, which is
	// the failure mode of every tree-walking test in this repo.
	if files < 10 {
		t.Fatalf("examined %d fixture file(s); the recordings are two directories of seven, so this gate is INERT", files)
	}
	t.Logf("examined %d fixture file(s)", files)
}
