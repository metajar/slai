package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/harness"
	"github.com/oorbt/slai/internal/store"
)

// fakeLogRecords reads FAKEHARNESS_LOG.
func fakeLogRecords(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var r map[string]any
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

func recsOf(recs []map[string]any, event string) []map[string]any {
	var out []map[string]any
	for _, r := range recs {
		if r["event"] == event {
			out = append(out, r)
		}
	}
	return out
}

// tool runs `slai tool` as agent against the project.
func (p *project) tool(agent, name, args string) string {
	p.t.Helper()
	cmd := exec.Command(filepath.Join(binDir, "slai"), "tool", name, args)
	cmd.Dir = p.dir
	cmd.Env = append(append([]string{}, p.env...), "SLAI_AGENT_ID="+agent, "SLAI_PROJECT="+p.dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		p.t.Fatalf("slai tool %s as %s: %v\n%s", name, agent, err, out)
	}
	return string(out)
}

// harnessProject is a trimmed project whose eng-1 runs on the given harness, with
// the store rows and run files `slai up` would create, but no supervisor: the fake
// is started by hand so the test controls exactly what it sees.
type harnessProject struct {
	*project
	kind     string
	worktree string
	ls       harness.LaunchSpec
	h        harness.Harness
}

func newHarnessProject(t *testing.T, kind string) *harnessProject {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed: the fake pi/opencode load the generated JS")
	}
	p := newProject(t, opts{engines: 1})
	teamPath := filepath.Join(p.dir, ".slai", "team.yaml")
	team, err := config.Load(teamPath)
	if err != nil {
		t.Fatal(err)
	}
	for i := range team.Agents {
		if team.Agents[i].ID == "eng-1" {
			team.Agents[i].Harness, team.Agents[i].Model = kind, "fake/model"
		}
	}
	if err := config.Write(teamPath, team); err != nil {
		t.Fatal(err)
	}
	p.mustGit("branch", "slai/integration", "main") // `slai up` creates it
	wt := filepath.Join(p.dir, ".slai", "worktrees", "eng-1")
	p.mustGit("worktree", "add", "-q", wt, "-b", "slai/eng-1/setup", "main")
	st := p.db()
	for _, a := range team.Agents {
		ag := store.Agent{ID: a.ID, Role: a.Role, Model: a.Model, Status: "idle", TmuxSession: "fh-" + a.ID}
		if a.ID == "eng-1" {
			ag.Worktree = wt
		}
		if err := st.UpsertAgent(ag); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()
	h, err := harness.For(harness.Kind(kind))
	if err != nil {
		t.Fatal(err)
	}
	hp := &harnessProject{project: p, kind: kind, worktree: wt, h: h}
	hp.ls = harness.LaunchSpec{AgentID: "eng-1", Model: "fake/model", PermissionMode: "auto", ProjectDir: p.dir, WorkDir: wt,
		SlaiBin: filepath.Join(binDir, "slai"), SystemPrompt: "You are eng-1.", SessionID: h.NewSessionID()}
	if err := h.WriteRunFiles(hp.ls); err != nil {
		t.Fatal(err)
	}
	return hp
}

// run starts the fake exactly as the harness' Command line would (run under sh,
// with a deliberately wrong ambient identity), feeds it the lines and waits for it.
func (hp *harnessProject) run(extraEnv []string, lines ...string) (stdout string, log []map[string]any) {
	t := hp.t
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "fakeharness.log")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", hp.h.Command(hp.ls))
	cmd.Dir = hp.worktree
	cmd.Env = append(append([]string{}, hp.env...), "FAKEHARNESS_LOG="+logPath, "SLAI_AGENT_ID=intruder", "SLAI_PROJECT=/somewhere/else")
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.Stdin = strings.NewReader(strings.Join(lines, "\n") + "\nexit\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s fake: %v\n%s", hp.kind, err, out)
	}
	return string(out), fakeLogRecords(t, logPath)
}

// AC 20: a scripted fake pi / opencode joins the team using only the run files:
// the slai MCP server comes from the generated extension / opencode.json, and the
// ambient SLAI_AGENT_ID is wrong. It reads its inbox, answers a ping and an ask,
// reports a task done, and its lifecycle reaches slai through the real extension /
// plugin and `slai hook`.
func TestFakeHarnessesJoinThroughRunFiles(t *testing.T) {
	for _, kind := range []string{"pi", "opencode"} {
		t.Run(kind, func(t *testing.T) {
			hp := newHarnessProject(t, kind)
			ask := hp.tool("principal", "ask", `{"to":"eng-1","question":"which db do we use?"}`)
			if !strings.Contains(ask, "message_id") {
				t.Fatalf("ask: %s", ask)
			}
			st := hp.db()
			pingID, err := st.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "ping", Body: "ping"})
			st.Close()
			if err != nil {
				t.Fatal(err)
			}
			hp.tool("orch", "create_task", `{"title":"fake task","description":"do it","assignee":"eng-1"}`)

			out, log := hp.run(nil, "[slai] You have 3 new items. Call get_inbox.")

			start := recsOf(log, "start")
			if len(start) != 1 {
				t.Fatalf("start records: %v\n%s", start, out)
			}
			s := start[0]
			if s["agent"] != "eng-1" || s["project"] != hp.dir {
				t.Errorf("identity came from %v/%v, want eng-1/%s", s["agent"], s["project"], hp.dir)
			}
			if s["env_agent"] != "eng-1" {
				t.Errorf("the Command pins the inline env: env_agent=%v", s["env_agent"])
			}
			args := strings.Join(toStrings(s["mcp_args"]), " ")
			if !strings.Contains(args, "mcp --agent eng-1 --project "+hp.dir) {
				t.Errorf("the MCP server is not launched with the pinned flags: %s", args)
			}
			if kind == "pi" && !strings.Contains(strings.Join(toStrings(s["args"]), " "), "--no-approve") {
				t.Errorf("pi must be launched with --no-approve: %v", s["args"])
			}
			tools := toStrings(recsOf(log, "tools")[0]["tools"])
			for _, want := range []string{"get_inbox", "report", "ask", "reply", "pong"} {
				if !contains(tools, want) {
					t.Errorf("slai mcp exposed no %s to the engineer: %v", want, tools)
				}
			}
			called := map[string]bool{}
			for _, r := range recsOf(log, "tool") {
				if r["error"] == nil || r["error"] == "" {
					called[r["name"].(string)] = true
				}
			}
			for _, want := range []string{"get_inbox", "pong", "reply", "get_task", "request_review"} {
				if !called[want] {
					t.Errorf("tool %s was not called successfully; calls: %v", want, called)
				}
			}

			// what slai saw
			st = hp.db()
			defer st.Close()
			if _, ok, _ := st.GetKV("pong." + itoa(pingID)); !ok {
				t.Errorf("the pong for ping %d never reached slai", pingID)
			}
			msgs, _ := st.RecentMessages(50)
			gotReply := false
			for _, m := range msgs {
				if m.Kind == "reply" && m.From == "eng-1" && m.To == "principal" && strings.Contains(m.Body, "fake "+kind) {
					gotReply = true
				}
			}
			if !gotReply {
				t.Errorf("no reply from eng-1 to principal in %v", msgs)
			}
			tasks, _ := st.ListTasks("")
			if len(tasks) != 1 || tasks[0].Status != "in_review" {
				for _, r := range recsOf(log, "tool") {
					t.Logf("tool %v %v => %v %v", r["name"], r["args"], r["result"], r["error"])
				}
				t.Errorf("after the fake engineer's turn the task should be in review: %+v", tasks)
			}
			hasEvent := func(kind string) bool {
				es, _ := st.EventsByKind("eng-1", kind, 5)
				return len(es) > 0
			}
			for _, k := range []string{"hook:sessionstart", "hook:userpromptsubmit", "hook:pretooluse", "hook:posttooluse", "hook:stop"} {
				if !hasEvent(k) {
					t.Errorf("slai recorded no %s from the %s harness", k, kind)
				}
			}
			if kind == "pi" {
				if _, err := os.Stat(harness.PiGuardReadyPath(hp.dir, "eng-1")); err != nil {
					t.Errorf("pi extension never wrote guard_ready: %v", err)
				}
			} else if !hasEvent("hook:guard_ready") {
				t.Error("opencode plugin never reported guard_ready")
			}
			if kind == "opencode" {
				if id, _ := os.ReadFile(harness.SessionIDPath(hp.dir, "eng-1")); !strings.HasPrefix(strings.TrimSpace(string(id)), "ses_") {
					t.Errorf("the plugin's session.created id was not stored: %q", id)
				}
			}
		})
	}
}

// AC 10 end to end: a write outside the lane through the harness' own guard is
// refused by `slai hook`, and the file never appears. A write inside the lane passes.
func TestFakeHarnessLaneGuardBlocksWritesOutsideTheWorktree(t *testing.T) {
	for _, kind := range []string{"pi", "opencode"} {
		t.Run(kind, func(t *testing.T) {
			hp := newHarnessProject(t, kind)
			outside := filepath.Join(hp.dir, "escaped.txt")
			_, log := hp.run([]string{"FAKEHARNESS_WRITE_ENG_1=" + outside}, "[slai] You have 0 new items. Call get_inbox.")
			w := recsOf(log, "write_attempt")
			if len(w) != 1 {
				t.Fatalf("no write attempt recorded: %v", log)
			}
			if w[0]["blocked"] != true {
				t.Fatalf("the out-of-lane write was not blocked: %v", w)
			}
			if _, err := os.Stat(outside); err == nil {
				t.Fatal("the file was written anyway")
			}
			if ev := hp.events("guard:lane", "eng-1"); len(ev) == 0 {
				t.Error("no guard:lane event for the denied write")
			}
		})
	}
}

func toStrings(v any) []string {
	var out []string
	if l, ok := v.([]any); ok {
		for _, x := range l {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
