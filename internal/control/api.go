// Package control implements the HTTP API served over a Unix socket for
// communication between tunnelctl and the daemon.
package control

import (
	"github.com/asumaran/kubetunnel/internal/logging"
	"github.com/asumaran/kubetunnel/internal/supervisor"
)

// StatusResponse is what GET /status returns.
type StatusResponse struct {
	Tunnels []supervisor.Status `json:"tunnels"`
}

// LogResponse is what GET /logs returns.
type LogResponse struct {
	Entries []logging.Entry `json:"entries"`
}

// Event is what the /events SSE stream emits.
type Event struct {
	Type    string            `json:"type"`
	Tunnel  string            `json:"tunnel,omitempty"`
	Status  supervisor.Status `json:"status,omitempty"`
	Message string            `json:"message,omitempty"`
}

// Error is the JSON error format.
type Error struct {
	Error string `json:"error"`
}
