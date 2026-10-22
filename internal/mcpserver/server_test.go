package mcpserver

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/git"
	"github.com/oorbt/slai/internal/roles"
	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
)

func newService(t *testing.T) *service.Service {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	team := &config.Team{
		Project: config.Project{IntegrationBranch: "slai/integration"},
		Agents: []config.Agent{
			{ID: "orch", Role: config.RoleOrchestrator}, {ID: "pm", Role: config.RolePM},
			{ID: "prin", Role: config.RolePrincipal}, {ID: "eng-1", Role: config.RoleEngineer},
			{ID: "fg", Role: config.RoleFocusGroup},
		},
	}
	return &service.Service{Store: st, Git: &git.Repo{Dir: t.TempDir()}, Team: team}
}

func connect(t *testing.T, svc *service.Service, caller string) *mcp.ClientSession {
	t.Helper()
	srv, err := New(svc, caller)
	if err != nil {
		t.Fatal(err)
	}
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, tool string, args any) (*mcp.CallToolResult, error) {
	t.Helper()
	return cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
}

func text(r *mcp.CallToolResult) string {
	if len(r.Content) == 0 {
		return ""
	}
	return r.Content[0].(*mcp.TextContent).Text
}

func TestToolListMatchesRole(t *testing.T) {
	svc := newService(t)
	for _, a := range svc.Team.Agents {
		cs := connect(t, svc, a.ID)
		res, err := cs.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, tl := range res.Tools {
			got = append(got, tl.Name)
			if tl.Description == "" || tl.InputSchema == nil {
				t.Errorf("%s: tool %s missing description/schema", a.ID, tl.Name)
			}
		}
		want := roles.ToolNames(a.Role)
		sort.Strings(got)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: got %v want %v", a.ID, got, want)
		}
	}
}

func TestCallsAsDifferentRoles(t *testing.T) {
	svc := newService(t)
	orch := connect(t, svc, "orch")
	eng := connect(t, svc, "eng-1")

	r, err := call(t, orch, "create_task", map[string]any{"title": "T", "description": "d", "assignee": "eng-1"})
	if err != nil || r.IsError {
		t.Fatal(err, r)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text(r)), &out); err != nil || out["task_id"] != "T-0001" || out["branch"] != "slai/eng-1/T-0001" {
		t.Fatalf("%s %v", text(r), err)
	}

	r, _ = call(t, eng, "get_inbox", map[string]any{})
	if r.IsError || !strings.Contains(text(r), `"kind":"assignment"`) {
		t.Fatalf("%s", text(r))
	}
	r, _ = call(t, eng, "get_task", map[string]any{"task_id": "T-0001"})
	if r.IsError || !strings.Contains(text(r), `"status":"in_progress"`) {
		t.Fatalf("%s", text(r))
	}

	// engineer cannot create tasks: not registered, so the call fails
	r, err = call(t, eng, "create_task", map[string]any{"title": "x", "assignee": "eng-1"})
	if err == nil && !r.IsError {
		t.Fatalf("engineer create_task succeeded: %s", text(r))
	}
	if ts, _ := svc.Store.ListTasks(""); len(ts) != 1 {
		t.Fatalf("tasks=%d", len(ts))
	}

	// tool errors surface as IsError results
	r, err = call(t, eng, "get_task", map[string]any{"task_id": "T-0042"})
	if err != nil || !r.IsError || !strings.Contains(text(r), "not found") {
		t.Fatal(err, r)
	}
	// call-time authorization even if a tool were registered: svc layer rejects
	if err := svc.Authorize("eng-1", "approve"); err == nil {
		t.Fatal("authorize should reject")
	}
}

func TestReportAndGetTaskWithoutTaskID(t *testing.T) {
	svc := newService(t)
	orch := connect(t, svc, "orch")
	pm := connect(t, svc, "pm")
	eng := connect(t, svc, "eng-1")

	// no task assigned: clear result, not a schema error
	r, err := call(t, pm, "get_task", map[string]any{})
	if err != nil || r.IsError || !strings.Contains(text(r), "no task assigned") {
		t.Fatal(err, text(r))
	}
	// message-driven report goes to the orchestrator as a note
	r, err = call(t, pm, "report", map[string]any{"status": "done", "summary": "spec at specs/x.md"})
	if err != nil || r.IsError {
		t.Fatal(err, text(r))
	}
	r, _ = call(t, orch, "get_inbox", map[string]any{})
	if !strings.Contains(text(r), "pm reports done: spec at specs/x.md") {
		t.Fatalf("orch inbox: %s", text(r))
	}
	// a given but unknown task_id keeps the old behaviour
	r, _ = call(t, pm, "report", map[string]any{"task_id": "T-0099", "status": "done", "summary": "x"})
	if !r.IsError || !strings.Contains(text(r), "not found") {
		t.Fatalf("%s", text(r))
	}

	// with one assigned task, get_task() returns it (and starts it)
	if r, err := call(t, orch, "create_task", map[string]any{"title": "T", "description": "d", "assignee": "eng-1"}); err != nil || r.IsError {
		t.Fatal(err, text(r))
	}
	r, _ = call(t, eng, "get_task", map[string]any{})
	if r.IsError || !strings.Contains(text(r), `"id":"T-0001"`) || !strings.Contains(text(r), `"status":"in_progress"`) {
		t.Fatalf("%s", text(r))
	}
	// task-less report resolves to the open task: the engineer guard still applies
	r, _ = call(t, eng, "report", map[string]any{"status": "done", "summary": "all done"})
	if !r.IsError || !strings.Contains(text(r), "request_review") {
		t.Fatalf("task-less report(done) bypassed the review guard: %s", text(r))
	}
	if r, _ = call(t, eng, "report", map[string]any{"status": "blocked", "summary": "stuck"}); r.IsError {
		t.Fatalf("%s", text(r))
	}
	if tk, _ := svc.Store.GetTask("T-0001"); tk.Status != "blocked" {
		t.Fatalf("T-0001 status = %s, want blocked", tk.Status)
	}
	if err := svc.Store.UpdateTaskStatus("T-0001", "in_progress"); err != nil {
		t.Fatal(err)
	}
	// with several, a list
	call(t, orch, "create_task", map[string]any{"title": "U", "description": "d", "assignee": "eng-1"})
	r, _ = call(t, eng, "get_task", map[string]any{})
	if r.IsError || !strings.Contains(text(r), `"tasks"`) || !strings.Contains(text(r), "T-0002") {
		t.Fatalf("%s", text(r))
	}
	// several open tasks: task-less report is ambiguous
	r, _ = call(t, eng, "report", map[string]any{"status": "blocked", "summary": "x"})
	if !r.IsError || !strings.Contains(text(r), "several open tasks (T-0001, T-0002); pass task_id") {
		t.Fatalf("%s", text(r))
	}
	// report with task_id still works for the assignee (blocked)
	r, _ = call(t, eng, "report", map[string]any{"task_id": "T-0001", "status": "blocked", "summary": "stuck"})
	if r.IsError || !strings.Contains(text(r), `"task_id":"T-0001"`) {
		t.Fatalf("%s", text(r))
	}
}

// A call the SDK rejects before our handler (wrong argument type) must still count
// as the agent reaching the server: the supervisor's MCP-crash check keys on it.
func TestInvalidArgumentsStillStampLastSeen(t *testing.T) {
	svc := newService(t)
	cs := connect(t, svc, "eng-1")
	if v, ok, _ := svc.Store.GetKV("lastseen.eng-1"); ok {
		t.Fatalf("stamped before any call: %q", v)
	}
	_, err := call(t, cs, "report", map[string]any{"status": 42}) // status must be a string
	if err == nil {
		t.Log("call unexpectedly accepted")
	}
	if _, ok, _ := svc.Store.GetKV("lastseen.eng-1"); !ok {
		t.Fatal("a rejected tools/call left no last-seen stamp")
	}
}
