package fleetmcp

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
)

// probe_model (fleet-control C8): ask a cell to measure one of its
// RESIDENT models against that model's own rolling baseline.
//
// Three things this tool deliberately does not do, because each is the
// phase's failure mode: it does not warm (a probe never loads a model —
// it refuses and says to warm first), it does not force past the guards
// (a busy box is a skip with a reason, not an override flag), and it
// does not act on the answer (degraded is a display state; the
// remediation verb is the existing unload_model).
func (s *Server) toolProbeModel(cell, model string, rebaseline bool) (string, error) {
	if _, ok := s.hosts.Cells[cell]; !ok {
		return "", fmt.Errorf("unknown cell %q (not in hosts.yaml)", cell)
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return "", fmt.Errorf("model is required")
	}

	latest := s.fleet.LatestProbe(cell, model)
	if why, ok := s.fleet.ProbeGuard(cell, model); !ok {
		// A skip is an ANSWER, not a failure: the caller asked whether the
		// box could be measured right now and it could not. The last known
		// result rides along so the agent still has something to say.
		return renderProbe(fmt.Sprintf("No probe issued: %s.", why), latest, cell, model)
	}
	if err := s.fleet.QueueCommand(cell, fleetapi.AnnounceCommand{Verb: "probe", Model: model, Rebaseline: rebaseline}); err != nil {
		return "", fmt.Errorf("probe %s on %s: %v", model, cell, err)
	}
	note := fmt.Sprintf("Probe queued for %s's next announce (delivered within one heartbeat; "+
		"the result rides the announce AFTER that, so read it back with fleet_status in ~2 heartbeats).", cell)
	if rebaseline {
		note += " The stored baseline for this model will be cleared first."
	}
	return renderProbe(note, latest, cell, model)
}

// renderProbe is the tool's payload: the note plus the last known block,
// as one JSON document so an agent never has to parse prose.
func renderProbe(note string, latest *fleetapi.AnnounceProbe, cell, model string) (string, error) {
	data, err := json.Marshal(struct {
		Cell   string                  `json:"cell"`
		Model  string                  `json:"model"`
		Note   string                  `json:"note"`
		Latest *fleetapi.AnnounceProbe `json:"latest_result"`
	}{Cell: cell, Model: model, Note: note, Latest: latest})
	if err != nil {
		return "", err
	}
	return string(data), nil
}
