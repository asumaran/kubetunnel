// Package tui implements the Bubble Tea dashboard for tunnelctl.
//
// The layout is built from consolidated Bubbles components: a table.Model for
// the tunnel list (alignment, cursor and scrolling), a viewport.Model for the
// log pane (real scrollback + follow), textinput.Model for the search/filter
// prompts, and help.Model for the footer keymap.
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
	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
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

// keyMap is the centralized keymap; it satisfies help.KeyMap so help.Model can
// render the footer.
type keyMap struct {
	Up      key.Binding
	Down    key.Binding
	Tab     key.Binding
	Restart key.Binding
	Reload  key.Binding
	Search  key.Binding
	Filter  key.Binding
	Pause   key.Binding
	Follow  key.Binding
	Clear   key.Binding
	Quit    key.Binding
}

func defaultKeys() keyMap {
	return keyMap{
		Up:      key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down:    key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		Tab:     key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "focus")),
		Restart: key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "restart")),
		Reload:  key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "reload")),
		Search:  key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "search")),
		Filter:  key.NewBinding(key.WithKeys("f"), key.WithHelp("f", "filter")),
		Pause:   key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "pause")),
		Follow:  key.NewBinding(key.WithKeys("F"), key.WithHelp("F", "follow")),
		Clear:   key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "clear")),
		Quit:    key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	}
}

func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Tab, k.Restart, k.Reload, k.Search, k.Filter, k.Pause, k.Follow, k.Clear, k.Quit}
}

func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Up, k.Down, k.Tab},
		{k.Restart, k.Reload, k.Search, k.Filter},
		{k.Pause, k.Follow, k.Clear, k.Quit},
	}
}

type model struct {
	client *control.Client

	width, height int

	tbl     table.Model
	vp      viewport.Model
	vpReady bool

	tunnels []supervisor.Status

	entries    []logging.Entry
	maxEntries int

	focus       focusArea
	search      string
	filter      string
	filterPred  logquery.Predicate
	searchInput textinput.Model
	filterInput textinput.Model

	keys keyMap
	help help.Model

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

	tbl := table.New(
		table.WithColumns(columns(120)),
		table.WithFocused(true),
	)
	ts := table.DefaultStyles()
	ts.Header = ts.Header.Bold(true).Foreground(lipgloss.Color("#c0caf5"))
	ts.Selected = ts.Selected.Foreground(lipgloss.Color("#1a1b26")).
		Background(lipgloss.Color("#bb9af7")).Bold(true)
	tbl.SetStyles(ts)

	hp := help.New()

	m := &model{
		client:      cli,
		maxEntries:  1000,
		focus:       focusTable,
		followMode:  true,
		searchInput: si,
		filterInput: fi,
		filterPred:  logquery.Always,
		tbl:         tbl,
		keys:        defaultKeys(),
		help:        hp,
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
		m.layout()
		m.refreshLogs()
		return m, nil

	case statusMsg:
		// Preserve selection by name across snapshots so the highlight doesn't
		// jump when the daemon reorders tunnels.
		prevName := m.currentName()
		m.tunnels = msg.snap.Tunnels
		m.tbl.SetRows(rows(m.tunnels))
		m.selectByName(prevName)
		m.layout()
		m.refreshLogs()
		return m, nil

	case logMsg:
		if m.paused {
			return m, nil
		}
		m.entries = append(m.entries, msg.e)
		if len(m.entries) > m.maxEntries {
			m.entries = m.entries[len(m.entries)-m.maxEntries:]
		}
		m.refreshLogs()
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
	// Text-entry focus: forward to the active input first.
	switch m.focus {
	case focusSearch:
		switch msg.Type {
		case tea.KeyEsc:
			m.setFocus(focusLogs)
			m.searchInput.Blur()
			return m, nil
		case tea.KeyEnter:
			m.search = m.searchInput.Value()
			m.setFocus(focusLogs)
			m.searchInput.Blur()
			m.refreshLogs()
			return m, nil
		}
		var cmd tea.Cmd
		m.searchInput, cmd = m.searchInput.Update(msg)
		m.search = m.searchInput.Value()
		m.refreshLogs()
		return m, cmd
	case focusFilter:
		switch msg.Type {
		case tea.KeyEsc:
			m.setFocus(focusLogs)
			m.filterInput.Blur()
			return m, nil
		case tea.KeyEnter:
			q := m.filterInput.Value()
			p, err := logquery.Parse(q)
			if err != nil {
				m.err = "filter error: " + err.Error()
				return m, nil
			}
			m.filter = q
			m.filterPred = p
			m.err = ""
			m.setFocus(focusLogs)
			m.filterInput.Blur()
			m.refreshLogs()
			return m, nil
		}
		var cmd tea.Cmd
		m.filterInput, cmd = m.filterInput.Update(msg)
		return m, cmd
	}

	// Global commands take priority over widget navigation, so keys like "f"
	// always mean "filter" and never the viewport's half-page-down.
	switch {
	case key.Matches(msg, m.keys.Quit):
		m.stopStreams()
		return m, tea.Quit
	case key.Matches(msg, m.keys.Tab):
		if m.focus == focusTable {
			m.setFocus(focusLogs)
		} else {
			m.setFocus(focusTable)
		}
		return m, nil
	case key.Matches(msg, m.keys.Restart):
		if name := m.currentName(); name != "" {
			go func() { _ = m.client.Restart(name) }()
		}
		return m, nil
	case key.Matches(msg, m.keys.Reload):
		go func() { _ = m.client.Reload() }()
		return m, nil
	case key.Matches(msg, m.keys.Search):
		m.setFocus(focusSearch)
		m.searchInput.Focus()
		m.searchInput.SetValue(m.search)
		return m, textinput.Blink
	case key.Matches(msg, m.keys.Filter):
		m.setFocus(focusFilter)
		m.filterInput.Focus()
		m.filterInput.SetValue(m.filter)
		return m, textinput.Blink
	case key.Matches(msg, m.keys.Pause):
		m.paused = !m.paused
		return m, nil
	case key.Matches(msg, m.keys.Follow):
		m.followMode = !m.followMode
		if m.followMode {
			m.vp.GotoBottom()
		}
		return m, nil
	case key.Matches(msg, m.keys.Clear):
		m.entries = nil
		m.refreshLogs()
		return m, nil
	case msg.Type == tea.KeyEsc:
		m.search = ""
		m.refreshLogs()
		return m, nil
	}

	// Otherwise forward navigation to the focused widget.
	var cmd tea.Cmd
	switch m.focus {
	case focusTable:
		m.tbl, cmd = m.tbl.Update(msg)
		m.refreshLogs() // selection may have changed
	case focusLogs:
		m.vp, cmd = m.vp.Update(msg)
	}
	return m, cmd
}

func (m *model) setFocus(a focusArea) {
	m.focus = a
	if a == focusTable {
		m.tbl.Focus()
	} else {
		m.tbl.Blur()
	}
}

// currentName is the name of the currently selected tunnel, or "".
func (m *model) currentName() string {
	c := m.tbl.Cursor()
	if c >= 0 && c < len(m.tunnels) {
		return m.tunnels[c].Name
	}
	return ""
}

func (m *model) selectByName(name string) {
	if name == "" {
		return
	}
	for i, t := range m.tunnels {
		if t.Name == name {
			m.tbl.SetCursor(i)
			return
		}
	}
}

func (m *model) stopStreams() {
	for _, c := range m.cancelFuncs {
		c()
	}
}

// layout recomputes component dimensions from the current terminal size and
// tunnel count. Safe to call repeatedly.
func (m *model) layout() {
	if m.width == 0 || m.height == 0 {
		return
	}
	headerH := 1
	footerH := 1
	contentH := m.height - headerH - footerH
	if contentH < 8 {
		contentH = 8
	}

	// Table box: header row + one line per tunnel + top/bottom border, capped so
	// the log pane keeps at least a usable height.
	tableViewH := len(m.tunnels) + 1 // +1 for the table's own header row
	if tableViewH < 2 {
		tableViewH = 2
	}
	tableBoxH := tableViewH + 2 // rounded border top+bottom
	maxTableBoxH := contentH - minLogsBoxH
	if tableBoxH > maxTableBoxH {
		tableBoxH = maxTableBoxH
		tableViewH = tableBoxH - 2
	}
	if tableViewH < 2 {
		tableViewH = 2
		tableBoxH = 4
	}

	// Content area inside a box = box.Width - 2 (border) - 2 (horizontal
	// padding). We render boxes at box.Width(m.width-2), so content is m.width-4.
	innerW := m.width - 4
	if innerW < 20 {
		innerW = 20
	}
	m.tbl.SetColumns(columns(innerW))
	m.tbl.SetWidth(innerW)
	// table.SetHeight(h) renders exactly h lines: 1 header + (h-1) rows.
	m.tbl.SetHeight(tableViewH)

	logsBoxH := contentH - tableBoxH
	if logsBoxH < minLogsBoxH {
		logsBoxH = minLogsBoxH
	}
	vpH := logsBoxH - 2 - m.logsHeaderLines() // border + title/input lines
	if vpH < 1 {
		vpH = 1
	}
	m.vp.Width = innerW
	m.vp.Height = vpH
	if m.followMode {
		m.vp.GotoBottom()
	}
	m.vpReady = true
}

const minLogsBoxH = 7

// logsHeaderLines is how many lines the log box reserves above the viewport
// (the title line, plus an input or filter/search tag line when present).
func (m *model) logsHeaderLines() int {
	lines := 1 // title
	if m.focus == focusSearch || m.focus == focusFilter || m.search != "" || m.filter != "" {
		lines++
	}
	return lines
}

// refreshLogs rebuilds the viewport content from the entries that pass the
// selected-tunnel, filter and search constraints.
func (m *model) refreshLogs() {
	if !m.vpReady {
		return
	}
	selName := m.currentName()
	needle := strings.ToLower(m.search)
	var b strings.Builder
	first := true
	for _, e := range m.entries {
		if selName != "" && e.Tunnel != "" && e.Tunnel != selName {
			continue
		}
		if m.filterPred != nil && !m.filterPred(e) {
			continue
		}
		line := formatEntry(e)
		if needle != "" {
			if !strings.Contains(strings.ToLower(line), needle) {
				continue
			}
			line = highlightSubstring(line, m.search)
		}
		if !first {
			b.WriteByte('\n')
		}
		b.WriteString(line)
		first = false
	}
	m.vp.SetContent(b.String())
	if m.followMode {
		m.vp.GotoBottom()
	}
}

func (m *model) View() string {
	if m.width == 0 {
		return "loading..."
	}

	title := titleStyle.Render(" kubetunnel ")
	subtitle := dim.Render("resilient kubectl port-forward daemon + local HTTPS reverse proxy")
	header := title + "  " + subtitle

	tableBox := m.tableBox()
	logsBox := m.logsBox()
	footer := m.footerView()

	return lipgloss.JoinVertical(lipgloss.Left, header, tableBox, logsBox, footer)
}

func (m *model) tableBox() string {
	box := boxStyle
	if m.focus == focusTable {
		box = focusedBox
	}
	return box.Width(m.width - 2).Render(m.tbl.View())
}

func (m *model) logsBox() string {
	selName := m.currentName()
	titleText := "logs"
	if selName != "" {
		titleText = "logs: " + selName
		if t := m.selectedTarget(); t != "" {
			titleText += "  →  " + t
		}
	}

	var sb strings.Builder
	sb.WriteString(dim.Render(titleText))
	sb.WriteString("\n")

	switch {
	case m.focus == focusSearch:
		sb.WriteString(inputStyle.Render("/"))
		sb.WriteString(m.searchInput.View())
		sb.WriteString("\n")
	case m.focus == focusFilter:
		sb.WriteString(inputStyle.Render("filter: "))
		sb.WriteString(m.filterInput.View())
		sb.WriteString("\n")
	case m.search != "" || m.filter != "":
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

	sb.WriteString(m.vp.View())

	box := boxStyle
	if m.focus == focusLogs || m.focus == focusSearch || m.focus == focusFilter {
		box = focusedBox
	}
	return box.Width(m.width - 2).Render(sb.String())
}

func (m *model) selectedTarget() string {
	c := m.tbl.Cursor()
	if c >= 0 && c < len(m.tunnels) {
		return m.tunnels[c].InternalTarget()
	}
	return ""
}

func (m *model) footerView() string {
	hints := m.help.View(m.keys)
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

// columns returns the table columns sized so the rendered table fills tableW
// (the table's content width). The table adds 1 space of padding on each side
// of every cell, so the visible width of a column is its Width + 2.
func columns(tableW int) []table.Column {
	const (
		nameW     = 18
		stateW    = 9
		uptimeW   = 9
		restartsW = 8
		healthW   = 6
		numCols   = 7
		padPerCol = 2
	)
	avail := tableW - numCols*padPerCol
	rest := avail - (nameW + stateW + uptimeW + restartsW + healthW)
	hostW, targetW := 28, 28
	if rest >= 24 {
		hostW = rest * 48 / 100
		targetW = rest - hostW
	}
	return []table.Column{
		{Title: "NAME", Width: nameW},
		{Title: "STATE", Width: stateW},
		{Title: "UPTIME", Width: uptimeW},
		{Title: "RESTARTS", Width: restartsW},
		{Title: "HEALTH", Width: healthW},
		{Title: "HOSTNAME", Width: hostW},
		{Title: "TARGET", Width: targetW},
	}
}

func rows(tunnels []supervisor.Status) []table.Row {
	out := make([]table.Row, 0, len(tunnels))
	for _, t := range tunnels {
		out = append(out, table.Row{
			t.Name,
			string(t.State),
			orDash(t.Uptime),
			fmt.Sprintf("%d", t.Restarts),
			healthText(t.HealthOK),
			t.Hostname,
			t.InternalTarget(),
		})
	}
	return out
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
