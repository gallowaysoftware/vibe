package fleetmcp

import (
	"encoding/json"
	"fmt"
)

// The C9 notifier's two agent verbs. Both are deliberately thin: the
// policy lives in fleetapi/fleetnotify, and an agent that could tune
// dwells or rate limits per call would be able to mute the fleet by
// accident.

// toolNotifyScope declares away/home. It gates NOTIFICATION only —
// nothing about a cell changes — and the reply says so, because an agent
// relaying "I've set the fleet to away" must not imply anything was
// drained.
func (s *Server) toolNotifyScope(scope, reason, until string) (string, error) {
	sc, err := s.fleet.SetNotifyScope(scope, reason, until, "mcp")
	if err != nil {
		return "", err
	}
	out := map[string]any{
		"scope": sc.Scope,
		"since": sc.Since,
		"note": "notification delivery only: no cell was drained, no catalog changed. " +
			"Alarms keep firing and stay visible in fleet_status while away.",
	}
	if sc.Until != nil {
		out["until"] = *sc.Until
	} else if sc.Scope == "away" {
		out["warning"] = "no `until` set: this away window ends only when someone declares home."
	}
	if sc.Reason != "" {
		out["reason"] = sc.Reason
	}
	data, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// toolNotifyTest sends one explicit message through the real delivery
// path.
func (s *Server) toolNotifyTest(message string) (string, error) {
	if message == "" {
		message = "test notification from vibe fleet"
	}
	if err := s.fleet.SendNotification("vibe fleet: test", message); err != nil {
		return "", err
	}
	return fmt.Sprintf("%q queued for delivery; it bypasses the away gate and the dwell, so a "+
		"failure to arrive is a delivery failure (check fleet_status's notify.delivery block)", message), nil
}
