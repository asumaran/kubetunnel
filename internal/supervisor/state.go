package supervisor

import "time"

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
	Name       string    `json:"name"`
	Hostname   string    `json:"hostname"`
	LocalPort  int       `json:"local_port"`
	State      State     `json:"state"`
	PID        int       `json:"pid,omitempty"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	Uptime     string    `json:"uptime,omitempty"`
	Restarts   int       `json:"restarts"`
	LastError  string    `json:"last_error,omitempty"`
	HealthOK   bool      `json:"health_ok"`
	LastHealth time.Time `json:"last_health,omitempty"`
}
