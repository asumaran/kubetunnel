package tui

import "github.com/charmbracelet/lipgloss"

var (
	border     = lipgloss.RoundedBorder()
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7aa2f7"))
	dim        = lipgloss.NewStyle().Foreground(lipgloss.Color("#565f89"))
	ok         = lipgloss.NewStyle().Foreground(lipgloss.Color("#9ece6a"))
	warn       = lipgloss.NewStyle().Foreground(lipgloss.Color("#e0af68"))
	bad        = lipgloss.NewStyle().Foreground(lipgloss.Color("#f7768e"))
	boxStyle   = lipgloss.NewStyle().Border(border).BorderForeground(lipgloss.Color("#414868")).Padding(0, 1)
	focusedBox = lipgloss.NewStyle().Border(border).BorderForeground(lipgloss.Color("#bb9af7")).Padding(0, 1)
	inputStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#e0af68"))
	highlight  = lipgloss.NewStyle().Background(lipgloss.Color("#e0af68")).Foreground(lipgloss.Color("#1a1b26"))
)
