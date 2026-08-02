package fleetapi

import (
	"embed"
	"net/http"
)

// The fleet page (fleet-control C4 §3): one static file over the
// substrate C1–C3 built — the derived-state table live off
// /api/fleet/state + /events, thin action buttons over the MCP facade's
// existing tools (no new mutation surface), deep links to each cell's
// llama-swap /ui. No framework, no build step, no external assets: it
// must load on a LAN with no internet.

//go:embed fleet.html
var fleetPageFS embed.FS

// registerFleetPage mounts GET /ui/fleet (the path deliberately avoids
// colliding with llama-swap's /ui on cells). fleetd role only.
func (s *Server) registerFleetPage(mux *http.ServeMux) {
	mux.HandleFunc("GET /ui/fleet", func(w http.ResponseWriter, r *http.Request) {
		data, err := fleetPageFS.ReadFile("fleet.html")
		if err != nil {
			http.Error(w, "page missing", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(data)
	})
}
