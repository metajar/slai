package tui

import (
	"strconv"

	tea "github.com/charmbracelet/bubbletea"
)

// priorityCmd raises ("up"), lowers ("down") or sends to the top ("top") the task under
// the Tasks cursor. The write goes through Options.SetPriority (the supervisor, same as
// `slai priority`); the TUI never writes the store itself.
func (m *Model) priorityCmd(spec string) tea.Cmd {
	t, ok := m.currentTask()
	if !ok {
		return nil
	}
	if m.opts.SetPriority == nil {
		m.setStatus("can't change priority here", true)
		return nil
	}
	set := m.opts.SetPriority
	return func() tea.Msg {
		p, err := set(t.ID, spec)
		if err != nil {
			return actionMsg{err: err}
		}
		return actionMsg{text: t.ID + " priority is now " + strconv.Itoa(p), followTask: t.ID}
	}
}
