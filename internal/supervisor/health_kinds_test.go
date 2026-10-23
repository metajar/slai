package supervisor

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/harness"
	"github.com/oorbt/slai/internal/hooks"
	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/tmux"
)

// Every escalation kind must behave the same way: it fires exactly once however
// many ticks follow, it closes (and so re-arms) when the agent makes progress, and
// it never fires for an agent that is paused, stopped on purpose or focused in the
// TUI. TestEscalationKindsTable runs those three checks over a table with one row
// per kind; TestEveryEscalationKindHasARow fails when a kind constant is added to
// the package without a row, so a new kind cannot skip them.

type kenv struct {
	t   *testing.T
	s   *Supervisor
	now *time.Time
	ctx context.Context
}

func (e *kenv) tick() {
	e.s.healthTick(e.ctx)
	e.s.driveTick(e.ctx)
}

func (e *kenv) task(assignee string) store.Task {
	tk, err := e.s.Store.CreateTask(store.Task{Title: "x", Assignee: assignee, CreatedBy: "orch", Status: store.StatusInProgress})
	if err != nil {
		e.t.Fatal(err)
	}
	return tk
}

// progressed records that id made a tool call and fired a hook after the escalation.
func (e *kenv) progressed(id string) {
	*e.now = e.now.Add(time.Second)
	_ = e.s.Store.AddEvent(id, "hook:pretooluse", "{}")
	_ = e.s.Store.SetKV(kvLastSeen+id, itoa64(e.now.Add(time.Minute).Unix()))
}

type kindRow struct {
	kind  string
	agent string // whose escalation it is ("team" for team-wide)
	env   func(t *testing.T) (*Supervisor, *time.Time)
	// arm sets up the condition without ticking; fire ticks until it escalates (called once).
	arm, fire func(e *kenv)
	// progress makes the cause go away or the agent move, then ticks.
	progress func(e *kenv)
	// holds lists the holds ("paused", "stopped", "focused") the kind must respect; noHold says why none apply.
	holds  []string
	noHold string
}

var allHolds = []string{"paused", "stopped", "focused"}

func kindRows() []kindRow {
	return []kindRow{
		{kind: EscRateLimited, agent: "eng-1", env: healthEnv, holds: allHolds,
			arm: func(e *kenv) {
				show(e.t, e.s, "eng-1", "Claude usage limit reached")
				_ = e.s.Store.SetAgentStatus("eng-1", "rate_limited")
			},
			fire: func(e *kenv) { e.s.healthTick(e.ctx) },
			progress: func(e *kenv) {
				_ = e.s.Store.SetAgentStatus("eng-1", "working")
				e.progressed("eng-1")
				e.s.healthTick(e.ctx)
			}},
		{kind: EscRateLimitedAll, agent: "team", env: healthEnv, noHold: "team-wide: no single agent to hold",
			arm: func(e *kenv) {
				for _, id := range []string{"orch", "eng-1", "eng-2"} {
					_ = e.s.Store.SetAgentStatus(id, "rate_limited")
				}
			},
			fire:     func(e *kenv) { e.s.healthTick(e.ctx) },
			progress: func(e *kenv) { _ = e.s.Store.SetAgentStatus("eng-1", "idle"); e.s.healthTick(e.ctx) }},
		{kind: EscModalStuck, agent: "eng-1", env: healthEnv, holds: allHolds,
			arm: func(e *kenv) {
				e.task("eng-1")
				show(e.t, e.s, "eng-1", "Do you want to proceed?", "❯ 1. Yes")
				_ = e.s.Store.SetAgentStatus("eng-1", "waiting")
			},
			fire: func(e *kenv) {
				*e.now = e.now.Add(ModalPatience + time.Second)
				e.s.healthTick(e.ctx)
			},
			progress: func(e *kenv) { _ = e.s.Store.SetAgentStatus("eng-1", "working"); e.s.healthTick(e.ctx) }},
		{kind: EscReviveGaveUp, agent: "eng-1", env: healthEnv, holds: []string{"stopped"},
			arm: func(e *kenv) {
				e.task("eng-1")
				_ = e.s.Tmux.KillSession(e.ctx, e.s.Session("eng-1"))
				_ = e.s.Store.SetAgentStatus("eng-1", "dead")
				var recent []string
				for i := 0; i < ReviveMax; i++ {
					recent = append(recent, itoa64(e.now.Unix()-int64(i)))
				}
				_ = e.s.Store.SetKV(kvRevive+"eng-1", strings.Join(recent, ","))
			},
			fire: func(e *kenv) { e.s.driveTick(e.ctx) },
			progress: func(e *kenv) {
				cfg, _ := e.s.agentCfg("eng-1")
				if _, err := e.s.StartAgent(e.ctx, cfg); err != nil {
					e.t.Fatal(err)
				}
				e.s.healthTick(e.ctx)
			}},
		{kind: EscOrchDown, agent: "orch", env: healthEnv, holds: allHolds,
			arm: func(e *kenv) {
				e.task("eng-1")
				_ = e.s.Tmux.KillSession(e.ctx, e.s.Session("orch"))
				_ = e.s.Store.SetAgentStatus("orch", "dead")
			},
			fire: func(e *kenv) {
				*e.now = e.now.Add(OrchSilentAfter + time.Second)
				e.s.healthTick(e.ctx)
			},
			progress: func(e *kenv) {
				e.s.driveTick(e.ctx) // the revive
				e.progressed("orch")
				e.s.healthTick(e.ctx)
			}},
		{kind: EscUnreadStuck, agent: "eng-1", env: healthEnv, holds: allHolds,
			arm: func(e *kenv) {
				wokenMail(e.t, e.s, "eng-1")
				_ = e.s.Store.SetAgentStatus("eng-1", "idle")
			},
			fire: func(e *kenv) {
				*e.now = e.now.Add(UnreadAfter + time.Second)
				e.s.healthTick(e.ctx)
				*e.now = e.now.Add(UnreadRestartAfter + time.Second)
				e.s.healthTick(e.ctx)
				e.s.healthTick(e.ctx)
			},
			progress: func(e *kenv) {
				if _, err := e.s.Store.ClaimInbox("eng-1"); err != nil {
					e.t.Fatal(err)
				}
				e.s.healthTick(e.ctx)
			}},
		{kind: EscAttachedBlocking, agent: "eng-1", env: healthEnv, holds: allHolds,
			arm: func(e *kenv) {
				_, _ = e.s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "note", Body: "please look"})
				_ = e.s.Store.SetAgentStatus("eng-1", "idle")
				standIn(e.t, attachedIdle(e.now, time.Hour))
			},
			fire: func(e *kenv) {
				*e.now = e.now.Add(AttachedBlockingAfter + time.Second)
				e.s.healthTick(e.ctx)
			},
			progress: func(e *kenv) {
				standIn(e.t, func() ([]tmux.ClientInfo, error) { return nil, nil }) // the client detached
				e.s.healthTick(e.ctx)
			}},
		{kind: EscStuckGaveUp, agent: "eng-1", env: healthEnv, noHold: "raised only by the stuck detector, whose hold handling stuck_test.go covers",
			arm: func(e *kenv) {},
			fire: func(e *kenv) {
				a := mustAgent(e.t, e.s, "eng-1")
				cfg, _ := e.s.agentCfg("eng-1")
				e.s.stuckGiveUp(a, cfg, 0, "kill failed")
			},
			progress: func(e *kenv) { e.progressed("eng-1"); e.s.healthTick(e.ctx) }},
		{kind: EscSwitchFailed, agent: "eng-1", env: healthEnv, holds: []string{"paused", "focused"},
			arm: func(e *kenv) {
				wt := e.s.taskWorktree(store.Agent{ID: "eng-1"})
				if err := os.Rename(wt, wt+".off"); err != nil {
					e.t.Fatal(err)
				}
				tk, _ := e.s.Store.CreateTask(store.Task{Title: "D", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusAssigned})
				_, _ = e.s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "assignment", TaskID: tk.ID, Body: "do D"})
				_ = e.s.Store.SetAgentStatus("eng-1", "idle")
				// the switch has been failing for longer than SwitchEscalateAfter
				_ = e.s.Store.SetKV(kvPrepFail+"eng-1", itoa64(e.now.Add(-SwitchEscalateAfter-time.Minute).Unix()))
			},
			fire: func(e *kenv) { e.s.wakeTick(e.ctx) },
			progress: func(e *kenv) {
				wt := e.s.taskWorktree(store.Agent{ID: "eng-1"})
				_ = os.Rename(wt+".off", wt)
				_ = e.s.Store.SetAgentStatus("eng-1", "idle")
				e.s.wakeTick(e.ctx)
			}},
		{kind: EscNoDecision, agent: "orch", env: healthEnv, holds: []string{"paused"},
			arm: func(e *kenv) {
				tk, _ := e.s.Store.CreateTask(store.Task{Title: "silent", Assignee: "eng-2", CreatedBy: "orch", Status: store.StatusInProgress})
				_ = e.s.Store.SetAgentStatus("eng-2", "idle")
				e.s.saveDecisions(map[string]pendingDecision{tk.ID: {Owner: "eng-2", Since: e.now.Unix(), After: e.s.lastEventID()}})
			},
			fire: func(e *kenv) {
				*e.now = e.now.Add(DecisionWithin + time.Second)
				tasks, _ := e.s.Store.ListTasks("")
				if !e.s.Paused() && !isPaused(e.s, "orch") {
					e.s.decisionTick(tasks)
				}
			},
			progress: func(e *kenv) {
				tasks, _ := e.s.Store.ListTasks("")
				for _, tk := range tasks {
					_ = e.s.Store.UpdateTaskStatus(tk.ID, store.StatusCancelled)
				}
				tasks, _ = e.s.Store.ListTasks("")
				e.s.decisionTick(tasks)
			}},
		{kind: EscWakeIgnored, agent: "orch", env: healthEnv, holds: []string{"paused"},
			arm: func(e *kenv) {
				id, _ := e.s.Store.AddMessage(store.Message{From: "slai", To: "orch", Kind: "note", Body: "decide"})
				first := e.now.Add(-GiveUpAfter - time.Minute).Unix()
				e.s.saveWake("orch", wakeState{id: id, n: MaxUnansweredWakes, firstAt: first, lastAt: e.now.Unix() - 1})
				_ = e.s.Store.SetAgentStatus("orch", "idle")
			},
			fire: func(e *kenv) { e.s.wakeTick(e.ctx) },
			progress: func(e *kenv) {
				e.progressed("orch") // a slai tool call after the last wake
				_ = e.s.Store.SetAgentStatus("orch", "idle")
				e.s.wakeTick(e.ctx)
			}},
		{kind: EscStalled, agent: "eng-1", env: healthEnv, noHold: "raised by the reminder ladder after its own nudges, which skip held agents",
			arm: func(e *kenv) { e.task("eng-1") },
			fire: func(e *kenv) {
				tasks, _ := e.s.Store.ListTasks("")
				cfg, _ := e.s.agentCfg("eng-1")
				e.s.escalateStalled(cfg, tasks)
			},
			progress: func(e *kenv) {
				tasks, _ := e.s.Store.ListTasks("")
				for _, tk := range tasks {
					if err := e.s.Store.UpdateTaskStatus(tk.ID, store.StatusCancelled); err != nil {
						e.t.Fatal(err)
					}
				}
				e.s.healthTick(e.ctx)
			}},
		{kind: EscHooksFailing, agent: "eng-1", env: healthEnv, holds: allHolds,
			arm: func(e *kenv) {
				_ = e.s.Store.SetAgentStatus("eng-1", "working")
				*e.now = e.now.Add(HooksSilentAfter + time.Minute)
				_ = hooks.RecordErr(e.s.Store, hooks.KVHookErr+"eng-1", "settings.json not readable", *e.now)
				showLines(e.t, e.s, "eng-1", "✻ Thinking… (12s · esc to interrupt)")
			},
			fire: func(e *kenv) {
				for i := 0; i <= hooksPaneChanges; i++ {
					e.s.healthTick(e.ctx)
					show(e.t, e.s, "eng-1", "output line "+strconv.Itoa(i))
				}
				e.s.healthTick(e.ctx)
				e.s.healthTick(e.ctx)
			},
			progress: func(e *kenv) {
				_ = e.s.Store.AddEvent("eng-1", "hook:pretooluse", "{}")
				*e.now = time.Now()
				e.s.healthTick(e.ctx)
			}},
		{kind: EscMCPCrash, agent: "eng-1", env: healthEnv, holds: allHolds,
			arm: func(e *kenv) {
				e.task("eng-1")
				_ = e.s.Store.SetAgentStatus("eng-1", "idle")
				_ = e.s.Store.SetKV(hooks.KVMCPPending+"eng-1", itoa64(e.now.Unix()))
			},
			fire: func(e *kenv) {
				*e.now = e.now.Add(MCPStampWindow + time.Second)
				e.s.healthTick(e.ctx) // first failure: restart
				cfg, _ := e.s.agentCfg("eng-1")
				if _, err := e.s.StartAgent(e.ctx, cfg); err != nil {
					e.t.Fatal(err)
				}
				_ = e.s.Store.SetAgentStatus("eng-1", "idle")
				*e.now = e.now.Add(5 * time.Minute)
				_ = e.s.Store.SetKV(hooks.KVMCPPending+"eng-1", itoa64(e.now.Unix()))
				*e.now = e.now.Add(MCPStampWindow + time.Second)
				e.s.healthTick(e.ctx) // second, inside the window: escalate
				e.s.healthTick(e.ctx)
			},
			progress: func(e *kenv) { e.progressed("eng-1"); e.s.healthTick(e.ctx) }},
		{kind: EscUpgradeFailed, agent: "team", env: upgradeEnvFor, noHold: "supervisor-level: no agent involved",
			arm: func(e *kenv) {
				e.s.Exec = func(string, []string, []string) error { return os.ErrPermission }
				e.s.recordBuild()
			},
			fire: func(e *kenv) {
				replaceExe(e.t, e.s.ExePath, "a different binary")
				e.s.reexecIfUpgraded()
				*e.now = e.now.Add(ExeSettle + time.Second)
				e.s.reexecIfUpgraded()
			},
			progress: func(e *kenv) { restarted(e.s, e.now).recordBuild() }}, // the next supervisor starts healthy
		{kind: EscAgentUpgradeFailed, agent: "eng-1", env: upgradeEnvFor, holds: []string{"paused", "stopped"},
			arm: func(e *kenv) {
				e.s.Build = "new"
				e.s.recordBuild()
				_ = e.s.Store.SetKV(service.KVMCPBuildPrefix+"eng-1", "old")
				_ = e.s.Store.SetKV(kvUpgradeTry+"eng-1", "new@"+itoa64(e.now.Add(-UpgradeComeUp-time.Minute).Unix()))
				_ = e.s.Store.SetAgentStatus("eng-1", "idle")
			},
			fire: func(e *kenv) { e.s.upgradeTick(e.ctx) },
			progress: func(e *kenv) {
				_ = e.s.Store.SetKV(service.KVMCPBuildPrefix+"eng-1", "new")
				e.s.healthTick(e.ctx)
			}},
		{kind: EscGuardMissing, agent: "eng-1", env: piEnv, holds: allHolds,
			arm: func(e *kenv) { _ = e.s.Store.SetKV(kvLaunchedAt+"eng-1", itoa64(e.now.Unix())) },
			fire: func(e *kenv) {
				*e.now = e.now.Add(GuardReadyWithin + time.Second)
				e.s.healthTick(e.ctx)
			},
			progress: func(e *kenv) {
				_ = os.WriteFile(harness.GuardReadyPath(e.s.Dir, "eng-1"), nil, 0o644)
				e.s.healthTick(e.ctx)
			}},
		{kind: EscTeamInvalid, agent: "team", env: hotEnv, noHold: "about the file, not an agent",
			arm: func(e *kenv) {},
			fire: func(e *kenv) {
				p := e.s.teamPath()
				_ = os.WriteFile(p, []byte("agents: [broken: \n"), 0o644)
				bump(e.t, p)
				e.s.teamTick(e.ctx)
			},
			progress: func(e *kenv) { writeTeam(e.t, e.s, e.s.Team); e.s.teamTick(e.ctx) }},
		{kind: EscTeamRemovalBlocked, agent: "eng-2", env: hotEnv, noHold: "the reload leaves a busy agent running by design",
			arm: func(e *kenv) {
				e.task("eng-2")
				_ = e.s.Store.SetAgentStatus("eng-2", "working")
			},
			fire: func(e *kenv) {
				tm := cloneTeam(e.t, e.s.Team)
				tm.Agents = tm.Agents[:2]
				writeTeam(e.t, e.s, tm)
				e.s.teamTick(e.ctx)
			},
			progress: func(e *kenv) {
				tasks, _ := e.s.Store.ListTasks("")
				for _, tk := range tasks {
					_ = e.s.Store.UpdateTaskStatus(tk.ID, store.StatusCancelled)
				}
				_ = e.s.Store.SetAgentStatus("eng-2", "idle")
				*e.now = e.now.Add(TeamRetry + time.Second)
				e.s.teamTick(e.ctx)
			}},
	}
}

// piEnv: the team of healthEnv with eng-1 configured as a pi agent (its session is
// the fake claude, which is all the guard check needs: a live session).
func piEnv(t *testing.T) (*Supervisor, *time.Time) {
	s, now := healthEnv(t)
	s.Team.Agents[1].Harness, s.Team.Agents[1].Model = "pi", "zai/glm-5.3"
	return s, now
}

func upgradeEnvFor(t *testing.T) (*Supervisor, *time.Time) {
	s, now, _ := upgradeEnv(t)
	return s, now
}

func openKind(s *Supervisor, kind string) []store.Escalation {
	es, _ := s.Store.OpenEscalations()
	var out []store.Escalation
	for _, e := range es {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func TestEscalationKindsTable(t *testing.T) {
	for _, row := range kindRows() {
		row := row
		t.Run(row.kind, func(t *testing.T) {
			if len(row.holds) == 0 && row.noHold == "" {
				t.Fatal("a row needs holds, or noHold saying why none apply")
			}
			newEnv := func() *kenv {
				s, now := row.env(t)
				return &kenv{t: t, s: s, now: now, ctx: context.Background()}
			}
			t.Run("fires once and clears on progress", func(t *testing.T) {
				e := newEnv()
				row.arm(e)
				row.fire(e)
				open := openKind(e.s, row.kind)
				if len(open) != 1 || open[0].Agent != row.agent {
					t.Fatalf("after firing: open %s escalations = %+v, want one for %s", row.kind, open, row.agent)
				}
				fired, msgs := countEvents(e.s, row.kind), len(slaiMsgs(t, e.s, "orch"))
				for i := 0; i < 3; i++ {
					*e.now = e.now.Add(time.Second)
					if row.kind == EscStuckGaveUp || row.kind == EscUpgradeFailed {
						row.fire(e) // the cause is re-observed on every pass: it must not re-fire
					} else {
						e.tick()
					}
				}
				if got := countEvents(e.s, row.kind); got != fired {
					t.Fatalf("%s fired again: %d events, was %d", row.kind, got, fired)
				}
				if got := len(slaiMsgs(t, e.s, "orch")); got != msgs {
					t.Fatalf("orchestrator told again: %d messages, was %d", got, msgs)
				}
				row.progress(e)
				if open := openKind(e.s, row.kind); len(open) != 0 {
					t.Fatalf("not cleared by progress: %+v", open)
				}
			})
			for _, hold := range row.holds {
				hold := hold
				t.Run("no-fire when "+hold, func(t *testing.T) {
					e := newEnv()
					row.arm(e)
					applyHold(e.s, hold, row.agent)
					before := countEvents(e.s, row.kind)
					func() {
						defer func() { // fire may legitimately find nothing to do
							if r := recover(); r != nil {
								t.Fatalf("panic: %v", r)
							}
						}()
						row.fire(e)
						applyHold(e.s, hold, row.agent)
						e.s.healthTick(e.ctx)
					}()
					if open := openKind(e.s, row.kind); len(open) != 0 || countEvents(e.s, row.kind) != before {
						t.Fatalf("%s fired for a %s agent: %+v", row.kind, hold, open)
					}
				})
			}
		})
	}
}

func applyHold(s *Supervisor, hold, id string) {
	switch hold {
	case "paused":
		_ = s.Store.SetAgentPaused(id, true)
	case "stopped":
		_ = s.Store.SetKV(kvStopped+id, "1")
	case "focused":
		_ = s.Store.TouchFocus(id, s.now().Add(48*time.Hour)) // still focused however far the fake clock moves
	}
}

// Kinds are the string values of the Esc* constants declared in this package.
func TestEveryEscalationKindHasARow(t *testing.T) {
	fset := token.NewFileSet()
	files, _ := filepath.Glob("*.go")
	declared := map[string]string{} // kind -> constant name
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range af.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, sp := range gd.Specs {
				vs := sp.(*ast.ValueSpec)
				for i, n := range vs.Names {
					if !strings.HasPrefix(n.Name, "Esc") || i >= len(vs.Values) {
						continue
					}
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						v, _ := strconv.Unquote(lit.Value)
						declared[v] = n.Name
					}
				}
			}
		}
	}
	if len(declared) < 10 {
		t.Fatalf("found only %d Esc* constants: the scan is broken", len(declared))
	}
	rows := map[string]bool{}
	for _, r := range kindRows() {
		if rows[r.kind] {
			t.Errorf("duplicate row for %s", r.kind)
		}
		rows[r.kind] = true
	}
	var missing []string
	for kind, name := range declared {
		if !rows[kind] {
			missing = append(missing, name+" ("+kind+")")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("escalation kinds without a row in kindRows (health_kinds_test.go): %s", strings.Join(missing, ", "))
	}
	for kind := range rows {
		if _, ok := declared[kind]; !ok {
			t.Errorf("row for %q matches no Esc* constant", kind)
		}
	}
}
