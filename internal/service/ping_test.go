package service

import (
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/store"
)

// answerPing plays the target agent: when a ping message for `to` appears it calls
// pong as `via` (the identity the agent's MCP server runs as) reporting `reported`.
func answerPing(t *testing.T, s *Service, to, via, reported string) {
	t.Helper()
	go func() {
		for i := 0; i < 400; i++ {
			msgs, _ := s.Store.UndeliveredFor(to)
			for _, m := range msgs {
				if _, answered, _ := s.Store.GetKV(pongKey(m.ID)); m.Kind == "ping" && !answered {
					_, _ = s.Pong(ctx, via, PongArgs{MessageID: m.ID, AgentID: reported})
					return
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
}

func TestPingOutcomes(t *testing.T) {
	old := pingPoll
	pingPoll = 10 * time.Millisecond
	t.Cleanup(func() { pingPoll = old })
	s := newSvc(t, true)

	answerPing(t, s, "eng-1", "eng-1", "eng-1")
	r := j(must(s.Ping(ctx, "orch", PingArgs{Agent: "eng-1", TimeoutSeconds: 5})))
	if r["result"] != "alive" || r["reported_id"] != "eng-1" {
		t.Fatalf("healthy ping: %v", r)
	}
	if r["last_seen"].(float64) == 0 {
		t.Fatalf("last_seen not set after pong: %v", r)
	}

	// the shell reports a different id than assigned
	answerPing(t, s, "eng-1", "eng-1", "orch")
	r = j(must(s.Ping(ctx, "orch", PingArgs{Agent: "eng-1", TimeoutSeconds: 5})))
	if r["result"] != "identity_mismatch" || r["reported_id"] != "orch" || r["assigned_id"] != "eng-1" {
		t.Fatalf("reported mismatch: %v", r)
	}

	// the MCP server answers as another agent (flipped SLAI_AGENT_ID)
	answerPing(t, s, "eng-1", "pm", "eng-1")
	r = j(must(s.Ping(ctx, "orch", PingArgs{Agent: "eng-1", TimeoutSeconds: 5})))
	if r["result"] != "identity_mismatch" || r["answered_by"] != "pm" || r["assigned_id"] != "eng-1" {
		t.Fatalf("answered-by mismatch: %v", r)
	}

	// unresponsive: bounded wait, distinct result
	start := time.Now()
	r = j(must(s.Ping(ctx, "orch", PingArgs{Agent: "pm", TimeoutSeconds: 1})))
	if r["result"] != "no_response" || time.Since(start) > 3*time.Second {
		t.Fatalf("no response: %v after %v", r, time.Since(start))
	}

	_, err := s.Ping(ctx, "orch", PingArgs{Agent: "nobody"})
	wantErr(t, err, "unknown agent")
	_, err = s.Ping(ctx, "orch", PingArgs{Agent: "orch"})
	wantErr(t, err, "unknown agent")
	_, err = s.Ping(ctx, "eng-1", PingArgs{Agent: "pm"})
	wantErr(t, err, "not allowed")
	_, err = s.Pong(ctx, "eng-1", PongArgs{MessageID: 9999, AgentID: "eng-1"})
	wantErr(t, err, "not a ping")
}

func boardLiveness(t *testing.T, s *Service) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, a := range j(must(s.GetBoard(ctx, "orch", StatusFilter{})))["agents"].([]any) {
		m := a.(map[string]any)
		out[m["id"].(string)] = m["liveness"].(string)
	}
	return out
}

func TestLastSeenAndBoardLiveness(t *testing.T) {
	old := pingPoll
	pingPoll = 10 * time.Millisecond
	t.Cleanup(func() { pingPoll = old })
	s := newSvc(t, true)

	// the orchestrator just called get_board; nobody else has made a call yet
	if l := boardLiveness(t, s); l["orch"] != "idle" || l["eng-1"] != "idle" {
		t.Fatalf("initial liveness: %v", l)
	}
	if s.lastSeen("eng-1") != 0 {
		t.Fatal("eng-1 has a last_seen before any call")
	}
	// an ordinary tool call stamps last_seen
	must(s.GetInbox(ctx, "eng-1"))
	if s.lastSeen("eng-1") == 0 {
		t.Fatal("last_seen not updated by get_inbox")
	}
	if l := boardLiveness(t, s); l["eng-1"] != "idle" {
		t.Fatalf("after a call: %v", l)
	}
	// seen long ago but owing nothing: still idle; with unread mail it is stale (stale needs silence: see TestAgentHealth)
	_ = s.Store.SetKV(lastSeenKey("eng-1"), "1")
	if l := boardLiveness(t, s); l["eng-1"] != "idle" {
		t.Fatalf("old last_seen, nothing owed: %v", l)
	}
	if _, err := s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "note", Body: "x"}); err != nil {
		t.Fatal(err)
	}
	_ = s.Store.SetKV(lastSeenKey("eng-1"), "1")
	if l := boardLiveness(t, s); l["eng-1"] != "alive" {
		t.Fatalf("old last_seen: %v", l)
	}
	// a failed ping with no call since: dead; a later call revives it
	must(s.Ping(ctx, "orch", PingArgs{Agent: "eng-1", TimeoutSeconds: 1}))
	if l := boardLiveness(t, s); l["eng-1"] != "dead" {
		t.Fatalf("after failed ping: %v", l)
	}
	// the same failed ping on a working agent is busy (it was mid-turn), not dead
	_ = s.Store.SetAgentStatus("eng-1", "working")
	if l := boardLiveness(t, s); l["eng-1"] != "busy" {
		t.Fatalf("failed ping while working: %v", l)
	}
	// anything newer than the ping (here a status change) overrides it
	time.Sleep(1100 * time.Millisecond)
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	if l := boardLiveness(t, s); l["eng-1"] == "dead" {
		t.Fatalf("status change after the failed ping should not read dead: %v", l)
	}
	// a failed ping followed by a hook event on a live session is never dead
	must(s.Ping(ctx, "orch", PingArgs{Agent: "eng-1", TimeoutSeconds: 1}))
	if l := boardLiveness(t, s); l["eng-1"] != "dead" {
		t.Fatalf("second failed ping: %v", l)
	}
	time.Sleep(1100 * time.Millisecond)
	_ = s.Store.AddEvent("eng-1", "hook:stop", "{}")
	if l := boardLiveness(t, s); l["eng-1"] == "dead" {
		t.Fatalf("hook event after the failed ping: %v", l)
	}
	time.Sleep(1100 * time.Millisecond) // last_seen has 1s resolution
	must(s.GetInbox(ctx, "eng-1"))
	if l := boardLiveness(t, s); l["eng-1"] == "dead" {
		t.Fatalf("after a call following the failed ping: %v", l)
	}
}

func TestBoardExposesStatusAndSupervisor(t *testing.T) {
	s := newSvc(t, true)
	_ = s.Store.SetAgentStatus("eng-1", "working")
	tk, _ := s.Store.CreateTask(store.Task{Title: "w", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress})
	b := j(must(s.GetBoard(ctx, "orch", StatusFilter{})))
	var eng map[string]any
	for _, a := range b["agents"].([]any) {
		if m := a.(map[string]any); m["id"] == "eng-1" {
			eng = m
		}
	}
	if eng["status"] != "working" || eng["current_task"] != tk.ID || eng["last_activity"] == nil {
		t.Fatalf("agent view = %v", eng)
	}
	if sv := b["supervisor"].(map[string]any); sv["running"] != false {
		t.Fatalf("no heartbeat yet, supervisor = %v", sv)
	}
	_ = s.Store.SetSupervisorHeartbeat(time.Now().Unix())
	if sv := j(must(s.GetBoard(ctx, "orch", StatusFilter{})))["supervisor"].(map[string]any); sv["running"] != true {
		t.Fatalf("fresh heartbeat, supervisor = %v", sv)
	}
	_ = s.Store.SetSupervisorHeartbeat(time.Now().Unix() - 60)
	sv := j(must(s.GetBoard(ctx, "orch", StatusFilter{})))["supervisor"].(map[string]any)
	if sv["running"] != false || sv["heartbeat_age_s"].(float64) < 59 {
		t.Fatalf("old heartbeat, supervisor = %v", sv)
	}
}

func TestPingDefaultTimeoutAndBusyWhenHooksRecent(t *testing.T) {
	if PingDefaultTimeout != 60*time.Second {
		t.Fatalf("default ping timeout = %s", PingDefaultTimeout)
	}
	s := newSvc(t, true)
	time.Sleep(1100 * time.Millisecond)
	must(s.Ping(ctx, "orch", PingArgs{Agent: "eng-1", TimeoutSeconds: 1})) // unanswered
	if l := boardLiveness(t, s); l["eng-1"] != "dead" {
		t.Fatalf("silent agent with no hooks after a failed ping: %v", l)
	}
	// hooks fired shortly before the ping: it is mid-session, so busy, not dead
	s2 := newSvc(t, true)
	_ = s2.Store.AddEvent("eng-1", "hook:pretooluse", "{}")
	time.Sleep(1100 * time.Millisecond)
	must(s2.Ping(ctx, "orch", PingArgs{Agent: "eng-1", TimeoutSeconds: 1}))
	if l := boardLiveness(t, s2); l["eng-1"] != "busy" {
		t.Fatalf("unanswered ping with recent hooks: %v", l)
	}
}

func TestBoardCarriesBuildAndStaleBuild(t *testing.T) {
	s := newSvc(t, true)
	b := j(must(s.GetBoard(ctx, "orch", StatusFilter{})))
	if b["build"] == "" || b["build"] == nil {
		t.Fatalf("board has no build: %v", b)
	}
	_ = s.Store.SetKV(KVSupervisorBuild, "new")
	_ = s.Store.SetKV(KVMCPBuildPrefix+"eng-1", "old")
	_ = s.Store.SetKV(KVMCPBuildPrefix+"orch", "new")
	b = j(must(s.GetBoard(ctx, "orch", StatusFilter{})))
	if b["build"] != "new" {
		t.Fatalf("build = %v, want the supervisor's", b["build"])
	}
	stale := map[string]bool{}
	for _, a := range b["agents"].([]any) {
		m := a.(map[string]any)
		stale[m["id"].(string)], _ = m["stale_build"].(bool)
	}
	if !stale["eng-1"] || stale["orch"] {
		t.Fatalf("stale_build = %v", stale)
	}
}

func TestBoardCarriesHarnessAndDegraded(t *testing.T) {
	s := newSvc(t, true)
	s.Team.Agents = append(s.Team.Agents,
		config.Agent{ID: "oc-1", Role: config.RoleEngineer, Model: "zai/glm-5.3", Harness: "opencode"},
		config.Agent{ID: "pi-1", Role: config.RoleEngineer, Model: "zai/glm-5.3", Harness: "pi"})
	b := j(must(s.GetBoard(ctx, "orch", StatusFilter{})))
	got := map[string]map[string]any{}
	for _, a := range b["agents"].([]any) {
		m := a.(map[string]any)
		got[m["id"].(string)] = m
	}
	if m := got["eng-1"]; m["harness"] != "claude" || m["degraded"] != nil || m["gauges"] != "ok" || m["gauges_reason"] != nil {
		t.Fatalf("claude entry = %v", m)
	}
	m := got["oc-1"]
	deg, _ := m["degraded"].([]any)
	if m["harness"] != "opencode" || len(deg) != 2 || deg[0] != "statusline" || deg[1] != "resume_by_id" ||
		m["gauges"] != "n/a" || !strings.Contains(m["gauges_reason"].(string), "not supported by opencode") {
		t.Fatalf("opencode entry = %v", m)
	}
	if m := got["pi-1"]; m["harness"] != "pi" || m["gauges"] != "n/a" {
		t.Fatalf("pi entry = %v", m)
	}
}

// The board shows the task the agent is on, not its latest assignment.
func TestBoardCurrentTaskIsTheWorkingTask(t *testing.T) {
	s := newSvc(t, true)
	cur, _ := s.Store.CreateTask(store.Task{Title: "doing", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress})
	_, _ = s.Store.CreateTask(store.Task{Title: "stuck", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusBlocked})
	_ = s.Store.SetAgentTask("eng-1", "T-0002") // the stale "latest assignment"
	get := func() string {
		for _, a := range j(must(s.GetBoard(ctx, "orch", StatusFilter{})))["agents"].([]any) {
			if m := a.(map[string]any); m["id"] == "eng-1" {
				v, _ := m["current_task"].(string)
				return v
			}
		}
		return "?"
	}
	if got := get(); got != cur.ID {
		t.Fatalf("current_task = %q, want %s", got, cur.ID)
	}
	_ = s.Store.UpdateTaskStatus(cur.ID, store.StatusInReview)
	_ = s.Store.UpdateTaskStatus(cur.ID, store.StatusMerged)
	if got := get(); got != "" {
		t.Fatalf("nothing qualifies, current_task = %q", got)
	}
}

// get_board reads the wake-gate inputs once per call, however many agents it lists.
func TestBoardReadsGatesOncePerCall(t *testing.T) {
	s := newSvc(t, true)
	s.Team.Defaults.MaxConcurrentAgents = 2
	s.Team.Defaults.Caps.DailyUSD = 10
	s.Team.Defaults.Caps.PerAgentUSD = 5
	for _, extra := range []string{"eng-2", "eng-3", "eng-4"} {
		s.Team.Agents = append(s.Team.Agents, config.Agent{ID: extra, Role: config.RoleEngineer})
		_ = s.Store.UpsertAgent(store.Agent{ID: extra, Role: config.RoleEngineer})
	}
	before := gateLoads.Load()
	b := j(must(s.GetBoard(ctx, "orch", StatusFilter{})))
	if n := len(b["agents"].([]any)); n < 8 {
		t.Fatalf("only %d agents on the board", n)
	}
	if got := gateLoads.Load() - before; got != 1 {
		t.Fatalf("gate inputs read %d times for one board of %d agents, want 1", got, len(b["agents"].([]any)))
	}
}

// The batched gate source yields the same gates as reading them agent by agent.
func TestGateSourceMatchesPerAgentReads(t *testing.T) {
	s := newSvc(t, true)
	cfg := GateConfig{MaxConcurrent: 1, DailyUSD: 1, PerAgentUSD: 0.5}
	_ = s.Store.SetAgentStatus("orch", "working")
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	_, _ = s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "note", Body: "x"})
	_ = s.Store.AddUsage("pm", 1, 1, 0.6)
	_ = s.Store.SetAgentPaused("fg", true)
	now := time.Now()
	src := NewGateSource(s.Store, cfg, now)
	for _, a := range s.Team.Agents {
		if got, want := src.Agent(a.ID), ReadAgentGate(s.Store, cfg, a.ID, now); got != want {
			t.Errorf("%s: %+v vs %+v", a.ID, got, want)
		}
	}
	g := src.Agent("eng-1")
	if !g.QueuedForSlot || !src.Agent("pm").SpendCapped || !src.Agent("fg").Paused {
		t.Fatalf("gates not derived: eng-1=%+v pm=%+v fg=%+v", g, src.Agent("pm"), src.Agent("fg"))
	}
}
