// Package tui implements the Bubble Tea dashboard for tunnelctl.
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/asumaran/kubetunnel/internal/control"
	"github.com/asumaran/kubetunnel/internal/logging"
	"github.com/asumaran/kubetunnel/internal/logquery"
	"github.com/asumaran/kubetunnel/internal/supervisor"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Run starts the TUI against the daemon on `socket`.
func Run(socket string) error {
	cli := control.NewClient(socket)
	// Sanity check.
	if _, err := cli.Status(); err != nil {
		return fmt.Errorf("cannot reach daemon at %s: %w", socket, err)
	}
	m := newModel(cli)
	prog = tea.NewProgram(m, tea.WithAltScreen())
	_, err := prog.Run()
	return err
}

type focusArea int

const (
	focusTable focusArea = iota
	focusLogs
	focusSearch
	focusFilter
)

type statusMsg struct{ snap control.StatusResponse }
type logMsg struct{ e logging.Entry }
type errMsg struct{ err error }
type tickMsg struct{}

type model struct {
	client *control.Client

	width, height int

	tunnels  []supervisor.Status
	selected int

	entries    []logging.Entry
	maxEntries int

	focus       focusArea
	search      string
	filter      string
	filterPred  logquery.Predicate
	searchInput textinput.Model
	filterInput textinput.Model

	followMode bool
	paused     bool
	err        string

	cancelFuncs []context.CancelFunc
}

func newModel(cli *control.Client) *model {
	si := textinput.New()
	si.Placeholder = "search..."
	si.CharLimit = 120

	fi := textinput.New()
	fi.Placeholder = "level:error AND tunnel:api"
	fi.CharLimit = 200

	m := &model{
		client:      cli,
		maxEntries:  1000,
		focus:       focusTable,
		followMode:  true,
		searchInput: si,
		filterInput: fi,
		filterPred:  logquery.Always,
	}
	return m
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(
		m.startStatusStream(),
		m.startLogStream(),
		tick(),
	)
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m *model) startStatusStream() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithCancel(context.Background())
		m.cancelFuncs = append(m.cancelFuncs, cancel)
		ch, err := m.client.StreamStatus(ctx)
		if err != nil {
			return errMsg{err}
		}
		// Pull the first message synchronously so the UI has data.
		select {
		case s, ok := <-ch:
			if !ok {
				return errMsg{fmt.Errorf("status stream closed")}
			}
			go m.pumpStatus(ch)
			return statusMsg{s}
		case <-time.After(2 * time.Second):
			return errMsg{fmt.Errorf("status stream timeout")}
		}
	}
}

func (m *model) pumpStatus(ch <-chan control.StatusResponse) {
	for s := range ch {
		prog.Send(statusMsg{s})
	}
}

func (m *model) startLogStream() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithCancel(context.Background())
		m.cancelFuncs = append(m.cancelFuncs, cancel)
		ch, err := m.client.StreamLogs(ctx, "", "")
		if err != nil {
			return errMsg{err}
		}
		go m.pumpLogs(ch)
		return nil
	}
}

func (m *model) pumpLogs(ch <-chan logging.Entry) {
	for e := range ch {
		prog.Send(logMsg{e})
	}
}

// prog is a package-level pointer to the running tea.Program so background
// goroutines can post messages. Set by Run().
var prog *tea.Program

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	case statusMsg:
		// Preserve selection by name across snapshots so the highlight
		// doesn't jump when map iteration order shifts.
		var selName string
		if m.selected < len(m.tunnels) {
			selName = m.tunnels[m.selected].Name
		}
		m.tunnels = msg.snap.Tunnels
		m.selected = 0
		if selName != "" {
			for i, t := range m.tunnels {
				if t.Name == selName {
					m.selected = i
					break
				}
			}
		}
		if m.selected >= len(m.tunnels) {
			m.selected = 0
		}
		return m, nil
	case logMsg:
		if m.paused {
			return m, nil
		}
		m.entries = append(m.entries, msg.e)
		if len(m.entries) > m.maxEntries {
			m.entries = m.entries[len(m.entries)-m.maxEntries:]
		}
		return m, nil
	case errMsg:
		m.err = msg.err.Error()
		return m, nil
	case tickMsg:
		return m, tick()
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// If input fields have focus, forward keys to them first.
	if m.focus == focusSearch {
		if msg.Type == tea.KeyEsc {
			m.focus = focusLogs
			m.searchInput.Blur()
			return m, nil
		}
		if msg.Type == tea.KeyEnter {
			m.search = m.searchInput.Value()
			m.focus = focusLogs
			m.searchInput.Blur()
			return m, nil
		}
		var cmd tea.Cmd
		m.searchInput, cmd = m.searchInput.Update(msg)
		m.search = m.searchInput.Value()
		return m, cmd
	}
	if m.focus == focusFilter {
		if msg.Type == tea.KeyEsc {
			m.focus = focusLogs
			m.filterInput.Blur()
			return m, nil
		}
		if msg.Type == tea.KeyEnter {
			q := m.filterInput.Value()
			p, err := logquery.Parse(q)
			if err != nil {
				m.err = "filter error: " + err.Error()
				return m, nil
			}
			m.filter = q
			m.filterPred = p
			m.err = ""
			m.focus = focusLogs
			m.filterInput.Blur()
			return m, nil
		}
		var cmd tea.Cmd
		m.filterInput, cmd = m.filterInput.Update(msg)
		return m, cmd
	}

	switch msg.String() {
	case "q", "ctrl+c":
		m.stopStreams()
		return m, tea.Quit
	case "tab":
		if m.focus == focusTable {
			m.focus = focusLogs
		} else {
			m.focus = focusTable
		}
	case "up", "k":
		if m.focus == focusTable && m.selected > 0 {
			m.selected--
		}
	case "down", "j":
		if m.focus == focusTable && m.selected < len(m.tunnels)-1 {
			m.selected++
		}
	case "r":
		if len(m.tunnels) > 0 {
			name := m.tunnels[m.selected].Name
			go func() { _ = m.client.Restart(name) }()
		}
	case "R":
		go func() { _ = m.client.Reload() }()
	case "/":
		m.focus = focusSearch
		m.searchInput.Focus()
		m.searchInput.SetValue(m.search)
		return m, textinput.Blink
	case "f":
		m.focus = focusFilter
		m.filterInput.Focus()
		m.filterInput.SetValue(m.filter)
		return m, textinput.Blink
	case "p":
		m.paused = !m.paused
	case "F":
		m.followMode = !m.followMode
	case "c":
		m.entries = nil
	case "esc":
		m.search = ""
	}
	return m, nil
}

func (m *model) stopStreams() {
	for _, c := range m.cancelFuncs {
		c()
	}
}

func (m *model) View() string {
	if m.width == 0 {
		return "loading..."
	}
	title := titleStyle.Render(" kubetunnel ")
	subtitle := dim.Render("resilient kubectl port-forward daemon + local HTTPS reverse proxy")
	header := title + "  " + subtitle
	headerH := lipgloss.Height(header)
	footerH := 1
	// Each bordered box adds 2 lines (top+bottom border) that Height() does
	// NOT include, so we subtract 4 for the two boxes.
	avail := m.height - headerH - footerH - 4 - 1
	if avail < 10 {
		avail = 10
	}
	tableH := min(len(m.tunnels)+2, avail/3)
	if tableH < 4 {
		tableH = 4
	}
	logsH := avail - tableH
	if logsH < 5 {
		logsH = 5
	}

	tableBox := m.renderTable(m.width-4, tableH)
	logsBox := m.renderLogs(m.width-4, logsH)
	footer := m.renderFooter()

	return lipgloss.JoinVertical(lipgloss.Left,
		header,
		tableBox,
		logsBox,
		footer,
	)
}

func (m *model) renderTable(w, h int) string {
	var sb strings.Builder
	sb.WriteString(headerRow.Render(fmt.Sprintf("%-20s %-10s %-10s %-8s %-6s %-42s %s", "NAME", "STATE", "UPTIME", "RESTARTS", "HEALTH", "HOSTNAME", "TARGET")))
	sb.WriteString("\n")
	for i, t := range m.tunnels {
		line := fmt.Sprintf("%-20s %-10s %-10s %-8d %-6s %-42s %s",
			trunc(t.Name, 20),
			t.State,
			orDash(t.Uptime),
			t.Restarts,
			healthText(t.HealthOK),
			trunc(t.Hostname, 42),
			t.InternalTarget(),
		)
		st := stateStyle(string(t.State))
		rendered := st.Render(line)
		if i == m.selected && m.focus == focusTable {
			rendered = focused.Render("▶ ") + rendered
		} else {
			rendered = "  " + rendered
		}
		sb.WriteString(rendered)
		sb.WriteString("\n")
	}
	box := boxStyle
	if m.focus == focusTable {
		box = focusedBox
	}
	return box.Width(w).Height(h).Render(sb.String())
}

func (m *model) renderLogs(w, h int) string {
	var selName, selTarget string
	if len(m.tunnels) > 0 {
		selName = m.tunnels[m.selected].Name
		selTarget = m.tunnels[m.selected].InternalTarget()
	}
	title := "logs"
	if selName != "" {
		title = "logs: " + selName
		if selTarget != "" {
			title += "  →  " + selTarget
		}
	}
	var sb strings.Builder
	sb.WriteString(dim.Render(title))
	sb.WriteString("\n")
	if m.focus == focusSearch {
		sb.WriteString(inputStyle.Render("/"))
		sb.WriteString(m.searchInput.View())
		sb.WriteString("\n")
	} else if m.focus == focusFilter {
		sb.WriteString(inputStyle.Render("filter: "))
		sb.WriteString(m.filterInput.View())
		sb.WriteString("\n")
	} else if m.search != "" || m.filter != "" {
		tags := []string{}
		if m.filter != "" {
			tags = append(tags, "filter="+m.filter)
		}
		if m.search != "" {
			tags = append(tags, "search="+m.search)
		}
		sb.WriteString(dim.Render(strings.Join(tags, "  ")))
		sb.WriteString("\n")
	}

	// Filter + search the entries.
	visible := make([]logging.Entry, 0, len(m.entries))
	needle := strings.ToLower(m.search)
	for _, e := range m.entries {
		if selName != "" && e.Tunnel != "" && e.Tunnel != selName {
			continue
		}
		if m.filterPred != nil && !m.filterPred(e) {
			continue
		}
		if needle != "" {
			if !strings.Contains(strings.ToLower(formatEntry(e)), needle) {
				continue
			}
		}
		visible = append(visible, e)
	}
	// Show only the last N that fit.
	maxLines := h - 4
	if maxLines < 1 {
		maxLines = 1
	}
	if len(visible) > maxLines {
		visible = visible[len(visible)-maxLines:]
	}
	for _, e := range visible {
		line := formatEntry(e)
		if needle != "" {
			line = highlightSubstring(line, m.search)
		}
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	box := boxStyle
	if m.focus == focusLogs || m.focus == focusSearch || m.focus == focusFilter {
		box = focusedBox
	}
	return box.Width(w).Height(h).Render(sb.String())
}

func (m *model) renderFooter() string {
	hints := dim.Render("[q] quit  [tab] focus  [r] restart  [R] reload  [/] search  [f] filter  [p] pause  [F] follow  [c] clear")
	var parts []string
	if m.paused {
		parts = append(parts, warn.Render("PAUSED"))
	}
	if !m.followMode {
		parts = append(parts, dim.Render("follow off"))
	}
	if m.err != "" {
		parts = append(parts, bad.Render("err: "+m.err))
	}
	if len(parts) == 0 {
		return hints
	}
	return hints + "   " + strings.Join(parts, "  ")
}

func formatEntry(e logging.Entry) string {
	ts := e.Time.Local().Format("15:04:05")
	level := strings.ToUpper(e.Level)
	if level == "" {
		level = "    "
	}
	stream := string(e.Stream)
	tname := e.Tunnel
	if tname == "" {
		tname = "-"
	}
	event := e.Event
	msg := e.Msg
	extra := ""
	for k, v := range e.Fields {
		extra += fmt.Sprintf(" %s=%v", k, v)
	}
	return fmt.Sprintf("%s %-5s %-7s [%s] %s %s%s", ts, level, stream, tname, event, msg, extra)
}

func highlightSubstring(s, needle string) string {
	if needle == "" {
		return s
	}
	ls := strings.ToLower(s)
	ln := strings.ToLower(needle)
	var out strings.Builder
	i := 0
	for {
		idx := strings.Index(ls[i:], ln)
		if idx == -1 {
			out.WriteString(s[i:])
			return out.String()
		}
		out.WriteString(s[i : i+idx])
		out.WriteString(highlight.Render(s[i+idx : i+idx+len(needle)]))
		i += idx + len(needle)
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func healthText(ok bool) string {
	if ok {
		return "OK"
	}
	return "FAIL"
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
