package supervisor

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/harness"
)

func guardEnv(t *testing.T, kind, id string) (*Supervisor, *time.Time) {
	t.Helper()
	s, now := healthEnv(t)
	for i := range s.Team.Agents {
		if s.Team.Agents[i].ID == id {
			s.Team.Agents[i].Harness, s.Team.Agents[i].Model = kind, "zai/glm-5.3"
		}
	}
	_ = s.Store.SetKV(kvLaunchedAt+id, itoa64(now.Unix()))
	return s, now
}

func isPaused(s *Supervisor, id string) bool {
	p, _ := s.Store.PausedAgents()
	return p[id]
}

// Decision 8c: a pi agent that can write and has no registered guard after 30 s is
// held and the user is told; it is never left running unguarded.
func TestPiWithoutGuardIsHeldAndEscalatedOnce(t *testing.T) {
	s, now := guardEnv(t, "pi", "eng-1")
	ctx := context.Background()
	*now = now.Add(GuardReadyWithin - time.Second)
	s.healthTick(ctx)
	if isPaused(s, "eng-1") || hasEvent(s, EscGuardMissing) {
		t.Fatal("judged before GuardReadyWithin")
	}
	*now = now.Add(2 * time.Second)
	for i := 0; i < 3; i++ {
		s.healthTick(ctx)
	}
	e, ok := openEsc(t, s, EscGuardMissing, "eng-1")
	if !ok || !e.User || !strings.Contains(e.Text, "held") || countEvents(s, EscGuardMissing) != 1 {
		t.Fatalf("escalation = %+v %v", e, ok)
	}
	if !isPaused(s, "eng-1") {
		t.Fatal("the pi agent was left running without a guard")
	}
	if got := len(slaiMsgs(t, s, "orch")); got != 1 {
		t.Fatalf("orch notes = %d", got)
	}
	// the guard registers late: the hold lifts by itself and the escalation closes
	if err := os.WriteFile(harness.GuardReadyPath(s.Dir, "eng-1"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s.healthTick(ctx)
	if isPaused(s, "eng-1") {
		t.Fatal("hold not lifted once the guard registered")
	}
	if _, ok := openEsc(t, s, EscGuardMissing, "eng-1"); ok {
		t.Fatal("escalation not closed")
	}
}

// A hold the user placed is not ours to lift.
func TestGuardRecoveryDoesNotUnpauseAUsersPause(t *testing.T) {
	s, now := guardEnv(t, "pi", "eng-1")
	_ = s.Store.SetAgentPaused("eng-1", true) // the user paused it
	*now = now.Add(GuardReadyWithin + time.Minute)
	s.healthTick(context.Background())
	if _, ok := openEsc(t, s, EscGuardMissing, "eng-1"); ok {
		t.Fatal("acted on a user-paused agent")
	}
	_ = os.WriteFile(harness.GuardReadyPath(s.Dir, "eng-1"), nil, 0o644)
	s.healthTick(context.Background())
	if !isPaused(s, "eng-1") {
		t.Fatal("unpaused an agent the user paused")
	}
}

func TestGuardReadyEventCountsOnlyForTheCurrentLaunch(t *testing.T) {
	s, now := guardEnv(t, "opencode", "eng-1")
	ctx := context.Background()
	// an event from before the launch does not count
	_ = s.Store.AddEvent("eng-1", "hook:guard_ready", "{}")
	_ = s.Store.SetKV(kvLaunchedAt+"eng-1", itoa64(now.Unix()+5))
	*now = now.Add(GuardReadyWithin + 10*time.Second)
	a := mustAgent(t, s, "eng-1")
	if s.guardReady(a) {
		t.Fatal("a guard_ready from an earlier launch was accepted")
	}
	// opencode is the softer case: the orchestrator is told, nothing is held
	s.healthTick(ctx)
	e, ok := openEsc(t, s, EscGuardMissing, "eng-1")
	if !ok || e.User || isPaused(s, "eng-1") {
		t.Fatalf("opencode: esc=%+v ok=%v paused=%v (want a quiet warning, no hold)", e, ok, isPaused(s, "eng-1"))
	}
	// this launch's event closes it
	_ = s.Store.SetKV(kvLaunchedAt+"eng-1", itoa64(time.Now().Unix()-1)) // relaunched a moment ago
	_ = s.Store.AddEvent("eng-1", "hook:guard_ready", "{}")
	s.healthTick(ctx)
	if _, ok := openEsc(t, s, EscGuardMissing, "eng-1"); ok {
		t.Fatal("not closed by a guard_ready event")
	}
}

func TestGuardCheckSkipsClaudeAndReadOnlyRoles(t *testing.T) {
	s, now := guardEnv(t, "pi", "orch") // the orchestrator cannot write: no mandatory guard
	_ = s.Store.SetKV(kvLaunchedAt+"eng-2", itoa64(now.Unix()))
	*now = now.Add(time.Hour)
	for i := 0; i < 2; i++ {
		s.healthTick(context.Background())
	}
	if es, _ := s.Store.OpenEscalations(); len(es) != 0 {
		t.Fatalf("escalated for a claude agent or a read-only role: %+v", es)
	}
	if isPaused(s, "orch") || isPaused(s, "eng-2") {
		t.Fatal("held an agent that needs no guard")
	}
}

// A new launch must register again: StartAgent clears the marker and stamps the time.
func TestStartAgentResetsGuardRegistration(t *testing.T) {
	s, _ := healthEnv(t)
	ctx := context.Background()
	if err := os.WriteFile(harness.GuardReadyPath(s.Dir, "eng-1"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_ = s.Tmux.KillSession(ctx, s.Session("eng-1"))
	cfg, _ := s.agentCfg("eng-1")
	if _, err := s.StartAgent(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(harness.GuardReadyPath(s.Dir, "eng-1")); err == nil {
		t.Fatal("a guard_ready marker survived into the next launch")
	}
	if s.kvGet(kvLaunchedAt+"eng-1") == "" {
		t.Fatal("launch time not recorded")
	}
}
