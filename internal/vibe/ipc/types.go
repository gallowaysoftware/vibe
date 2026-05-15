// Package ipc defines the request/response types exchanged between the vibe
// CLI and the vibe daemon over the local unix socket.
package ipc

import "time"

type StartRequest struct {
	Profile string `json:"profile"`
}

type StartResponse struct {
	Status   Status        `json:"status"`
	Frontend *FrontendInfo `json:"frontend,omitempty"`
}

type StopResponse struct {
	Status Status `json:"status"`
}

type Status struct {
	Running     bool      `json:"running"`
	Ready       bool      `json:"ready"`
	Profile     string    `json:"profile,omitempty"`
	StartedAt   time.Time `json:"started_at,omitempty"`
	BackendAddr string    `json:"backend_addr,omitempty"`
	ProxyAddr   string    `json:"proxy_addr,omitempty"`
	PID         int       `json:"pid,omitempty"`
}

type FrontendInfo struct {
	App             string `json:"app,omitempty"`
	WroteFile       string `json:"wrote_file,omitempty"`
	RestartRequired bool   `json:"restart_required,omitempty"`
}

type ListResponse struct {
	Profiles []ProfileSummary `json:"profiles"`
}

type ProfileSummary struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Path        string `json:"path"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}
