package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/oorbt/slai/internal/service"
)

type holdStub struct {
	held    bool
	name    string
	since   int64
	holds   []string
	release int
}

func (h *holdStub) wire(m *Model) {
	m.opts.CurrentHold = func() (string, int64, bool) { return h.name, h.since, h.held }
	m.opts.HoldTeam = func(n string) error { h.holds = append(h.holds, n); return nil }
	m.opts.ReleaseHold = func() error { h.release++; return nil }
}

func TestHoldBannerFollowsHoldState(t *testing.T) {
	m, _ := newModel(t)
	h := &holdStub{held: true, name: "lunch", since: time.Now().Add(-90 * time.Minute).Unix()}
	h.wire(m)
	m.Update(m.refreshCmd()())
	v := ansi.Strip(m.View())
	if !strings.Contains(v, "team on hold: lunch since") || !strings.Contains(v, "no wakes are being delivered") {
		t.Fatalf("banner missing:\n%s", v)
	}
	h.held = false
	m.Update(m.refreshCmd()())
	if v := ansi.Strip(m.View()); strings.Contains(v, "team on hold") {
		t.Fatalf("banner stayed after resume:\n%s", v)
	}
}

func TestHoldKeyAsksForAnOptionalNameThenCallsTheSupervisor(t *testing.T) {
	m, _ := newModel(t)
	h := &holdStub{}
	h.wire(m)
	m.Update(key("H"))
	if m.mode != mQuick || !m.quickHold {
		t.Fatalf("no prompt: mode=%v", m.mode)
	}
	typeText(m, "end of day")
	_, cmd := m.Update(key("enter"))
	msg := cmd().(actionMsg)
	if msg.err != nil || len(h.holds) != 1 || h.holds[0] != "end of day" || m.quickHold {
		t.Fatalf("holds=%v msg=%+v", h.holds, msg)
	}
	// an empty name is fine
	m.Update(key("H"))
	_, cmd = m.Update(key("enter"))
	cmd()
	if len(h.holds) != 2 || h.holds[1] != "hold" {
		t.Fatalf("holds=%v", h.holds)
	}
	// esc cancels
	m.Update(key("H"))
	m.Update(key("esc"))
	if m.mode != mList || m.quickHold || len(h.holds) != 2 {
		t.Fatal("esc did not cancel")
	}
}

func TestHoldKeyResumesAHeldTeam(t *testing.T) {
	m, _ := newModel(t)
	h := &holdStub{held: true, name: "x", since: time.Now().Unix()}
	h.wire(m)
	m.Update(m.refreshCmd()())
	_, cmd := m.Update(key("H"))
	if cmd == nil || m.mode == mQuick {
		t.Fatal("H on a held team should resume, not prompt")
	}
	if msg := cmd().(actionMsg); msg.err != nil || h.release != 1 || len(h.holds) != 0 {
		t.Fatalf("release=%d holds=%v msg=%+v", h.release, h.holds, msg)
	}
}

// The agent row's second line takes the liveness reason as is, so an agent the service
// reports as gated with "team hold" (internal/service: gates.go Reason) shows it.
func TestAgentRowsShowTeamHoldGate(t *testing.T) {
	m, _ := newModel(t)
	m.opts.IsPaused = func() bool { return true }
	m.opts.CurrentHold = func() (string, int64, bool) { return "lunch", time.Now().Unix(), true }
	m.Update(m.refreshCmd()())
	hl := service.Health{Liveness: "gated", Reason: "team hold"}
	for _, ag := range m.agents {
		if ag.ID == "pm" {
			if line := rowStatusLine(ag, m.tasks, m.events, hl, m.opts.Now(), ""); !strings.Contains(line, "team hold") {
				t.Fatalf("row line %q lacks the reason", line)
			}
		}
	}
	if v := ansi.Strip(m.View()); strings.Contains(v, "wake-ups paused") {
		t.Fatalf("the generic pause line duplicates the hold banner:\n%s", v)
	}
}
