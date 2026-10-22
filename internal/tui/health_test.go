package tui

import (
	"encoding/json"
	"github.com/oorbt/slai/internal/supervisor"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
)

func raiseKV(t *testing.T, s *store.Store, e store.Escalation) {
	t.Helper()
	b, _ := json.Marshal(e)
	if err := s.SetKV("esc."+e.Kind+"."+e.Agent, string(b)); err != nil {
		t.Fatal(err)
	}
}

func screen(m *Model) string { return ansi.Strip(m.View()) }

func TestEscalationBannerPersistsDismissesAndReshows(t *testing.T) {
	m, s := newModel(t)
	now := time.Unix(1_000_000, 0)
	m.opts.Now = func() time.Time { return now }
	raiseKV(t, s, store.Escalation{Agent: "eng-1", Kind: "revive_gave_up", Text: "3 attempts", Since: 100, User: true})
	raiseKV(t, s, store.Escalation{Agent: "eng-2", Kind: "stalled", Text: "orch only", Since: 101, User: false})
	m.Update(m.refreshCmd()())
	v := screen(m)
	if !strings.Contains(v, "eng-1: revive gave up — 3 attempts") {
		t.Fatalf("banner missing:\n%s", v)
	}
	if strings.Contains(v, "orch only") {
		t.Fatal("non-user escalation must not banner")
	}
	// Esc hides locally; store untouched; persists across refreshes.
	m.Update(key("esc"))
	m.Update(m.refreshCmd()())
	if strings.Contains(screen(m), "revive gave up") {
		t.Fatal("banner should be hidden after esc")
	}
	if esc, _ := s.OpenEscalations(); len(esc) != 2 {
		t.Fatalf("store state cleared: %v", esc)
	}
	// A new escalation shows again; x dismisses it.
	raiseKV(t, s, store.Escalation{Agent: "pm", Kind: "modal_stuck", Text: "perm", Since: 200, User: true})
	m.Update(m.refreshCmd()())
	if v := screen(m); !strings.Contains(v, "pm: stuck on a prompt") || strings.Contains(v, "revive gave up") {
		t.Fatalf("new escalation should show alone:\n%s", v)
	}
	m.Update(key("x"))
	if strings.Contains(screen(m), "stuck on a prompt") {
		t.Fatal("x should hide the banner")
	}
	if m.mode == mConfirm {
		t.Fatal("x must not open the stop confirm while dismissing")
	}
}

func TestSupervisorDownBanner(t *testing.T) {
	m, s := newModel(t)
	now := time.Unix(1_000_000, 0)
	m.opts.Now = func() time.Time { return now }
	_ = s.SetKV("supervisor.heartbeat", "999999")
	m.Update(m.refreshCmd()())
	if strings.Contains(screen(m), "supervisor: down") {
		t.Fatal("fresh heartbeat must not banner")
	}
	hb := now.Add(-time.Minute)
	_ = s.SetKV("supervisor.heartbeat", itoa(hb.Unix()))
	m.Update(m.refreshCmd()())
	want := "supervisor: down since " + hb.Format("15:04")
	if !strings.Contains(screen(m), want) {
		t.Fatalf("want %q:\n%s", want, screen(m))
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestAgentRowHealthChip(t *testing.T) {
	m, _ := newModel(t)
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 32})
	m.health = healthState{byAgent: map[string]service.Health{
		"eng-1": {Liveness: "stale", Reason: "owes work"},
		"pm":    {Liveness: "rate_limited", Reason: "until 14:05"},
		"orch":  {Liveness: "idle"},
	}}
	m.rebuild()
	v := screen(m)
	for _, w := range []string{"stale", "⚠ stale: owes work", "rate-limited", "⚠ rate-limited"} {
		if !strings.Contains(v, w) {
			t.Fatalf("missing %q:\n%s", w, v)
		}
	}
}

func TestLogRendersEscalationKinds(t *testing.T) {
	for kind, want := range map[string]string{
		"rate_limited": "rate limited", "rate_limited_all": "all agents rate limited", "modal_stuck": "stuck on a prompt", "revive_gave_up": "revive gave up",
		"orch_down": "orchestrator down", "supervisor_down": "supervisor down", "unread_stuck": "messages unread",
	} {
		got := formatEvent(store.Event{Agent: "eng-1", Kind: kind, Data: `{"text":"boom"}`})
		if !strings.Contains(got, want) || !strings.Contains(got, "eng-1") || !strings.Contains(got, "boom") {
			t.Errorf("%s: %q", kind, got)
		}
	}
}

func TestSupervisorDownWhenLockNotHeld(t *testing.T) {
	m, s := newModel(t)
	m.opts.Dir = t.TempDir()
	now := time.Unix(1_000_000, 0)
	m.opts.Now = func() time.Time { return now }
	_ = s.SetKV("supervisor.heartbeat", "999999")
	_ = os.MkdirAll(filepath.Dir(supervisor.PidPath(m.opts.Dir)), 0o755)
	if err := os.WriteFile(supervisor.PidPath(m.opts.Dir), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.Update(m.refreshCmd()())
	if !strings.Contains(screen(m), "supervisor: down since") {
		t.Fatalf("stale pidfile with free lock should banner:\n%s", screen(m))
	}
}
