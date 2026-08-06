package fleetapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/vibe/modelcat"
)

// TestNormalizedOllamaCatalogStaysReady is the merge seam between the two
// halves of the catalog fix.
//
// vibe's proxy now normalises an Ollama-shaped upstream into the OpenAI
// shape, which is what made the cell routable. The obvious way to do that
// — emit data[] and drop models[] — would have quietly broken THIS: fleetd
// has no /running to merge against on a llama.cpp-family cell, so the
// models[] key's presence is the only evidence it has that the models it
// can see are LOADED rather than merely configured. Every resident model
// on such a cell would have flipped to "stopped" in the fleet view, and no
// test in either package would have noticed.
//
// So the body under test is the proxy's actual output, produced by the
// same code the proxy calls, rather than a fixture that could drift away
// from it.
func TestNormalizedOllamaCatalogStaysReady(t *testing.T) {
	normalised, err := modelcat.Normalize([]byte(`{"models":[{"name":"qwen3.6-35b-a3b"}]}`))
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(normalised)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	_, ts, _ := newFleetdServer(t, []Cell{{Name: "laptop", URL: srv.URL, Class: "roaming"}})
	st := getState(t, ts)
	c := st.Cells[0]
	if !c.Reachable {
		t.Fatal("cell unreachable")
	}
	if len(c.Models) != 1 || c.Models[0].ID != "qwen3.6-35b-a3b" || c.Models[0].State != "ready" {
		t.Errorf("models = %+v, want qwen3.6-35b-a3b ready", c.Models)
	}
	if c.Display != DisplayServing {
		t.Errorf("display = %s, want SERVING", c.Display)
	}
}
