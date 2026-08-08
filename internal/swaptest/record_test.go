package swaptest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestRecord captures fixtures off a REAL llama-swap. It is the documented,
// repeatable capture procedure — re-recording against a future llama-swap
// must be a chore, not archaeology:
//
//	SWAPTEST_RECORD_URL=http://127.0.0.1:9000 \
//	SWAPTEST_RECORD_VERSION=v239 \
//	SWAPTEST_RECORD_MODEL=qwen3.6-27b \
//	  go test ./internal/swaptest -run TestRecord -v
//
// SWAPTEST_RECORD_MODEL is optional and must name a model that is ALREADY
// RESIDENT (check /running first). The recorder drives exactly one 8-token
// completion against it to capture a request lifecycle; it never loads a
// model, never unloads one, and never writes configuration. Without it the
// connect burst is still captured and the lifecycle frames are skipped.
//
// SWAPTEST_RECORD_VERSION defaults to whatever `llama-swap --version`
// reports if the binary is on PATH.
//
// Recordings are written BESIDE the existing ones, never over them: a
// heterogeneous fleet is the normal state here, so "both wires must pass"
// is the requirement, not "newest wins".
func TestRecord(t *testing.T) {
	base := os.Getenv("SWAPTEST_RECORD_URL")
	if base == "" {
		t.Skip("SWAPTEST_RECORD_URL unset; see the doc comment for the capture procedure")
	}
	base = strings.TrimRight(base, "/")

	version, commit := recordedVersion(t)
	dir := filepath.Join("fixtures", version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	t.Logf("recording %s (%s) from %s into %s", version, commit, base, dir)

	write := func(name string, b []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		t.Logf("  %s (%d bytes)", name, len(b))
	}

	for _, g := range recordedEndpoints {
		body := recordGET(t, base+g.path)
		write(g.name, foldHome(indentJSON(t, body)))
	}

	// The connect burst, raw. logData payloads are REDACTED — they carry the
	// operator's whole llama-swap log history (106 KB and 103 KB on the box
	// this was first recorded from) and belong in nobody's git history. The
	// original lengths are kept in RECORDED because the length is the part
	// this module cares about: fleetapi caps its SSE scanner at 8 MB.
	burst, sizes := recordEvents(t, base, 3*time.Second, nil)
	write("events-connect.sse", burst)

	if model := os.Getenv("SWAPTEST_RECORD_MODEL"); model != "" {
		assertResident(t, base, model)
		lifecycle, _ := recordEvents(t, base, 20*time.Second, func() {
			// One 8-token completion against a model that is already
			// resident. This is the whole mutation budget of the recorder.
			pokeChat(t, base, model)
		})
		write("events-request.sse", keepTypes(lifecycle, "inflight", "activity"))
	} else {
		t.Log("SWAPTEST_RECORD_MODEL unset; skipping the request-lifecycle capture")
	}

	// One deliberate 400. A refused request still produces an activity row,
	// with error_msg set and tokens at zero, and C7a's err_req counter is
	// the only thing standing between that row and a silent under-count.
	before := activityHeadID(t, base)
	postJSON(t, base+"/v1/chat/completions", `{"model":"`+recordErrModel(base)+`"}`)
	if row := activityRowAbove(t, base, before); row != nil {
		write("activity-row-error.json", indentJSON(t, row))
	}

	meta, _ := json.MarshalIndent(map[string]any{
		"version":       version,
		"commit":        commit,
		"at":            time.Now().UTC().Format(time.RFC3339),
		"source":        base,
		"captured_from": "a running llama-swap instance",
		"logdata_bytes": sizes,
		"redactions":    "TWO, and only two: logData frame payloads are replaced with a marker (their original byte lengths are in logdata_bytes), and the recording operator's home directory is folded to ~. Nothing else is altered.",
	}, "", "  ")
	write("RECORDED", append(meta, '\n'))
}

func recordedVersion(t *testing.T) (version, commit string) {
	t.Helper()
	if v := os.Getenv("SWAPTEST_RECORD_VERSION"); v != "" {
		return v, os.Getenv("SWAPTEST_RECORD_COMMIT")
	}
	bin := os.Getenv("SWAPTEST_RECORD_BIN")
	if bin == "" {
		bin = "llama-swap"
	}
	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("set SWAPTEST_RECORD_VERSION, or put llama-swap on PATH: %v", err)
	}
	// "version: v239 (dd81801), built at 2026-07-11T21:47:14Z"
	m := regexp.MustCompile(`version:\s*(\S+)\s*\(([0-9a-f]+)\)`).FindStringSubmatch(string(out))
	if m == nil {
		t.Fatalf("could not parse %q; set SWAPTEST_RECORD_VERSION", string(out))
	}
	return m[1], m[2]
}

// recordedEndpoints is the recorder's complete GET list, as data rather
// than as a literal inside the loop. It is data so U8's gate can assert on
// it: the one endpoint that must never appear here is /api/captures/{id},
// and a list a test can read is the difference between a rule and a hope.
var recordedEndpoints = []struct{ name, path string }{
	{"running.json", "/running"},
	{"models.json", "/v1/models"},
	{"activity-page.json", "/api/metrics/activity?sort=id&order=desc&limit=5&page=1"},
}

// guardedRequest is the ONE place this recorder builds an HTTP request,
// and it refuses the captures endpoint before it builds one.
//
// It is a builder rather than three call-site checks for two reasons. A
// guard in one of N call paths is not a guard — the recurring finding this
// repo has paid for repeatedly. And a check written at a call site can be
// applied to the wrong string: a review found `recordEvents` guarding the
// literal `base + "/api/events"`, which can never be a capture route, so
// the astscan rule saw a call and proved nothing. Here the guarded value
// and the fetched value are the same variable, by construction.
//
// A capture is the case foldHome and the logData redaction cannot handle:
// its sensitive part is the whole object, not a substring of it. See
// RefuseCaptureEndpoint (captures.go) for the argument.
func guardedRequest(t *testing.T, ctx context.Context, method, url string, body io.Reader) *http.Request {
	t.Helper()
	if why := RefuseCaptureEndpoint(url); why != "" {
		t.Fatal(why)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}
	return req
}

func recordGET(t *testing.T, url string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req := guardedRequest(t, ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return buf.Bytes()
}

// recordEvents streams /api/events, optionally running drive() once the
// connect burst has landed, and returns the raw bytes with logData payloads
// redacted plus the original payload sizes.
func recordEvents(t *testing.T, base string, budget time.Duration, drive func()) ([]byte, []int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	req := guardedRequest(t, ctx, http.MethodGet, base+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/events: %v", err)
	}
	defer resp.Body.Close()

	var out bytes.Buffer
	var sizes []int
	driven := false
	sawInflight := false
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 32<<20)
	var frameLines []string
	for sc.Scan() {
		line := sc.Text()
		if line != "" {
			frameLines = append(frameLines, line)
			continue
		}
		typ, redacted, size := redactFrame(strings.Join(frameLines, "\n"))
		frameLines = nil
		if size > 0 {
			sizes = append(sizes, size)
		}
		out.WriteString(redacted + "\n\n")
		if typ == "inflight" {
			if !sawInflight {
				sawInflight = true
				if drive != nil && !driven {
					driven = true
					go drive()
					continue
				}
			} else if driven && inflightIsTerminal(redacted) {
				// The lifecycle capture is complete once the request that
				// was driven has come and gone.
				return out.Bytes(), sizes
			}
		}
		if drive == nil && sawInflight {
			return out.Bytes(), sizes
		}
	}
	return out.Bytes(), sizes
}

// inflightIsTerminal reports whether an inflight frame retires the request
// the recorder drove: an empty full list on v239, a remove edge on v240+.
//
// It DECODES the frame rather than matching a substring of it. The inner
// payload is JSON inside a JSON string, so on the raw wire a remove edge
// reads `\"operation\":\"remove\"` — a substring test for the unescaped
// form can never fire, which is why the v247 capture ran to its full
// timeout and swept in every unrelated request the router served meanwhile.
// A recorder that over-captures writes other people's model names and
// request paths into a fixture destined for a public repo, under a RECORDED
// stamp that promises exactly two redactions.
func inflightIsTerminal(rawFrame string) bool {
	for _, line := range strings.Split(rawFrame, "\n") {
		rest, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		var env struct {
			Type string `json:"type"`
			Data string `json:"data"`
		}
		if json.Unmarshal([]byte(rest), &env) != nil || env.Type != "inflight" {
			return false
		}
		var inner struct {
			Operation string            `json:"operation"`
			Requests  []json.RawMessage `json:"requests"`
		}
		if json.Unmarshal([]byte(env.Data), &inner) != nil {
			return false
		}
		if inner.Operation == "remove" {
			return true
		}
		return inner.Operation == "" && len(inner.Requests) == 0
	}
	return false
}

// redactFrame replaces a logData frame's payload with a marker, returning
// the frame type, the (possibly redacted) frame, and the original payload
// length. Every other frame type is returned byte-for-byte.
func redactFrame(f string) (typ, out string, origLen int) {
	for _, line := range strings.Split(f, "\n") {
		rest, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		var env struct {
			Type string `json:"type"`
			Data string `json:"data"`
		}
		if err := json.Unmarshal([]byte(rest), &env); err != nil {
			return "", f, 0
		}
		if env.Type != "logData" {
			return env.Type, f, 0
		}
		n := len(env.Data)
		redacted := rawFrame("logData", `{"data":"[REDACTED BY swaptest RECORDER: `+fmt.Sprint(n)+` bytes of operator log]"}`)
		return env.Type, strings.TrimSuffix(redacted, "\n\n"), n
	}
	return "", f, 0
}

// keepTypes filters a raw SSE capture down to the named frame types.
func keepTypes(raw []byte, types ...string) []byte {
	want := map[string]bool{}
	for _, t := range types {
		want[t] = true
	}
	var out bytes.Buffer
	for _, f := range strings.Split(string(raw), "\n\n") {
		if strings.TrimSpace(f) == "" {
			continue
		}
		typ, _, _ := redactFrame(f)
		if want[typ] {
			out.WriteString(f + "\n\n")
		}
	}
	return out.Bytes()
}

func assertResident(t *testing.T, base, model string) {
	t.Helper()
	var wrap struct {
		Running []struct {
			Model string `json:"model"`
			State string `json:"state"`
		} `json:"running"`
	}
	if err := json.Unmarshal(recordGET(t, base+"/running"), &wrap); err != nil {
		t.Fatalf("decode /running: %v", err)
	}
	for _, r := range wrap.Running {
		if r.Model == model && r.State == "ready" {
			return
		}
	}
	t.Fatalf("%s is not already resident on %s — the recorder must never load a model; pick one from /running or unset SWAPTEST_RECORD_MODEL", model, base)
}

func pokeChat(t *testing.T, base, model string) {
	body := fmt.Sprintf(`{"model":%q,"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, model)
	postJSON(t, base+"/v1/chat/completions", body)
}

func postJSON(t *testing.T, url, body string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req := guardedRequest(t, ctx, http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Logf("POST %s: %v", url, err)
		return
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	t.Logf("POST %s -> %d (%d bytes)", url, resp.StatusCode, buf.Len())
}

func recordErrModel(_ string) string { return "swaptest-no-such-model" }

func activityHeadID(t *testing.T, base string) int64 {
	t.Helper()
	var p struct {
		Data []struct {
			ID int64 `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recordGET(t, base+"/api/metrics/activity?sort=id&order=desc&limit=1&page=1"), &p); err != nil {
		t.Fatalf("decode activity: %v", err)
	}
	if len(p.Data) == 0 {
		return 0
	}
	return p.Data[0].ID
}

func activityRowAbove(t *testing.T, base string, id int64) []byte {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		raw := recordGET(t, base+"/api/metrics/activity?sort=id&order=desc&limit=5&page=1")
		var p struct {
			Data []json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("decode activity: %v", err)
		}
		for _, r := range p.Data {
			var head struct {
				ID int64 `json:"id"`
			}
			if err := json.Unmarshal(r, &head); err == nil && head.ID > id {
				return r
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Log("no activity row appeared above the refused request; skipping activity-row-error.json")
	return nil
}

// foldHome replaces the operator's home directory with ~ before a fixture
// is written. It is the SECOND and last redaction the recorder performs
// (logData payloads are the first), and both are named in RECORDED. The
// repo is public and /running echoes the whole llama-server argv; the same
// fold is what router/fingerprint.go's normalizeHome does, for the same
// reason. Nothing in this package asserts on those bytes.
func foldHome(b []byte) []byte {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || home == "/" {
		return b
	}
	return bytes.ReplaceAll(b, []byte(home), []byte("~"))
}

func indentJSON(t *testing.T, b []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := json.Indent(&out, b, "", "  "); err != nil {
		return b
	}
	out.WriteByte('\n')
	return out.Bytes()
}
