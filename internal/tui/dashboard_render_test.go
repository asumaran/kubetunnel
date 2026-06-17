package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/asumaran/kubetunnel/internal/control"
	"github.com/asumaran/kubetunnel/internal/logging"
	"github.com/asumaran/kubetunnel/internal/supervisor"
	"github.com/charmbracelet/lipgloss"
)

// TestDashboardFrame renders a full frame off-screen and checks the layout
// invariants that are easy to break: every line is exactly the terminal width
// (no wrap/overflow), all tunnels appear as rows, and the columns/target are
// present.
func TestDashboardFrame(t *testing.T) {
	m := newModel(control.NewClient("/dev/null"))
	m.tunnels = []supervisor.Status{
		{Name: "cms-shield", State: supervisor.StateRunning, Uptime: "1h2m", Restarts: 1, HealthOK: true, Hostname: "cms-shield-api.sta.k8s.masmovil.com", Namespace: "digital-sta", Target: "svc/cms-shield-api", RemotePort: 80},
		{Name: "prometheus-digital", State: supervisor.StateFailing, Restarts: 7, HealthOK: false, Hostname: "digital--prometheus.sta.k8s.masmovil.com", Namespace: "digital-sta", Target: "svc/prometheus-operated", RemotePort: 9090},
	}
	m.tbl.SetRows(rows(m.tunnels))
	m.width, m.height = 110, 24
	m.layout()
	m.entries = []logging.Entry{
		{Time: time.Now(), Level: "info", Tunnel: "cms-shield", Event: "ready", Msg: "tunnel up"},
		{Time: time.Now(), Level: "error", Tunnel: "prometheus-digital", Event: "exit", Msg: "forbidden"},
	}
	m.refreshLogs()

	frame := m.View()

	for i, l := range strings.Split(frame, "\n") {
		if w := lipgloss.Width(l); w > m.width {
			t.Errorf("line %d overflows width: got %d, want <= %d: %q", i, w, m.width, l)
		}
	}
	for _, want := range []string{
		"NAME", "STATE", "HEALTH", "HOSTNAME", "TARGET",
		"cms-shield", "prometheus-digital",
		"digital-sta/svc/cms-shield-api:80", // selected tunnel's full target in the logs title
	} {
		if !strings.Contains(frame, want) {
			t.Errorf("frame missing %q", want)
		}
	}
}
