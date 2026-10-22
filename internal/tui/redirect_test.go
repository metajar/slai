package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/supervisor"
	"github.com/oorbt/slai/internal/tmux"
)

func redirectModel(t *testing.T, status string) (*Model, *fakePane, *store.Store) {
	m, fp, s := paneModel(t)
	m.opts.Supervisor = "daemon"
	_ = s.SetAgentStatus("eng-2", status)
	m.Update(m.refreshCmd()())
	m.cursorTo("eng-2")
	m.sel = "eng-2"
	return m, fp, s
}

func typeText(m *Model, text string) {
	for _, r := range text {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

func TestRedirectKeyQueuesCorrectionForTheSupervisorOnly(t *testing.T) {
	m, fp, s := redirectModel(t, "working")
	m.Update(key("c"))
	if m.mode != mQuick || !m.quickRedirect || m.quickTo != "eng-2" {
		t.Fatalf("mode=%v redirect=%v to=%q status=%q", m.mode, m.quickRedirect, m.quickTo, m.status)
	}
	if !strings.Contains(ansi.Strip(m.View()), "interrupt eng-2") {
		t.Fatal("redirect prompt not labelled")
	}
	typeText(m, "use the other API")
	_, cmd := m.Update(key("enter"))
	if cmd == nil {
		t.Fatal("enter produced no command")
	}
	cmd()
	msgs, _ := s.UndeliveredFor("eng-2")
	if len(msgs) != 1 || msgs[0].Kind != "note" || msgs[0].From != "user" ||
		msgs[0].Body != "Course correction from user: use the other API" {
		t.Fatalf("messages = %+v", msgs)
	}
	if v, ok, _ := s.GetKV(supervisor.KVRedirectPrefix + "eng-2"); !ok || v == "" {
		t.Fatal("no interrupt request for the supervisor")
	}
	if len(fp.seqs) != 0 || len(fp.lines) != 0 || len(fp.pastes) != 0 {
		t.Fatalf("the TUI must not touch tmux: seqs=%v lines=%v", fp.seqs, fp.lines)
	}
	if m.mode != mList || m.quickRedirect {
		t.Fatal("prompt left open")
	}
}

func TestRedirectRefusedWithAVisibleReason(t *testing.T) {
	cases := []struct {
		name, status, want string
		setup              func(*Model)
	}{
		{"dead", "dead", "not running", nil},
		{"idle", "idle", "not busy", nil},
		{"asking", "waiting_on_user", "waiting for your answer", nil},
		{"team paused", "working", "team is paused", func(m *Model) { m.paused = true }},
		{"agent paused", "working", "is paused", func(m *Model) { m.heldAgents = map[string]bool{"eng-2": true} }},
		{"no supervisor", "working", "no supervisor", func(m *Model) { m.opts.Supervisor = "" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, _, s := redirectModel(t, c.status)
			if c.setup != nil {
				c.setup(m)
			}
			m.Update(key("c"))
			if m.mode == mQuick {
				t.Fatal("prompt opened")
			}
			if !m.statusE || !strings.Contains(m.status, c.want) {
				t.Fatalf("status = %q (err=%v), want %q", m.status, m.statusE, c.want)
			}
			if msgs, _ := s.UndeliveredFor("eng-2"); len(msgs) != 0 {
				t.Fatal("something was queued")
			}
		})
	}
}

func TestRedirectRefusedOverPermissionDialog(t *testing.T) {
	m, fp, s := redirectModel(t, "working")
	body := "Do you want to proceed?\n❯ 1. Yes\n  2. No\n"
	pw, ph := m.previewSize()
	m.Update(previewMsg{agent: "eng-2", snap: tmux.Snapshot{Body: body, W: pw, H: ph}})
	m.Update(key("c"))
	if m.mode == mQuick || !strings.Contains(m.status, "permission dialog") {
		t.Fatalf("mode=%v status=%q", m.mode, m.status)
	}
	if msgs, _ := s.UndeliveredFor("eng-2"); len(msgs) != 0 || len(fp.seqs) != 0 {
		t.Fatal("nothing may be queued or typed")
	}
}

func TestCourseCorrectionAppearsInAgentDetail(t *testing.T) {
	m, _, s := redirectModel(t, "working")
	a, _ := m.curAgent()
	if _, err := supervisor.RequestRedirect(s, a, "switch to sqlite"); err != nil {
		t.Fatal(err)
	}
	m.Update(key("I"))
	if v := ansi.Strip(m.vp.View()); !strings.Contains(v, "Course corrections") || !strings.Contains(v, "switch to sqlite") {
		t.Fatalf("detail lacks the correction:\n%s", v)
	}
}

func TestAgentDetailShowsLaneDenials(t *testing.T) {
	m, _, s := redirectModel(t, "working")
	view := func() string { m.Update(key("I")); return ansi.Strip(m.vp.View()) }
	if v := view(); !strings.Contains(v, "Lane denials") || !strings.Contains(v, "none in the last 24h") {
		t.Fatalf("zero denials must render cleanly:\n%s", v)
	}
	m.Update(key("esc"))
	for _, p := range []string{"/etc/a", "/etc/b", "/etc/c", "/etc/d"} {
		_ = s.AddEvent("eng-2", "lane_denied", `{"agent":"eng-2","tool":"Write","path":"`+p+`","reason":"r"}`)
	}
	_ = s.AddEvent("eng-1", "lane_denied", `{"tool":"Bash","path":"other"}`)
	v := view()
	if !strings.Contains(v, "4 in the last 24h") || !strings.Contains(v, "Write /etc/d") || !strings.Contains(v, "Write /etc/b") ||
		strings.Contains(v, "/etc/a") || strings.Contains(v, "other") {
		t.Fatalf("detail:\n%s", v)
	}
}
