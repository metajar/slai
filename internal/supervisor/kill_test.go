package supervisor

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/store"
)

// causes returns the `cause` of every event of the restart kinds, by kind.
func causes(t *testing.T, s *Supervisor) map[string][]string {
	t.Helper()
	evs, err := s.Store.RecentEvents(2000)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, e := range evs {
		for _, k := range RestartKinds {
			if e.Kind == k {
				var d map[string]any
				_ = json.Unmarshal([]byte(e.Data), &d)
				c, _ := d["cause"].(string)
				out[k] = append(out[k], c)
			}
		}
	}
	return out
}

// Every way the supervisor kills or restarts a session, driven through the code that
// really does it: each records an event with a non-empty, human-readable cause.
func TestEveryRestartRecordsACause(t *testing.T) {
	seen := map[string]bool{}
	run := func(name string, want []string, f func(t *testing.T) *Supervisor) {
		t.Run(name, func(t *testing.T) {
			s := f(t)
			got := causes(t, s)
			for _, k := range want {
				cs := got[k]
				if len(cs) == 0 {
					t.Fatalf("no %s event recorded", k)
				}
				for _, c := range cs {
					if strings.TrimSpace(c) == "" || strings.Contains(c, "slai bug") || len(c) < 15 {
						t.Errorf("%s: cause %q is empty, unspecified or not human-readable", k, c)
					}
				}
				seen[k] = true
			}
			t.Logf("%v", got)
		})
	}
	ctx := context.Background()

	run("stuck turn: nudge did not help", []string{"stuck_restart", "auto_revive"}, func(t *testing.T) *Supervisor {
		s, now := stuckEnv(t)
		_, _ = s.Store.CreateTask(store.Task{Title: "x", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress})
		stuckTicks(s, now)
		restartTicks(s, now)
		s.driveTick(ctx) // revive
		c := causes(t, s)["stuck_restart"][0]
		if !strings.Contains(c, "no hook activity for") || !strings.Contains(c, "pane did not change") {
			t.Errorf("stuck cause = %q", c)
		}
		return s
	})
	run("dead MCP connection", []string{"stuck_restart"}, func(t *testing.T) *Supervisor {
		s, now := healthEnv(t)
		_, _ = s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "note", Body: "read me"})
		_ = s.Store.AddEvent("eng-1", "hook:userpromptsubmit", "{}")
		first := now.Add(-GiveUpAfter - time.Minute).Unix()
		msgs := mustUndelivered(t, s, "eng-1")
		s.saveWake("eng-1", wakeState{id: msgs[0].ID, n: MaxUnansweredWakes, firstAt: first, lastAt: first})
		_ = s.Store.SetAgentStatus("eng-1", "idle")
		s.wakeTick(ctx)
		if c := causes(t, s)["stuck_restart"]; len(c) != 1 || !strings.Contains(c[0], "MCP connection looks dead") {
			t.Errorf("mcp-dead cause = %v", c)
		}
		return s
	})
	run("MCP server crash", []string{"stuck_restart"}, func(t *testing.T) *Supervisor {
		s, now := healthEnv(t)
		_, _ = s.Store.CreateTask(store.Task{Title: "x", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress})
		_ = s.Store.SetAgentStatus("eng-1", "idle")
		_ = s.Store.SetKV("mcp.pending.eng-1", itoa64(now.Unix()))
		*now = now.Add(MCPStampWindow + time.Second)
		s.healthTick(ctx)
		if c := causes(t, s)["stuck_restart"]; len(c) != 1 || !strings.Contains(c[0], "never reached the service") {
			t.Errorf("mcp-crash cause = %v", c)
		}
		return s
	})
	run("mail left unread", []string{"stuck_restart"}, func(t *testing.T) *Supervisor {
		s, now := healthEnv(t)
		wokenMail(t, s, "eng-1")
		_ = s.Store.SetAgentStatus("eng-1", "idle")
		*now = now.Add(UnreadAfter + time.Second)
		s.healthTick(ctx)
		*now = now.Add(UnreadRestartAfter + time.Second)
		s.healthTick(ctx)
		if c := causes(t, s)["stuck_restart"]; len(c) != 1 || !strings.Contains(c[0], "unread for over") {
			t.Errorf("unread cause = %v", c)
		}
		return s
	})
	run("orchestrator down", []string{"orch_restart"}, func(t *testing.T) *Supervisor {
		s, now := healthEnv(t)
		_, _ = s.Store.CreateTask(store.Task{Title: "x", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress})
		_ = s.Store.SetAgentStatus("orch", "idle")
		wokenMail(t, s, "orch")
		task, _ := s.Store.ListTasks("")
		_ = s.Store.SetKV(kvBoardAt, signature(task)+"|1")
		*now = now.Add(OrchSilentAfter + time.Minute)
		s.healthTick(ctx)
		if c := causes(t, s)["orch_restart"]; len(c) != 1 || !strings.Contains(c[0], "orchestrator restarted: ") {
			t.Errorf("orch cause = %v", c)
		}
		return s
	})
	run("slai upgraded", []string{"agent_upgraded"}, func(t *testing.T) *Supervisor {
		s, _, _ := upgradeEnv(t)
		s.Build = "v2"
		s.recordBuild()
		staleAll(s)
		_ = s.Store.SetAgentStatus("eng-1", "idle")
		_ = s.Store.SetAgentStatus("eng-2", "working")
		_ = s.Store.SetAgentStatus("orch", "working")
		s.upgradeTick(ctx)
		if c := causes(t, s)["agent_upgraded"]; len(c) != 1 || !strings.Contains(c[0], "slai was upgraded") || !strings.Contains(c[0], "v2") {
			t.Errorf("upgrade cause = %v", c)
		}
		return s
	})
	run("team.yaml change and removal", []string{"agent_restarted", "agent_removed"}, func(t *testing.T) *Supervisor {
		s, _ := healthEnv(t)
		tm := cloneTeam(t, s.Team)
		tm.Agents[1].Model = "opus" // eng-1: launch config changed
		tm.Agents = tm.Agents[:2]   // eng-2: removed
		s.Team = tm
		if _, err := s.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		return s
	})
	run("session died", []string{"auto_revive"}, func(t *testing.T) *Supervisor {
		s, _ := healthEnv(t)
		_, _ = s.Store.CreateTask(store.Task{Title: "x", Assignee: "eng-2", CreatedBy: "orch", Status: store.StatusInProgress})
		_ = s.Tmux.KillSession(ctx, s.Session("eng-2"))
		_ = s.Store.SetAgentStatus("eng-2", "dead")
		s.driveTick(ctx)
		if c := causes(t, s)["auto_revive"]; len(c) != 1 || !strings.Contains(c[0], "its session died") || !strings.Contains(c[0], "1 task") {
			t.Errorf("revive cause = %v", c)
		}
		return s
	})
	run("stopped on request", []string{"agent_stopped"}, func(t *testing.T) *Supervisor {
		s, _ := healthEnv(t)
		if err := s.StopAgent(ctx, "eng-2"); err != nil {
			t.Fatal(err)
		}
		return s
	})

	// a kind added to RestartKinds without a scenario above fails here
	for _, k := range RestartKinds {
		if !seen[k] {
			t.Errorf("restart kind %q is not exercised by any scenario in TestEveryRestartRecordsACause", k)
		}
	}
}

func TestCauselessRestartsAreFlaggedNotSilent(t *testing.T) {
	s, _ := healthEnv(t)
	s.restartEvent("eng-1", "stuck_restart", "   ", nil)
	c := causes(t, s)["stuck_restart"]
	if len(c) != 1 || !strings.Contains(c[0], "slai bug") {
		t.Fatalf("a missing cause must be recorded as an obvious bug, got %v", c)
	}
}

// Sessions are only ever killed through killSession (every caller states a cause),
// apart from `slai down`'s teardown, which stops everything at the user's request.
func TestSessionsAreOnlyKilledThroughKillSession(t *testing.T) {
	fset := token.NewFileSet()
	files, _ := filepath.Glob("*.go")
	var offenders []string
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
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok {
					if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "KillSession" {
						name := fn.Name.Name
						if name != "killSession" && name != "Teardown" {
							offenders = append(offenders, f+":"+name)
						}
					}
				}
				return true
			})
		}
	}
	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Fatalf("KillSession called outside killSession/Teardown (add a cause and use s.killSession): %v", offenders)
	}
}
