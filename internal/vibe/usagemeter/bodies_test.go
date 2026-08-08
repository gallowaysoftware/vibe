package usagemeter

// "Counts only, never bodies" is the one clause of futures item 8 that
// shipped structurally and was never gated. It holds because ActivityRow
// (usagemeter.go) decodes a strict SUBSET of llama-swap's
// store.ActivityLogEntry: the counting fields, and nothing that can carry
// operator text. llama-swap's real row also carries `error_msg` (which
// llama-swap builds out of the upstream's response body) and `metadata`
// (arbitrary client-supplied strings) — and, since the capture feature,
// `has_capture`, whose capture holds the verbatim prompt and completion at
// GET /api/captures/{id}.
//
// encoding/json's ignore-unknown-fields default is what keeps that text
// out. That default is a property of a struct definition, so a one-line
// edit to ActivityRow silently turns this ledger into a text store. These
// two tests are the mechanism: one fails if the type grows a field that
// could hold a body, the other fails if any such text reaches the state
// file or the wire.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
)

// countingFields is the complete field set ActivityRow is allowed to have.
// Every entry is a number, an instant, a def name or a fixed route string —
// each low-cardinality and none of it operator text. Adding a field means
// changing this list, which is the point: it is a decision, not a diff.
var countingFields = map[string]bool{
	"ID":             true,
	"Timestamp":      true,
	"Model":          true,
	"ReqPath":        true,
	"RespStatusCode": true,
	"Tokens":         true,
	"DurationMs":     true,
}

func TestActivityRow_CannotCarryABody(t *testing.T) {
	rt := reflect.TypeOf(ActivityRow{})
	for i := range rt.NumField() {
		f := rt.Field(i)
		if !countingFields[f.Name] {
			t.Errorf("ActivityRow grew field %s %s: this ledger counts and never stores text. "+
				"If it is genuinely a count, add it to countingFields; if it can hold a prompt, "+
				"a completion, an error body or client metadata, it does not belong on this type "+
				"(futures item 8: \"Counts only, never bodies\")", f.Name, f.Type)
		}
	}
	// The nested token block is numbers only, for the same reason.
	tt := reflect.TypeOf(Tokens{})
	for i := range tt.NumField() {
		f := tt.Field(i)
		switch f.Type.Kind() {
		case reflect.Int64, reflect.Float64:
		default:
			t.Errorf("Tokens.%s is %s; every token field is a number", f.Name, f.Type)
		}
	}
}

// TestPoll_TextOnTheWireReachesNeitherTheStateFileNorTheWire drives a real
// poll against a server answering with the fields llama-swap actually sends
// — including the three this package deliberately does not decode — and
// asserts none of that text survives anywhere the collector writes.
func TestPoll_TextOnTheWireReachesNeitherTheStateFileNorTheWire(t *testing.T) {
	// A marker shaped like the thing that must never leak: a prompt.
	const secret = "PRIVATE-PROMPT-3f9a1c my ssh key is hunter2 and the patient's name is"

	page := fmt.Sprintf(`{"data":[
	  {"id":2,"timestamp":"2026-08-06T12:00:01Z","model":"qwen","req_path":"/v1/chat/completions",
	   "resp_status_code":500,"duration_ms":12,"error_msg":%[1]q,"has_capture":true,
	   "metadata":{"x-session-id":%[1]q},
	   "tokens":{"input_tokens":0,"output_tokens":0,"cache_tokens":0}},
	  {"id":1,"timestamp":"2026-08-06T12:00:00Z","model":"qwen","req_path":"/v1/chat/completions",
	   "resp_status_code":200,"duration_ms":900,"error_msg":"","has_capture":true,
	   "metadata":{"prompt":%[1]q},
	   "tokens":{"input_tokens":100,"output_tokens":50,"cache_tokens":900}}
	],"page":1,"limit":999,"total":2,"total_pages":1}`, secret)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/metrics/activity" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(page)); err != nil {
			t.Errorf("write page: %v", err)
		}
	}))
	t.Cleanup(ts.Close)

	c := newCollector(t, ts.URL)
	if err := c.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	// The counts still landed — a collector that leaked nothing because it
	// read nothing would pass the leak half vacuously.
	if got := totalsFor(t, c, "qwen", BasisChat); got.Req != 1 || got.Out != 50 || got.ErrReq != 1 {
		t.Fatalf("the counting half did not happen, so the leak half proves nothing: %+v", got)
	}

	state, err := os.ReadFile(c.cfg.StatePath)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	if strings.Contains(string(state), secret) {
		t.Errorf("operator text reached the state file %s", c.cfg.StatePath)
	}

	wire, err := json.Marshal(c.Snapshot())
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if strings.Contains(string(wire), secret) {
		t.Errorf("operator text reached the announce wire: %s", wire)
	}
}
