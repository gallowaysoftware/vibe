package fleetmcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
)

// newAnnouncingFacade is newTestFacade plus the fleetapi routes, so a
// test can drive the announce protocol end to end: queue a piggyback
// verb through a tool, then collect it the way a real cell does.
func newAnnouncingFacade(t *testing.T, cells map[string]fleetcfg.Cell) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	var fleetCells []fleetapi.Cell
	for name, c := range cells {
		fleetCells = append(fleetCells, fleetapi.Cell{Name: name, URL: c.URL, Class: string(c.Class)})
	}
	fleet := fleetapi.New(fleetCells, filepath.Join(dir, "history.json"),
		func() fleetapi.DaemonInfo { return fleetapi.DaemonInfo{} },
		fleetapi.Options{IntentPath: filepath.Join(dir, "intent.json"), LastSeenPath: filepath.Join(dir, "last-seen.json")})
	t.Cleanup(fleet.Close)
	mux := http.NewServeMux()
	fleet.Register(mux)
	New(fleet, &fleetcfg.File{Cells: cells}, Options{}).Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

// announce posts one heartbeat and returns fleetd's response.
func announce(t *testing.T, ts *httptest.Server, req fleetapi.AnnounceRequest) fleetapi.AnnounceResponse {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(ts.URL+"/api/fleet/announce", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("announce: HTTP %d", resp.StatusCode)
	}
	var out fleetapi.AnnounceResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestMCPWarmModelChatClassIsAllowed pins MIN-Q: the class guard exists
// to refuse firing a chat completion at an EMBED id. A chat-class entry
// documents ownership; refusing it told the caller a chat model "is not
// chat". Without the skip this test fails.
func TestMCPWarmModelChatClassIsAllowed(t *testing.T) {
	front := newFakeLlamaSwap(t, "qwen3.6-27b", "bge-embed")
	_, ts := newTestFacade(t, map[string]fleetcfg.Cell{
		"front": {URL: front.srv.URL, Class: fleetcfg.ClassAlwaysOn},
	}, map[string]string{"qwen3.6-27b": fleetcfg.ModelClassChat, "bge-embed": "embed"})

	resp := rpc(t, ts, `{"jsonrpc":"2.0","id":20,"method":"tools/call","params":{"name":"warm_model","arguments":{"model":"qwen3.6-27b"}}}`)
	text, isErr := toolText(t, resp)
	if isErr {
		t.Fatalf("a chat-class model must still warm: %s", text)
	}
	if !strings.Contains(text, "Warming qwen3.6-27b") {
		t.Errorf("text = %q", text)
	}

	// The guard still fires loudly for every other class.
	resp = rpc(t, ts, `{"jsonrpc":"2.0","id":21,"method":"tools/call","params":{"name":"warm_model","arguments":{"model":"bge-embed"}}}`)
	text, isErr = toolText(t, resp)
	if !isErr || !strings.Contains(text, "embed-class") {
		t.Errorf("embed-class model: isErr=%v text=%q", isErr, text)
	}
}

// TestMCPUnloadDefiniteAnswerIsNotQueued: the piggyback fallback is for
// DELIVERY failures. A 4xx is the admin API answering, and queueing it
// tells an agent the verb is "done on the next heartbeat" when the cell
// will refuse it identically.
func TestMCPUnloadDefiniteAnswerIsNotQueued(t *testing.T) {
	cell := newFakeLlamaSwap(t, "qwen3.6-27b")
	ts := newAnnouncingFacade(t, map[string]fleetcfg.Cell{
		"front":    {URL: "http://127.0.0.1:1", Class: fleetcfg.ClassAlwaysOn},
		"gpu-cell": {URL: cell.srv.URL, Class: fleetcfg.ClassOpportunistic},
	})
	// The cell announces the id, so the piggyback path is genuinely
	// available — only the 404 decides.
	announce(t, ts, fleetapi.AnnounceRequest{V: fleetapi.AnnounceVersion, Cell: "gpu-cell", Seq: 1,
		Models: []fleetapi.AnnounceModel{{ID: "ghost", State: "ready"}}})

	resp := rpc(t, ts, `{"jsonrpc":"2.0","id":22,"method":"tools/call","params":{"name":"unload_model","arguments":{"cell":"gpu-cell","model":"ghost"}}}`)
	text, isErr := toolText(t, resp)
	if !isErr {
		t.Fatalf("a 404 from the admin port must be an error, got %q", text)
	}
	if strings.Contains(text, "queued") {
		t.Errorf("a definitive 404 was queued for the announce path: %q", text)
	}
	out := announce(t, ts, fleetapi.AnnounceRequest{V: fleetapi.AnnounceVersion, Cell: "gpu-cell", Seq: 2,
		Models: []fleetapi.AnnounceModel{{ID: "ghost", State: "ready"}}})
	if len(out.Commands) != 0 {
		t.Errorf("commands = %+v, want none", out.Commands)
	}
}

// TestMCPUnloadUnreachableCellQueues is the mirror (MIN-G's actual
// purpose): a dead admin port becomes a piggybacked verb the cell
// collects on its next heartbeat, not a failure.
func TestMCPUnloadUnreachableCellQueues(t *testing.T) {
	ts := newAnnouncingFacade(t, map[string]fleetcfg.Cell{
		"front":    {URL: "http://127.0.0.1:1", Class: fleetcfg.ClassAlwaysOn},
		"gpu-cell": {URL: "http://" + deadAddr(t), Class: fleetcfg.ClassOpportunistic},
	})
	announce(t, ts, fleetapi.AnnounceRequest{V: fleetapi.AnnounceVersion, Cell: "gpu-cell", Seq: 1,
		Models: []fleetapi.AnnounceModel{{ID: "qwen3.6-27b", State: "ready"}}})

	resp := rpc(t, ts, `{"jsonrpc":"2.0","id":23,"method":"tools/call","params":{"name":"unload_model","arguments":{"cell":"gpu-cell","model":"qwen3.6-27b"}}}`)
	text, isErr := toolText(t, resp)
	if isErr {
		t.Fatalf("unreachable admin port must queue, not fail: %s", text)
	}
	if !strings.Contains(text, "queued") {
		t.Errorf("text = %q, want the queued-for-next-announce message", text)
	}
	out := announce(t, ts, fleetapi.AnnounceRequest{V: fleetapi.AnnounceVersion, Cell: "gpu-cell", Seq: 2,
		Models: []fleetapi.AnnounceModel{{ID: "qwen3.6-27b", State: "ready"}}})
	if len(out.Commands) != 1 || out.Commands[0].Verb != "unload" || out.Commands[0].Model != "qwen3.6-27b" {
		t.Fatalf("commands = %+v, want the queued unload", out.Commands)
	}
}

// TestCellClientTokenPrecedence pins NIT-E: in fleetd the per-cell
// token_file WINS over $VIBE_TOKEN (one env var must not void every
// cells.X.token_file), and an unreadable file is a typed error rather
// than a silent fall-through to the local token (MIN-P's shape).
func TestCellClientTokenPrecedence(t *testing.T) {
	dir := t.TempDir()
	tok := filepath.Join(dir, "cell-token")
	if err := os.WriteFile(tok, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	authCh := make(chan string, 4)
	cellDaemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authCh <- r.Header.Get("Authorization")
		http.Error(w, "no", http.StatusNotFound)
	}))
	t.Cleanup(cellDaemon.Close)

	s, _ := newTestFacade(t, map[string]fleetcfg.Cell{
		"front":    {URL: "http://127.0.0.1:1", Class: fleetcfg.ClassAlwaysOn},
		"gpu-cell": {URL: "http://127.0.0.1:1", Class: fleetcfg.ClassOpportunistic, DaemonURL: cellDaemon.URL, TokenFile: tok},
		"bad-cell": {URL: "http://127.0.0.1:1", Class: fleetcfg.ClassOpportunistic, DaemonURL: cellDaemon.URL, TokenFile: filepath.Join(dir, "missing")},
	}, nil)

	t.Setenv("VIBE_TOKEN", "from-env")
	c, err := s.cellClient("gpu-cell")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.Status(t.Context())
	select {
	case got := <-authCh:
		if got != "Bearer from-file" {
			t.Errorf("Authorization = %q, want the per-cell token_file to win over $VIBE_TOKEN", got)
		}
	default:
		t.Fatal("the cell daemon saw no request")
	}

	if _, err := s.cellClient("bad-cell"); err == nil || !strings.Contains(err.Error(), "token_file") {
		t.Errorf("unreadable token_file: got %v, want a typed error naming it", err)
	}
}
