package supervisor

import (
	"fmt"
	"time"
)

// State is the lifecycle state of a tunnel.
type State string

const (
	StateStopped  State = "Stopped"
	StateStarting State = "Starting"
	StateRunning  State = "Running"
	StateBackoff  State = "Backoff"
	StateFailing  State = "Failing"
)

// Status is a snapshot of a tunnel's state for the control API / TUI.
type Status struct {
	Name        string    `json:"name"`
	Hostname    string    `json:"hostname"`
	LocalPort   int       `json:"local_port"`
	KubeContext string    `json:"kube_context,omitempty"`
	Namespace   string    `json:"namespace,omitempty"`
	Target      string    `json:"target,omitempty"`
	RemotePort  int       `json:"remote_port,omitempty"`
	State       State     `json:"state"`
	PID         int       `json:"pid,omitempty"`
	StartedAt   time.Time `json:"started_at,omitempty"`
	Uptime      string    `json:"uptime,omitempty"`
	Restarts    int       `json:"restarts"`
	LastError   string    `json:"last_error,omitempty"`
	HealthOK    bool      `json:"health_ok"`
	LastHealth  time.Time `json:"last_health,omitempty"`
}

// InternalTarget is the in-cluster destination the tunnel forwards to, e.g.
// "digital-sta/svc/prometheus-operated:9090". Empty if the target is unknown.
func (s Status) InternalTarget() string {
	if s.Target == "" {
		return ""
	}
	return fmt.Sprintf("%s/%s:%d", s.Namespace, s.Target, s.RemotePort)
}
