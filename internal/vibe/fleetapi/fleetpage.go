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

// fleetPagePath is the page's route (deliberately avoiding a collision
// with llama-swap's /ui on cells). It is the ONE AccessPublic entry in
// the route table: the file carries no fleet data, and it must load in a
// bare browser tab so its own token prompt can run.
const fleetPagePath = "/ui/fleet"

// fleetPageHandler serves the embedded page. It takes a *Server for the
// route table's uniform handler signature and uses none of it — the page
// is a static asset.
func fleetPageHandler(*Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := fleetPageFS.ReadFile("fleet.html")
		if err != nil {
			http.Error(w, "page missing", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(data)
	}
}
