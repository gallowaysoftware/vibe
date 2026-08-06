package modelcat

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// openaiClient is what every consumer of a catalog actually reads: the
// data[] array and the id inside each row. vamp's ResolveModelID, `vibe
// cell`'s probe, fleetannounce's catalogIDs and the daemon's
// external-backend check all decode exactly this and nothing else, which
// is why a body they cannot read reports a cell serving nothing.
type openaiClient struct {
	Object string `json:"object"`
	Data   []struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	} `json:"data"`
}

func decodeAsClient(t *testing.T, body []byte) openaiClient {
	t.Helper()
	var c openaiClient
	if err := json.Unmarshal(body, &c); err != nil {
		t.Fatalf("an OpenAI client could not decode the catalog: %v\n%s", err, body)
	}
	return c
}

func TestNormalize_OllamaShapeBecomesOpenAI(t *testing.T) {
	// The exact shape observed on the novodoo cell.
	out, err := Normalize([]byte(`{"models":[{"name":"qwen3.6-35b-a3b"}]}`))
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	got := decodeAsClient(t, out)
	if got.Object != "list" {
		t.Errorf("object = %q, want list", got.Object)
	}
	if len(got.Data) != 1 || got.Data[0].ID != "qwen3.6-35b-a3b" {
		t.Fatalf("data = %+v, want one row with id qwen3.6-35b-a3b\n%s", got.Data, out)
	}
	if got.Data[0].Object != "model" {
		t.Errorf("row object = %q, want model", got.Data[0].Object)
	}
}

func TestNormalize_OllamaModelFieldWinsOverName(t *testing.T) {
	// Ollama's own clients address `model`; `name` is a display string on
	// some servers, so a row carrying both must be advertised as `model`.
	out, err := Normalize([]byte(`{"models":[{"name":"Qwen 3.6 (35B)","model":"qwen3.6-35b-a3b"}]}`))
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	got := decodeAsClient(t, out)
	if len(got.Data) != 1 || got.Data[0].ID != "qwen3.6-35b-a3b" {
		t.Fatalf("data = %+v, want id qwen3.6-35b-a3b\n%s", got.Data, out)
	}
}

func TestNormalize_DropsOllamaOnlyFields(t *testing.T) {
	out, err := Normalize([]byte(`{"models":[{"name":"m","model":"m",` +
		`"size":4831838208,"digest":"sha256:deadbeef","modified_at":"2026-01-01T00:00:00Z"}]}`))
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	for _, field := range []string{"size", "digest", "modified_at", "deadbeef"} {
		if strings.Contains(string(out), field) {
			t.Errorf("normalised catalog still carries the Ollama-only field %q: %s", field, out)
		}
	}
}

func TestNormalize_OpenAIShapePreservesRowsVerbatim(t *testing.T) {
	// llama-swap's own rows carry meta/status that clients (and hum) read.
	// Normalising must not be a re-encode that quietly drops them.
	in := `{"object":"list","data":[{"id":"chat-model","object":"model","created":0,` +
		`"owned_by":"llama-swap","meta":{"llamaswap":{"peerID":"cellA"}},"status":{"value":"unloaded"}}]}`
	out, err := Normalize([]byte(in))
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	for _, want := range []string{`"peerID":"cellA"`, `"value":"unloaded"`, `"owned_by":"llama-swap"`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("normalising an OpenAI catalog dropped %s: %s", want, out)
		}
	}
}

func TestNormalize_BothShapesAgreeOnOneIDSet(t *testing.T) {
	// Real llama.cpp emits both keys. The id must appear once, not twice.
	out, err := Normalize([]byte(`{"object":"list","data":[{"id":"m","object":"model"}],` +
		`"models":[{"name":"m","model":"m"}]}`))
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	got := decodeAsClient(t, out)
	if len(got.Data) != 1 || got.Data[0].ID != "m" {
		t.Fatalf("data = %+v, want exactly one row for m\n%s", got.Data, out)
	}
}

func TestParse_UnrecognisedShapeIsAnErrorNotAnEmptyCatalog(t *testing.T) {
	// The decision this pins: a shape nobody recognised must NOT become a
	// valid-looking empty catalog. An empty catalog is a claim ("this cell
	// serves nothing") that a parser which failed has no standing to make,
	// and the silent version of it is how an unroutable cell reached the
	// fleet in the first place.
	for _, body := range []string{
		`hi`,
		`[]`,
		`{"object":"list"}`,
		`{"data":"not-an-array"}`,
		`{"result":[{"id":"m"}]}`,
		``,
	} {
		c, err := Parse([]byte(body))
		if !errors.Is(err, ErrShape) {
			t.Errorf("Parse(%q) err = %v, want ErrShape", body, err)
		}
		if c != nil {
			t.Errorf("Parse(%q) returned a catalog alongside the error: %+v", body, c)
		}
	}
}

func TestParse_EmptyButValidCatalogIsNotAnError(t *testing.T) {
	// A router with nothing configured says so in a shape we recognise.
	// That IS a catalog, and it must survive as one.
	c, err := Parse([]byte(`{"object":"list","data":[]}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := c.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if got := string(out); got != `{"object":"list","data":[]}` {
		t.Errorf("empty catalog = %s", got)
	}
}

func TestParse_RowWithoutAnIDIsDropped(t *testing.T) {
	// A row a client cannot address is the phantom this package exists to
	// keep out: a catalog entry that 404s the moment anybody sends it.
	c, err := Parse([]byte(`{"object":"list","data":[{"object":"model"},{"id":"real"}]}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := c.IDs(); len(got) != 1 || got[0] != "real" {
		t.Errorf("IDs = %v, want [real]", got)
	}
}

func TestJSON_KeepsTheOllamaKeyOnlyWhenTheUpstreamSentOne(t *testing.T) {
	// fleetd reads the key's PRESENCE as residency evidence for a cell
	// with no /running. Emitting it for a llama-swap catalog would mark
	// every model on a cell whose /running transiently failed as "ready";
	// dropping it for a llama.cpp-family cell marks its LOADED models
	// "stopped". Both directions matter, so both are pinned here.
	ollama, err := Normalize([]byte(`{"models":[{"name":"m","model":"m"}]}`))
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if !strings.Contains(string(ollama), `"models":[{"name":"m","model":"m"}]`) {
		t.Errorf("normalised Ollama catalog lost the residency key: %s", ollama)
	}
	openai, err := Normalize([]byte(`{"object":"list","data":[{"id":"m"}]}`))
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if strings.Contains(string(openai), `"models"`) {
		t.Errorf("normalising an OpenAI catalog invented a residency key: %s", openai)
	}
}

func TestRename_MovesBothHalvesTogether(t *testing.T) {
	c, err := Parse([]byte(`{"models":[{"name":"/models/Qwen3.6.gguf","model":"/models/Qwen3.6.gguf"}]}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	c.Rename("/models/Qwen3.6.gguf", "qwen3.6")
	out, err := c.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if strings.Contains(string(out), "/models/Qwen3.6.gguf") {
		t.Errorf("rename left the upstream id somewhere in the catalog: %s", out)
	}
	got := decodeAsClient(t, out)
	if len(got.Data) != 1 || got.Data[0].ID != "qwen3.6" {
		t.Errorf("data = %+v, want id qwen3.6\n%s", got.Data, out)
	}
	if !strings.Contains(string(out), `"models":[{"name":"qwen3.6","model":"qwen3.6"}]`) {
		t.Errorf("residency half kept the old id: %s", out)
	}
}

func TestRename_PreservesOtherFieldsOfTheRow(t *testing.T) {
	c, err := Parse([]byte(`{"object":"list","data":[{"id":"up","object":"model","owned_by":"mlx"}]}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	c.Rename("up", "alias")
	out, _ := c.JSON()
	if !strings.Contains(string(out), `"owned_by":"mlx"`) {
		t.Errorf("rename dropped a field of the row: %s", out)
	}
	if !strings.Contains(string(out), `"id":"alias"`) {
		t.Errorf("rename did not apply: %s", out)
	}
}

func TestRename_OntoAnIDTheCatalogAlreadyHoldsDoesNotDuplicateIt(t *testing.T) {
	c, err := Parse([]byte(`{"models":[{"model":"up"},{"model":"alias"}]}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	c.Rename("up", "alias")
	if got := c.IDs(); len(got) != 1 || got[0] != "alias" {
		t.Errorf("IDs = %v, want [alias]", got)
	}
	out, _ := c.JSON()
	if !strings.Contains(string(out), `"models":[{"name":"alias","model":"alias"}]}`) {
		t.Errorf("residency list holds the id twice: %s", out)
	}
}

func TestMerge_DedupesByIDAndUnionsResidency(t *testing.T) {
	a, err := Parse([]byte(`{"object":"list","data":[{"id":"shared"},{"id":"only-a"}]}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	b, err := Parse([]byte(`{"models":[{"name":"shared"},{"name":"only-b"}]}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	a.Merge(b)
	if got := a.IDs(); len(got) != 3 {
		t.Fatalf("IDs = %v, want 3 deduped ids", got)
	}
	for _, id := range []string{"shared", "only-a", "only-b"} {
		if !a.Has(id) {
			t.Errorf("merged catalog is missing %q", id)
		}
	}
	out, _ := a.JSON()
	if !strings.Contains(string(out), `"models"`) {
		t.Errorf("merging a residency-bearing catalog dropped the key: %s", out)
	}
}

func TestOllamaID(t *testing.T) {
	for _, tc := range []struct {
		e    Entry
		want string
	}{
		{Entry{Model: "m", Name: "n"}, "m"},
		{Entry{Name: "n"}, "n"},
		{Entry{}, ""},
	} {
		if got := tc.e.OllamaID(); got != tc.want {
			t.Errorf("Entry%+v.OllamaID() = %q, want %q", tc.e, got, tc.want)
		}
	}
}
