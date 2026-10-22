package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math/rand"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/health"
	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
)

// boardOf is what get_board shows for every agent, through the real service.
func boardOf(t *testing.T, s *Supervisor, now time.Time) map[string][2]string {
	t.Helper()
	svc, err := service.Open(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Store.Close()
	svc.Now = func() time.Time { return now }
	raw, err := svc.GetBoard(context.Background(), "orch", service.StatusFilter{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(raw)
	var board struct {
		Agents []struct{ ID, Liveness, Reason string }
	}
	if err := json.Unmarshal(b, &board); err != nil {
		t.Fatal(err)
	}
	out := map[string][2]string{}
	for _, a := range board.Agents {
		out[a.ID] = [2]string{a.Liveness, a.Reason}
	}
	return out
}

// AC 3: for 200 random states the board and the supervisor classify every agent alike.
func TestBoardAndSupervisorAgreeOnRandomStates(t *testing.T) {
	rng := rand.New(rand.NewSource(20261021))
	statuses := []string{"idle", "working", "finished", "waiting", "waiting_on_user", "rate_limited", "dead"}
	taskStatuses := []string{store.StatusAssigned, store.StatusInProgress, store.StatusInReview, store.StatusChangesRequested, store.StatusMerged, store.StatusBlocked}
	var s *Supervisor
	var now time.Time
	counts := map[health.Liveness]int{}
	for i := 0; i < 200; i++ {
		if i%40 == 0 {
			s, _ = healthEnv(t)
			writeTeam(t, s, s.Team)
		}
		ids := []string{"orch", "eng-1", "eng-2"}
		id := ids[rng.Intn(len(ids))]
		_ = s.Store.SetAgentStatus(id, statuses[rng.Intn(len(statuses))])
		if rng.Intn(3) == 0 {
			_, _ = s.Store.CreateTask(store.Task{Title: "t", Assignee: ids[rng.Intn(len(ids))], CreatedBy: "orch", Status: taskStatuses[rng.Intn(len(taskStatuses))]})
		}
		if rng.Intn(4) == 0 {
			_, _ = s.Store.AddMessage(store.Message{From: "pm", To: id, Kind: "note", Body: "x"})
		}
		if rng.Intn(5) == 0 {
			_ = s.Store.SetAgentPaused(id, rng.Intn(2) == 0)
		}
		if rng.Intn(6) == 0 {
			_ = s.Store.SetKV(service.KVBackground+id, strconv.FormatInt(time.Now().Unix()-int64(rng.Intn(90)), 10)+"|1 monitor")
		}
		if rng.Intn(3) == 0 {
			_ = s.Store.SetKV("lastseen."+id, strconv.FormatInt(time.Now().Unix()-int64(rng.Intn(3600)), 10))
		}
		now = time.Now().Add(time.Duration(rng.Intn(45)) * time.Minute)
		s.Now = func() time.Time { return now }
		s.beginTick()
		board := boardOf(t, s, now)
		agents, _ := s.Store.GetAgents()
		for _, a := range agents {
			l, why := s.classify(a, true)
			counts[l]++
			if got := board[a.ID]; got != [2]string{string(l), why} {
				t.Fatalf("state %d, %s: board %v, supervisor %s %q", i, a.ID, got, l, why)
			}
			if !l.Valid() {
				t.Fatalf("%q is not a health.Liveness", l)
			}
		}
	}
	for _, l := range []health.Liveness{health.Dead, health.Blocked, health.Busy, health.Idle, health.Stale} {
		if counts[l] == 0 {
			t.Errorf("200 random states never produced %s: the property test is not exploring enough (%v)", l, counts)
		}
	}
}

// AC 1: the supervisor's liveness decisions go through the shared classifier, not
// through comparisons on the raw status.
func TestSupervisorBranchesOnTheSharedClassifier(t *testing.T) {
	branchFuncs := map[string]bool{"autoRevive": true, "allRateLimited": true, "rateLimitAgent": true, "modalAgent": true, "orchWatch": true, "clearResolved": true}
	fset := token.NewFileSet()
	files, _ := filepath.Glob("*.go")
	found := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range af.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || !branchFuncs[fn.Name.Name] {
				continue
			}
			found[fn.Name.Name] = true
			usesClassifier := false
			ast.Inspect(fn, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "livenessOf" {
						usesClassifier = true
					}
				case *ast.BinaryExpr:
					if x.Op != token.EQL && x.Op != token.NEQ {
						return true
					}
					for _, side := range []ast.Expr{x.X, x.Y} {
						sel, ok := side.(*ast.SelectorExpr)
						if !ok || sel.Sel.Name != "Status" {
							continue
						}
						other := x.Y
						if side == x.Y {
							other = x.X
						}
						if lit, ok := other.(*ast.BasicLit); ok && (lit.Value == `"dead"` || lit.Value == `"rate_limited"` || lit.Value == `"waiting"` || lit.Value == `"waiting_on_user"`) {
							t.Errorf("%s compares .Status with %s; ask s.livenessOf instead", fn.Name.Name, lit.Value)
						}
						if os, ok := other.(*ast.SelectorExpr); ok && strings.HasPrefix(os.Sel.Name, "Status") {
							t.Errorf("%s compares .Status with %s; ask s.livenessOf instead", fn.Name.Name, os.Sel.Name)
						}
					}
				}
				return true
			})
			if !usesClassifier {
				t.Errorf("%s makes a liveness decision without s.livenessOf", fn.Name.Name)
			}
		}
	}
	for name := range branchFuncs {
		if !found[name] {
			t.Errorf("branch function %s not found: update this test", name)
		}
	}
}

// The live gap: an agent whose turn ended while a Monitor runs is busy, not idle/stale.
func TestAgentWaitingOnAMonitorIsBusyNotIdle(t *testing.T) {
	s, now := healthEnv(t)
	ctx := context.Background()
	writeTeam(t, s, s.Team)
	_, _ = s.Store.CreateTask(store.Task{Title: "e2e", Assignee: "eng-2", CreatedBy: "orch", Status: store.StatusInProgress})
	_ = s.Store.SetAgentStatus("eng-2", "finished") // the Stop hook ran; the monitor is still going
	*now = now.Add(17 * time.Minute)
	board := func() [2]string { return boardOf(t, s, *now)["eng-2"] }
	if b := board(); b[0] != string(health.Stale) {
		t.Fatalf("control: with no monitor the agent should read stale after 17 min, got %v", b)
	}
	show(t, s, "eng-2", "  ⏸ manual mode on · 1 monitor · ← 11 agents · ↓ to manage")
	s.backgroundTick(ctx)
	if b := board(); b != [2]string{"busy", "waiting on background task (1 monitor)"} {
		t.Fatalf("with a monitor running: %v", b)
	}
	// it stops: the footer loses the count and the sighting is cleared on the next poll
	_ = s.Tmux.SendKeys(ctx, s.Session("eng-2"), "clear", false) // keep the pane live
	_ = s.Tmux.KillSession(ctx, s.Session("eng-2"))
	cfg, _ := s.agentCfg("eng-2")
	if _, err := s.StartAgent(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	_ = s.Store.SetAgentStatus("eng-2", "finished")
	*now = now.Add(BackgroundPoll + time.Second)
	s.backgroundTick(ctx)
	if v := s.kvGet(service.KVBackground + "eng-2"); v != "" {
		t.Fatalf("sighting not cleared once the footer lost it: %q", v)
	}
	// a stale sighting (supervisor stopped polling) expires by itself
	_ = s.Store.SetKV(service.KVBackground+"eng-2", fmt.Sprintf("%d|1 monitor", now.Add(-time.Hour).Unix()))
	if b := board(); b[0] == string(health.Busy) {
		t.Fatalf("an hour-old sighting still counted: %v", b)
	}
}

// The reminder ladder asks the classifier: an agent that owes work, whose turn ended
// (status finished), and that is waiting on a background shell/monitor is busy, not
// ignoring anyone - it gets no reminder, and a stalled escalation about it goes nowhere.
func TestReminderLadderSkipsAnAgentWaitingOnABackgroundTask(t *testing.T) {
	remind := func(t *testing.T, footer string) (reminders int) {
		s, now := driverEnv(t)
		ctx := context.Background()
		_, _ = s.Store.CreateTask(store.Task{Title: "e2e run", Assignee: "eng-2", CreatedBy: "orch", Status: store.StatusInProgress})
		_ = s.Store.SetAgentStatus("eng-2", "finished")
		if footer != "" {
			show(t, s, "eng-2", footer)
			s.backgroundTick(ctx)
		}
		for i := 0; i < 4; i++ { // far past NudgeIdleAfter and several NudgeEvery periods
			*now = now.Add(NudgeIdleAfter + NudgeEvery + time.Second)
			if footer != "" {
				s.rt.mu.Lock()
				s.rt.lastBG["eng-2"] = time.Time{} // poll again: the sighting stays fresh
				s.rt.mu.Unlock()
				s.backgroundTick(ctx)
			}
			s.driveTick(ctx)
			for _, m := range pendingFrom(t, s, "eng-2", "slai") {
				_ = s.Store.MarkDelivered(m.ID)
				reminders++
			}
			_ = s.Store.SetAgentStatus("eng-2", "finished")
		}
		return reminders
	}
	if n := remind(t, ""); n == 0 {
		t.Fatal("control: an idle agent with owed work should be reminded")
	}
	if n := remind(t, "  ⏸ manual mode on · 1 shell, 1 monitor · ← 11 agents · ↓ to manage"); n != 0 {
		t.Fatalf("reminded %d times an agent that is waiting on a background task", n)
	}
}
