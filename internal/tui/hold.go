package tui

import (
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// holdBannerLine is the banner while `slai hold` is in force.
func holdBannerLine(name string, since int64, now time.Time) string {
	if name == "" {
		name = "hold"
	}
	s := "⏸ team on hold: " + name
	if since > 0 {
		t := time.Unix(since, 0)
		s += fmt.Sprintf(" since %s (%s ago)", t.Format("15:04"), ageShort(now.Sub(t)))
	}
	return s + ", no wakes are being delivered (H resumes)"
}

// holdKey is `H`: resume when the team is on hold, otherwise ask for an optional name.
func (m *Model) holdKey() tea.Cmd {
	if m.held {
		return m.releaseHoldCmd()
	}
	if m.opts.HoldTeam == nil {
		m.setStatus("holding the team needs the supervisor (start it with slai up)", true)
		return nil
	}
	m.mode, m.quickHold, m.quickTo = mQuick, true, ""
	m.quick.SetValue("")
	m.quick.Focus()
	return textinput.Blink
}

func (m *Model) holdCmd(name string) tea.Cmd {
	hold := m.opts.HoldTeam
	if name == "" {
		name = "hold"
	}
	return func() tea.Msg {
		if err := hold(name); err != nil {
			return actionMsg{err: err}
		}
		return actionMsg{text: "team on hold (" + name + "): sessions stay up, nothing is woken"}
	}
}

func (m *Model) releaseHoldCmd() tea.Cmd {
	rel := m.opts.ReleaseHold
	if rel == nil {
		m.setStatus("resuming needs the supervisor (start it with slai up)", true)
		return nil
	}
	return func() tea.Msg {
		if err := rel(); err != nil {
			return actionMsg{err: err}
		}
		return actionMsg{text: "team resumed"}
	}
}
