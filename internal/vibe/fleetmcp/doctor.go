package fleetmcp

import (
	"context"
	"encoding/json"
)

// fleet_doctor (fleet-control C13). The same DoctorReport
// GET /api/fleet/doctor serves and `vibe fleet doctor` renders — one
// document for humans and agents, C9's rule.
//
// It is a READ tool in a facade whose other verbs actuate, so the
// description says so twice: an agent that reads "doctor" and infers a
// repair verb would be the one caller most likely to try fixing things
// during an incident.
func (s *Server) toolFleetDoctor(ctx context.Context) (string, error) {
	rep := s.fleet.Doctor(ctx)
	data, err := json.Marshal(rep)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
