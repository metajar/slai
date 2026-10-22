package tui

import (
	"errors"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/oorbt/slai/internal/harness"
	"github.com/oorbt/slai/internal/hooks"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/supervisor"
)

var errNoStore = errors.New("no store")

// redirectBlock says why a course correction cannot be sent to a now, or "" when
// it can. The supervisor re-checks the pane before it presses Escape; this only
// keeps the TUI from queueing something that cannot work.
func (m *Model) redirectBlock(a store.Agent) string {
	switch {
	case a.Status == "dead" || a.Status == "errored":
		return a.ID + " is not running — press v to revive it"
	case m.opts.Supervisor == "":
		return "no supervisor is running to interrupt " + a.ID + " (start one with slai up)"
	case m.paused:
		return "the team is paused — press p to resume before redirecting"
	case m.heldAgents[a.ID]:
		return a.ID + " is paused — press u to resume it before redirecting"
	case a.Status == "waiting_on_user":
		return a.ID + " is waiting for your answer — use space to answer it"
	case a.Status != "working":
		return a.ID + " is not busy (" + a.Status + ") — use space for an ordinary message"
	}
	if m.snapFor == a.ID && m.snapErr == "" {
		if h, err := harness.For(harness.Kind(m.harnessKind(a.ID))); err == nil && h.Features().PermissionPrompts {
			if ex := hooks.ModalExcerptWith(h.PaneRules(), m.snap.Body); ex != "" {
				return a.ID + " has a permission dialog open — answer it first (enter to focus)"
			}
		}
	}
	return ""
}

// openRedirect is the `c` key: course-correct the selected busy agent.
func (m *Model) openRedirect() tea.Cmd {
	a, ok := m.curAgent()
	if !ok {
		m.setStatus("select an agent first", true)
		return nil
	}
	if why := m.redirectBlock(a); why != "" {
		m.setStatus("can't redirect: "+why, true)
		return nil
	}
	m.mode, m.quickTo, m.quickRedirect = mQuick, a.ID, true
	m.quick.SetValue("")
	m.quick.Focus()
	return textinput.Blink
}

// redirectCmd queues the correction. Only the supervisor presses Escape and wakes the
// agent; the TUI writes the message and the request.
func (m *Model) redirectCmd(a store.Agent, text string) tea.Cmd {
	st := m.opts.Store
	return func() tea.Msg {
		if st == nil {
			return actionMsg{err: errNoStore}
		}
		if _, err := supervisor.RequestRedirect(st, a, strings.TrimSpace(text)); err != nil {
			return actionMsg{err: err}
		}
		return actionMsg{text: "interrupting " + a.ID + " with your correction"}
	}
}
