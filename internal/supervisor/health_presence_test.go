package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/tmux"
)

// standIn makes listClients report the given clients for every session until the test ends.
func standIn(t *testing.T, cs func() ([]tmux.ClientInfo, error)) {
	t.Helper()
	old := listClients
	listClients = func(context.Context, *tmux.Client, string) ([]tmux.ClientInfo, error) { return cs() }
	t.Cleanup(func() { listClients = old })
}

func attachedIdle(now *time.Time, idle time.Duration) func() ([]tmux.ClientInfo, error) {
	return func() ([]tmux.ClientInfo, error) {
		return []tmux.ClientInfo{{Name: "/dev/pts/9", Activity: now.Add(-idle)}}, nil
	}
}

func TestPresenceIdleAttachedClientWithOldMailRaisesAttachedBlocking(t *testing.T) {
	s, now := healthEnv(t)
	ctx := context.Background()
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	_, _ = s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "note", Body: "please look"})
	standIn(t, attachedIdle(now, 10*time.Minute))
	before := pane(t, s, "eng-1")

	s.healthTick(ctx) // mail is fresh: nothing yet
	if _, ok := openEsc(t, s, EscAttachedBlocking, "eng-1"); ok {
		t.Fatal("raised for mail that had only just arrived")
	}
	*now = now.Add(AttachedBlockingAfter + time.Second)
	s.healthTick(ctx)
	s.healthTick(ctx)
	e, ok := openEsc(t, s, EscAttachedBlocking, "eng-1")
	if !ok || !e.User || !strings.Contains(e.Text, "eng-1") || !strings.Contains(e.Text, "1 message") || !strings.Contains(e.Text, "Detach it") {
		t.Fatalf("attached_blocking = %+v %v", e, ok)
	}
	if countEvents(s, EscAttachedBlocking) != 1 {
		t.Fatalf("escalated %d times, want once", countEvents(s, EscAttachedBlocking))
	}
	time.Sleep(200 * time.Millisecond)
	if got := pane(t, s, "eng-1"); got != before || hasEvent(s, "unread_nudge") {
		t.Fatalf("slai typed into an attached agent's pane:\n%s", got)
	}

	// the client detaches: the escalation closes, and the wake loop is free to deliver
	standIn(t, func() ([]tmux.ClientInfo, error) { return nil, nil })
	s.healthTick(ctx)
	if _, ok := openEsc(t, s, EscAttachedBlocking, "eng-1"); ok {
		t.Fatal("still open after the client detached")
	}
}

func TestPresenceActiveClientIsNeverTypedOverNorEscalated(t *testing.T) {
	s, now := healthEnv(t)
	ctx := context.Background()
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	_, _ = s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "note", Body: "please look"})
	standIn(t, attachedIdle(now, 20*time.Second)) // typed 20 s ago
	before := pane(t, s, "eng-1")
	*now = now.Add(UnreadAfter + time.Minute)
	for i := 0; i < 3; i++ {
		s.healthTick(ctx)
	}
	if _, ok := openEsc(t, s, EscAttachedBlocking, "eng-1"); ok {
		t.Fatal("escalated while the user is actively typing")
	}
	time.Sleep(200 * time.Millisecond)
	if pane(t, s, "eng-1") != before || hasEvent(s, "unread_nudge") {
		t.Fatal("typed over an active user")
	}
	// a focused TUI is active too, however long the mail has waited
	standIn(t, func() ([]tmux.ClientInfo, error) { return nil, nil })
	_ = s.Store.TouchFocus("eng-1", *now)
	s.healthTick(ctx)
	if _, ok := openEsc(t, s, EscAttachedBlocking, "eng-1"); ok {
		t.Fatal("escalated for a focused TUI")
	}
}

func TestPresenceListClientsErrorDoesNotCountAsAttached(t *testing.T) {
	s, _ := healthEnv(t)
	ctx := context.Background()
	a, _ := s.Store.AgentByID("eng-1")
	standIn(t, func() ([]tmux.ClientInfo, error) { return nil, errors.New("tmux: server busy") })
	if s.userPresent(ctx, a) {
		t.Fatal("a failing list-clients silenced the agent")
	}
	if s.holdOff(ctx, a, map[string]bool{}) {
		t.Fatal("holdOff held an agent nobody is attached to")
	}
	standIn(t, func() ([]tmux.ClientInfo, error) { return []tmux.ClientInfo{{Name: "c", Activity: time.Now()}}, nil })
	if !s.userPresent(ctx, a) || !s.holdOff(ctx, a, map[string]bool{}) {
		t.Fatal("an attached client was not seen")
	}
}

// Mail that waited because a user was present is overdue once the user is gone, even though
// no wake line was ever typed for it.
func TestUnreadCountsMailSkippedForPresence(t *testing.T) {
	s, now := healthEnv(t)
	ctx := context.Background()
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	_, _ = s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "note", Body: "please look"})
	if n := len(s.oldUnread("eng-1")); n != 0 {
		t.Fatalf("never-woken mail counted without a presence skip: %d", n)
	}
	standIn(t, attachedIdle(now, time.Second))
	s.healthTick(ctx) // presence skip recorded
	*now = now.Add(UnreadAfter + time.Second)
	if n := len(s.oldUnread("eng-1")); n != 1 {
		t.Fatalf("mail skipped for presence not counted: %d", n)
	}
	if err := s.Store.DeleteKV(kvPresenceSkip + "eng-1"); err != nil {
		t.Fatal(err)
	}
	if n := len(s.oldUnread("eng-1")); n != 0 {
		t.Fatalf("mail not skipped for presence counted: %d", n)
	}
}

// silentHooks drives an agent that works for a long time with no hooks. footer, if set, is
// what Claude Code draws while a background shell runs, kept as the pane's last line.
func silentHooks(t *testing.T, footer string) (*Supervisor, bool) {
	t.Helper()
	s, now := healthEnv(t)
	ctx := context.Background()
	_ = s.Store.SetAgentStatus("eng-1", "working")
	*now = now.Add(HooksSilentAfter + time.Minute)
	showLines(t, s, "eng-1", "✻ Thinking… (12s · esc to interrupt)")
	for i := 0; i <= hooksPaneChanges+1; i++ {
		s.healthTick(ctx)
		show(t, s, "eng-1", "output line "+strconv.Itoa(i))
		if footer != "" {
			show(t, s, "eng-1", footer)
		}
	}
	s.healthTick(ctx)
	s.healthTick(ctx)
	_, ok := openEsc(t, s, EscHooksFailing, "eng-1")
	return s, ok
}

func TestHooksSilenceWhileWaitingOnBackgroundShellIsNotAFailure(t *testing.T) {
	if _, ok := silentHooks(t, ""); !ok {
		t.Fatal("control: a working pane with no hooks should raise hooks_failing")
	}
	if _, ok := silentHooks(t, "⏵⏵ auto mode on (shift+tab to cycle) · 1 shell · ← 11 agents"); ok {
		t.Fatal("an agent waiting on a background shell was reported as having no hooks")
	}
}

func TestHooksAlarmNamesHookLogOnlyWhenItExists(t *testing.T) {
	s, ok := silentHooks(t, "")
	if !ok {
		t.Fatal("no hooks_failing")
	}
	if body := slaiMsgs(t, s, "orch")[0].Body; strings.Contains(body, "hook.log") || !strings.Contains(body, "slai doctor") {
		t.Fatalf("alarm names a log that does not exist: %s", body)
	}
	s2, now := healthEnv(t)
	_ = now
	dir := filepath.Join(s2.Dir, ".slai", "run", "eng-1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hook.log"), []byte("hook error\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = s2.Store.SetAgentStatus("eng-1", "working")
	*now = now.Add(HooksSilentAfter + time.Minute)
	showLines(t, s2, "eng-1", "✻ Thinking… (12s · esc to interrupt)")
	for i := 0; i <= hooksPaneChanges+1; i++ {
		s2.healthTick(context.Background())
		show(t, s2, "eng-1", "output line "+strconv.Itoa(i))
	}
	s2.healthTick(context.Background())
	s2.healthTick(context.Background())
	if ms := slaiMsgs(t, s2, "orch"); len(ms) != 1 || !strings.Contains(ms[0].Body, ".slai/run/eng-1/hook.log") {
		t.Fatalf("alarm should name the existing hook.log: %+v", ms)
	}
}
