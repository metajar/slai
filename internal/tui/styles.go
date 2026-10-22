// Package tui implements the main slai terminal UI (PRD section 7).
package tui

import "github.com/charmbracelet/lipgloss"

var (
	colAmber  = lipgloss.Color("214")
	colGreen  = lipgloss.Color("42")
	colBlue   = lipgloss.Color("75")
	colRed    = lipgloss.Color("203")
	colGrey   = lipgloss.Color("244")
	colViolet = lipgloss.Color("141")

	styleDim      = lipgloss.NewStyle().Foreground(colGrey)
	styleBold     = lipgloss.NewStyle().Bold(true)
	styleSelected = lipgloss.NewStyle().Reverse(true)
	styleAmber    = lipgloss.NewStyle().Foreground(colAmber).Bold(true)
	styleErr      = lipgloss.NewStyle().Foreground(colRed)
	styleTitle    = lipgloss.NewStyle().Bold(true).Foreground(colViolet)

	paneBorder      = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colGrey)
	paneBorderFocus = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colBlue)
	// paneBorderFocused marks a pane that is receiving keystrokes (focus mode).
	edgeDim, edgeBlue = lipgloss.NewStyle().Foreground(colGrey), lipgloss.NewStyle().Foreground(colBlue)
	edgeGreen         = lipgloss.NewStyle().Foreground(colGreen).Bold(true)
	styleFocusTag     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("16")).Background(colGreen)
)

// statusStyle returns the colour style for an agent status.
func statusStyle(status string) lipgloss.Style {
	switch status {
	case "working":
		return lipgloss.NewStyle().Foreground(colGreen)
	case "waiting", "waiting_on_user":
		return styleAmber
	case "finished":
		return lipgloss.NewStyle().Foreground(colBlue)
	case "dead", "errored":
		return lipgloss.NewStyle().Foreground(colRed)
	}
	return lipgloss.NewStyle().Foreground(colGrey)
}
