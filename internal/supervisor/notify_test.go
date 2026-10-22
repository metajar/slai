package supervisor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/config"
)

// notifyEnv: a driver env whose notify command appends "<SLAI_EVENT> <stdin>" to a file.
func notifyEnv(t *testing.T, events ...string) (*Supervisor, string) {
	s, _ := driverEnv(t)
	out := filepath.Join(t.TempDir(), "notified")
	s.Team.Notify = config.Notify{Command: `printf '%s %s\n' "$SLAI_EVENT" "$(cat)" >> ` + out, Events: events}
	s.notifyTick(context.Background()) // first run: start from now
	return s, out
}

func notified(t *testing.T, out string, want int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, _ := os.ReadFile(out)
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		if len(b) == 0 {
			lines = nil
		}
		if len(lines) >= want || time.Now().After(deadline) {
			time.Sleep(150 * time.Millisecond) // let a wrongly extra run show up
			b, _ = os.ReadFile(out)
			if len(b) == 0 {
				return nil
			}
			return strings.Split(strings.TrimSpace(string(b)), "\n")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestNotifyRunsCommandOncePerEvent(t *testing.T) {
	s, out := notifyEnv(t)
	ctx := context.Background()
	s.event("orch", "ask_user", map[string]any{"question": "ship it?"})
	s.event("eng-1", "request_review", map[string]any{"task_id": "T-1"})
	s.event("eng-1", "pretooluse", map[string]any{}) // not a notify event
	s.notifyTick(ctx)
	s.notifyTick(ctx) // a repeated tick must not re-run anything
	got := notified(t, out, 2)
	if len(got) != 2 {
		t.Fatalf("got %d runs, want 2: %q", len(got), got)
	}
	seen := map[string]bool{}
	for _, l := range got {
		ev, js, _ := strings.Cut(l, " ")
		var p struct {
			Event string
			Agent string
		}
		if err := json.Unmarshal([]byte(js), &p); err != nil || p.Event != ev {
			t.Fatalf("line %q: payload event %q err %v", l, p.Event, err)
		}
		seen[ev] = true
	}
	if !seen["awaiting_user"] || !seen["review_requested"] {
		t.Fatalf("events seen: %v", seen)
	}
}

func TestNotifyEscalationFiresOnce(t *testing.T) {
	s, out := notifyEnv(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		s.escalate(escalation{agent: "eng-1", kind: "rate_limited", text: "limit", user: true})
		s.notifyTick(ctx)
	}
	got := notified(t, out, 1)
	if len(got) != 1 || !strings.HasPrefix(got[0], "escalation ") {
		t.Fatalf("runs = %q, want exactly one escalation", got)
	}
}

func TestNotifyEventsFilterAndNoReplayOfHistory(t *testing.T) {
	s, _ := driverEnv(t)
	s.event("orch", "ask_user", map[string]any{"question": "old"}) // before notify was enabled
	out := filepath.Join(t.TempDir(), "n")
	s.Team.Notify = config.Notify{Command: `echo "$SLAI_EVENT" >> ` + out, Events: []string{"spend_cap"}}
	ctx := context.Background()
	s.notifyTick(ctx)
	s.event("", "ask_user", map[string]any{"question": "filtered out"})
	s.event("eng-1", "spend_cap", map[string]any{"scope": "daily"})
	s.event("eng-1", "ask_user", map[string]any{"reason": "spend_cap"}) // covered by spend_cap
	s.notifyTick(ctx)
	got := notified(t, out, 1)
	if len(got) != 1 || got[0] != "spend_cap" {
		t.Fatalf("runs = %q, want only spend_cap", got)
	}
}

func TestNotifyHangingCommandIsKilledAndDoesNotDelayTick(t *testing.T) {
	s, _ := driverEnv(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	s.Team.Notify = config.Notify{Command: `echo $$ > ` + pidFile + `; exec sleep 60`, TimeoutSeconds: 1}
	ctx := context.Background()
	s.notifyTick(ctx)
	s.event("", "spend_warn", map[string]any{})
	start := time.Now()
	s.notifyTick(ctx)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("tick took %s with a hanging command", d)
	}
	var pid int
	for i := 0; i < 100 && pid == 0; i++ {
		b, _ := os.ReadFile(pidFile)
		pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		time.Sleep(20 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("command never started")
	}
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("hanging command was not killed after the timeout")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestNotifyAbsentChangesNothing(t *testing.T) {
	s, _ := driverEnv(t)
	s.event("orch", "ask_user", map[string]any{})
	s.notifyTick(context.Background())
	if s.kvGet(kvNotifyCursor) != "" {
		t.Fatal("notify touched state without notify.command")
	}
}
