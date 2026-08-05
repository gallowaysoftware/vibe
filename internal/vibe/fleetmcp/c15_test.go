package fleetmcp

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
)

// fleet-control C15: the MCP facade issues three of the seven
// fleetd→llama-swap calls (warm_model's completion, its catalog check,
// unload_model), so it holds the same credential rule as fleetapi.

const mcpSwapKey = "swap-key-mcp"

// keyedSwapCell is a llama-swap with apiKeys set (v239 semantics:
// /health exempt, everything else 401 without the exact bearer).
type keyedSwapCell struct {
	srv  *httptest.Server
	auth chan string
}

func newKeyedSwapCell(t *testing.T, key string) *keyedSwapCell {
	t.Helper()
	k := &keyedSwapCell{auth: make(chan string, 32)}
	k.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case k.auth <- r.Header.Get("Authorization"):
		default:
		}
		if key != "" && r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, `{"error":"unauthorized: invalid or missing API key","src":"llama-swap"}`, http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"lab-chat"}]}`))
		default:
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		}
	}))
	t.Cleanup(k.srv.Close)
	return k
}

func newKeyedFacade(t *testing.T, cellURL, keyValue string) *Server {
	t.Helper()
	dir := t.TempDir()
	cell := fleetcfg.Cell{URL: cellURL, Class: fleetcfg.ClassAlwaysOn}
	if keyValue != "" {
		p := filepath.Join(dir, "swap.key")
		if err := os.WriteFile(p, []byte(keyValue+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cell.SwapKeyFile = p
	}
	hosts := &fleetcfg.File{Cells: map[string]fleetcfg.Cell{fleetcfg.FrontCell: cell}}
	fleet := fleetapi.New([]fleetapi.Cell{{Name: fleetcfg.FrontCell, URL: cellURL}},
		filepath.Join(dir, "history.json"),
		func() fleetapi.DaemonInfo { return fleetapi.DaemonInfo{} },
		fleetapi.Options{Hosts: hosts, IntentPath: filepath.Join(dir, "intent.json")})
	t.Cleanup(fleet.Close)
	return New(fleet, hosts, Options{})
}

func TestWarmModelCatalogCheckCarriesTheSwapKey(t *testing.T) {
	front := newKeyedSwapCell(t, mcpSwapKey)
	s := newKeyedFacade(t, front.srv.URL, mcpSwapKey)
	if _, err := s.toolWarmModel(context.Background(), "lab-chat"); err != nil {
		t.Fatalf("warm_model against a keyed front failed: %v", err)
	}
	if got := <-front.auth; got != "Bearer "+mcpSwapKey {
		t.Fatalf("catalog check Authorization = %q, want the declared key", got)
	}
	// The completion itself is fired from a goroutine (JIT is the start
	// verb and the tool returns immediately), and it is the half that
	// actually loads the model — asserting only the synchronous catalog
	// read would be a test whose name claims more than its body proves.
	select {
	case got := <-front.auth:
		if got != "Bearer "+mcpSwapKey {
			t.Fatalf("warm completion Authorization = %q, want the declared key", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the background warm never reached the front")
	}
}

// TestWarmModelReportsTheCredentialDiagnosis: warm_model is the OPERATOR's
// verb, so it is never suppressed by the sticky refusal — but its
// synchronous half must hand back the sentence that names the config,
// because llama-swap's own 401 body names nothing.
func TestWarmModelReportsTheCredentialDiagnosis(t *testing.T) {
	front := newKeyedSwapCell(t, mcpSwapKey)
	s := newKeyedFacade(t, front.srv.URL, "")
	_, err := s.toolWarmModel(context.Background(), "lab-chat")
	if err == nil {
		t.Fatal("warm_model succeeded against a front that 401s")
	}
	if !strings.Contains(err.Error(), "swap_key_file") {
		t.Errorf("error does not name the config to fix: %v", err)
	}
}

func TestUnloadModelCarriesTheCellsSwapKey(t *testing.T) {
	cell := newKeyedSwapCell(t, mcpSwapKey)
	s := newKeyedFacade(t, cell.srv.URL, mcpSwapKey)
	if _, err := s.toolUnloadModel(context.Background(), fleetcfg.FrontCell, "lab-chat"); err != nil {
		t.Fatalf("unload_model against a keyed cell failed: %v", err)
	}
	if got := <-cell.auth; got != "Bearer "+mcpSwapKey {
		t.Fatalf("unload Authorization = %q, want the declared key", got)
	}
}

// TestUnloadModelUnresolvableKeyIsNotQueued: a key file fleetd cannot
// read is fleetd's problem, and the cell would execute the verb happily.
// Queueing it would hide a broken credential behind "done on its next
// heartbeat" — C6's piggyback rule, applied to the new failure.
func TestUnloadModelUnresolvableKeyIsNotQueued(t *testing.T) {
	cell := newKeyedSwapCell(t, mcpSwapKey)
	dir := t.TempDir()
	missing := filepath.Join(dir, "gone.key")
	hosts := &fleetcfg.File{Cells: map[string]fleetcfg.Cell{
		fleetcfg.FrontCell: {URL: cell.srv.URL, Class: fleetcfg.ClassAlwaysOn},
		"heavy":            {URL: cell.srv.URL, Class: fleetcfg.ClassAlwaysOn, SwapKeyFile: missing},
	}}
	fleet := fleetapi.New([]fleetapi.Cell{
		{Name: fleetcfg.FrontCell, URL: cell.srv.URL}, {Name: "heavy", URL: cell.srv.URL}},
		filepath.Join(dir, "history.json"),
		func() fleetapi.DaemonInfo { return fleetapi.DaemonInfo{} },
		fleetapi.Options{Hosts: hosts, IntentPath: filepath.Join(dir, "intent.json")})
	t.Cleanup(fleet.Close)
	s := New(fleet, hosts, Options{})

	out, err := s.toolUnloadModel(context.Background(), "heavy", "lab-chat")
	if err == nil {
		t.Fatalf("unload succeeded with an unreadable key file: %q", out)
	}
	if strings.Contains(err.Error(), "queued") {
		t.Errorf("a local credential failure was reported as queued: %v", err)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error does not name the file: %v", err)
	}
}

// TestEveryLlamaSwapRequestIsAuthorized is the twin of fleetapi's: every
// HTTP request this package builds goes to a cell's llama-swap, so every
// builder must call the one authorizer.
func TestEveryLlamaSwapRequestIsAuthorized(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	found := 0
	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				fn, ok := n.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					return true
				}
				builds, authorizes := false, false
				ast.Inspect(fn.Body, func(m ast.Node) bool {
					sel, ok := m.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					switch sel.Sel.Name {
					case "NewRequestWithContext", "NewRequest":
						builds = true
					case "AuthorizeSwap":
						authorizes = true
					}
					return true
				})
				if builds {
					found++
					if !authorizes {
						t.Errorf("%s: %s builds an HTTP request to a llama-swap without calling AuthorizeSwap (C15)",
							filepath.Base(path), fn.Name.Name)
					}
				}
				return true
			})
		}
	}
	if found == 0 {
		t.Fatal("no request builders found — the scan is inert")
	}
}
