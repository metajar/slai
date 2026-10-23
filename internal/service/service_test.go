package service

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/git"
	"github.com/oorbt/slai/internal/store"
)

var ctx = context.Background()

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// NewTestService builds a service over a temp git repo and store with a
// 5-agent team: orch, pm, prin, eng-1, fg.
func newSvc(t *testing.T, requireReview bool) *Service {
	t.Helper()
	curT = t
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	for k, v := range map[string]string{
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@e.com",
		"GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@e.com",
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_SYSTEM": os.DevNull,
	} {
		t.Setenv(k, v)
	}
	dir := t.TempDir()
	repo := &git.Repo{Dir: dir}
	if err := repo.InitRepo(ctx); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644)
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-m", "init")
	main, err := repo.DetectMainBranch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureBranch(ctx, "slai/integration", main); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	team := &config.Team{
		Project:  config.Project{Name: "p", MainBranch: main, IntegrationBranch: "slai/integration"},
		Defaults: config.Defaults{RequireReview: requireReview},
		Agents: []config.Agent{
			{ID: "orch", Role: config.RoleOrchestrator}, {ID: "pm", Role: config.RolePM},
			{ID: "prin", Role: config.RolePrincipal}, {ID: "eng-1", Role: config.RoleEngineer},
			{ID: "fg", Role: config.RoleFocusGroup},
		},
	}
	for _, a := range team.Agents {
		st.UpsertAgent(store.Agent{ID: a.ID, Role: a.Role})
	}
	return &Service{Store: st, Git: repo, Team: team, Dir: dir}
}

// commitOnBranch creates branch from integration with one commit.
func commitOnBranch(t *testing.T, s *Service, branch string) {
	t.Helper()
	gitRun(t, s.Dir, "branch", branch, "slai/integration")
	gitRun(t, s.Dir, "checkout", branch)
	os.WriteFile(filepath.Join(s.Dir, "b.txt"), []byte("two\n"), 0o644)
	gitRun(t, s.Dir, "add", "-A")
	gitRun(t, s.Dir, "commit", "-m", "work")
	gitRun(t, s.Dir, "checkout", "slai/integration")
}

func j(v any) map[string]any {
	b, _ := json.Marshal(v)
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}

var curT *testing.T

func must[T any](v T, err error) T {
	curT.Helper()
	if err != nil {
		curT.Fatal(err)
	}
	return v
}

func wantErr(t *testing.T, err error, sub string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), sub) {
		t.Fatalf("want error containing %q, got %v", sub, err)
	}
}

func createTask(t *testing.T, s *Service) string {
	t.Helper()
	r := j(must(s.CreateTask(ctx, "orch", CreateTaskArgs{Title: "Do it", Description: "desc", Assignee: "eng-1", Files: []string{"b.txt"}})))
	return r["task_id"].(string)
}

func eventKinds(t *testing.T, s *Service) []string {
	var out []string
	for _, e := range must(s.Store.RecentEvents(100)) {
		out = append(out, e.Kind)
	}
	return out
}

func TestRoleMatrix(t *testing.T) {
	s := newSvc(t, true)
	all := Tools
	allowed := map[string][]string{
		"orch":  {"create_task", "update_task", "get_board", "send_message", "ask_user", "get_inbox", "cancel_task", "finish", "ping", "pong", "reply", "remember", "recall", "get_memory", "whoami", "clear_context"},
		"pm":    {"get_board", "get_task", "report", "send_message", "ask", "ask_user", "get_inbox", "pong", "reply", "remember", "recall", "get_memory", "whoami"},
		"prin":  {"get_board", "review_diff", "approve", "request_changes", "send_message", "ask", "get_inbox", "pong", "reply", "remember", "recall", "get_memory", "whoami"},
		"eng-1": {"get_task", "report", "request_review", "ask", "send_message", "get_inbox", "pong", "reply", "remember", "recall", "get_memory", "whoami"},
		"fg":    {"get_inbox", "reply", "get_board", "pong", "recall", "get_memory", "whoami"},
	}
	for who, ok := range allowed {
		set := map[string]bool{}
		for _, n := range ok {
			set[n] = true
		}
		for _, tool := range all {
			err := s.Authorize(who, tool)
			if set[tool] && err != nil {
				t.Errorf("%s/%s should be allowed: %v", who, tool, err)
			}
			if !set[tool] && err == nil {
				t.Errorf("%s/%s should be denied", who, tool)
			}
		}
	}
	if s.Authorize("ghost", "get_inbox") == nil {
		t.Error("unknown caller allowed")
	}
	// Denied call writes no event and has no effect.
	_, err := s.CreateTask(ctx, "eng-1", CreateTaskArgs{Title: "x", Assignee: "eng-1"})
	wantErr(t, err, "not allowed")
	if ts, _ := s.Store.ListTasks(""); len(ts) != 0 {
		t.Error("task created by denied caller")
	}
	_, err = s.Call(ctx, "fg", "approve", json.RawMessage(`{"task_id":"T-0001"}`))
	wantErr(t, err, "not allowed")
}

func TestLifecycleApprove(t *testing.T) {
	s := newSvc(t, true)
	id := createTask(t, s)
	if id != "T-0001" {
		t.Fatal(id)
	}
	task := must(s.Store.GetTask(id))
	if task.Status != "assigned" || task.Branch != "slai/eng-1/T-0001" || task.CreatedBy != "orch" {
		t.Fatalf("%+v", task)
	}
	// assignment queued
	in := j(must(s.GetInbox(ctx, "eng-1")))["items"].([]any)
	if len(in) != 1 || in[0].(map[string]any)["kind"] != "assignment" {
		t.Fatalf("inbox %v", in)
	}
	if in2 := j(must(s.GetInbox(ctx, "eng-1")))["items"].([]any); len(in2) != 0 {
		t.Fatal("inbox not drained")
	}
	// request_review before moving in progress / without commits
	_, err := s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "s"})
	wantErr(t, err, "in_progress")
	gt := j(must(s.GetTask(ctx, "eng-1", GetTaskArgs{TaskID: id})))
	if gt["status"] != "in_progress" || gt["description"] != "desc" || gt["branch"] != "slai/eng-1/T-0001" {
		t.Fatalf("%v", gt)
	}
	if must(s.Store.GetTask(id)).Status != "in_progress" {
		t.Fatal("not persisted")
	}
	_, err = s.GetTask(ctx, "eng-1", GetTaskArgs{TaskID: "T-9999"})
	wantErr(t, err, "not found")
	_, err = s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "s"})
	if err == nil {
		t.Fatal("expected error for missing/empty branch")
	}
	gitRun(t, s.Dir, "branch", "slai/eng-1/T-0001", "slai/integration")
	_, err = s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "s"})
	wantErr(t, err, "no commits")
	// reviewing a task with no commits and not in review explains itself
	_, err = s.ReviewDiff(ctx, "prin", TaskRef{TaskID: id})
	wantErr(t, err, "nothing to review")
	gitRun(t, s.Dir, "branch", "-D", "slai/eng-1/T-0001")
	commitOnBranch(t, s, "slai/eng-1/T-0001")

	// approve before review fails
	_, err = s.Approve(ctx, "prin", ApproveArgs{TaskID: id})
	wantErr(t, err, "not in_review")

	r := j(must(s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "done it"})))
	if r["status"] != "in_review" {
		t.Fatal(r)
	}
	pin := j(must(s.GetInbox(ctx, "prin")))["items"].([]any)
	if len(pin) != 1 || pin[0].(map[string]any)["kind"] != "review" {
		t.Fatalf("%v", pin)
	}
	d := j(must(s.ReviewDiff(ctx, "prin", TaskRef{TaskID: id})))
	if !strings.Contains(d["patch"].(string), "+two") || d["files"].([]any)[0] != "b.txt" {
		t.Fatalf("%v", d)
	}
	if d["integration_branch"] != "slai/integration" || len(d["base_sha"].(string)) != 40 || len(d["integration_sha"].(string)) != 40 {
		t.Fatalf("review_diff must name the integration branch and base: %v", d)
	}
	ap := j(must(s.Approve(ctx, "prin", ApproveArgs{TaskID: id, Notes: "lgtm"})))
	if ap["status"] != "merge_queued" {
		t.Fatal(ap)
	}
	evs, _ := s.Store.RecentEvents(100)
	var found bool
	for _, e := range evs {
		if e.Kind == "merge_requested" {
			found = true
			var d map[string]string
			json.Unmarshal([]byte(e.Data), &d)
			if d["task_id"] != id || d["notes"] != "lgtm" || e.Agent != "prin" {
				t.Fatalf("%+v", e)
			}
		}
	}
	if !found {
		t.Fatal("no merge_requested event")
	}
	// service never merges
	if must(s.Store.GetTask(id)).Status != "in_review" {
		t.Fatal("approve must not change status")
	}
	kinds := strings.Join(eventKinds(t, s), ",")
	for _, k := range []string{"create_task", "get_task", "request_review", "approve", "review_diff", "get_inbox"} {
		if !strings.Contains(kinds, k) {
			t.Errorf("missing event %s in %s", k, kinds)
		}
	}
}

func TestNoReviewRequired(t *testing.T) {
	s := newSvc(t, false)
	id := createTask(t, s)
	must(s.GetTask(ctx, "eng-1", GetTaskArgs{TaskID: id}))
	commitOnBranch(t, s, "slai/eng-1/"+id)
	r := j(must(s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "x"})))
	if r["status"] != "merge_queued" {
		t.Fatal(r)
	}
	evs, _ := s.Store.RecentEvents(10)
	if evs[0].Kind != "merge_requested" || evs[0].Agent != "eng-1" || !strings.Contains(evs[0].Data, id) {
		t.Fatalf("%+v", evs[0])
	}
	if in := j(must(s.GetInbox(ctx, "prin")))["items"].([]any); len(in) != 0 {
		t.Fatal("principal should not be notified")
	}
}

func TestChangesRequestedRoundsBlock(t *testing.T) {
	s := newSvc(t, true)
	id := createTask(t, s)
	must(s.GetTask(ctx, "eng-1", GetTaskArgs{TaskID: id}))
	commitOnBranch(t, s, "slai/eng-1/"+id)
	s.GetInbox(ctx, "orch")
	for round := 1; round <= 3; round++ {
		must(s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "again"}))
		r := j(must(s.RequestChanges(ctx, "prin", RequestChangesArgs{TaskID: id, Comments: "fix thing " + string(rune('0'+round))})))
		if int(r["review_rounds"].(float64)) != round {
			t.Fatal(r)
		}
		if round < 3 {
			if r["status"] != "changes_requested" {
				t.Fatal(r)
			}
			in := j(must(s.GetInbox(ctx, "eng-1")))["items"].([]any)
			last := in[len(in)-1].(map[string]any)
			if last["kind"] != "review" || !strings.Contains(last["body"].(string), "fix thing") {
				t.Fatalf("%v", in)
			}
			// get_task resumes work and shows comments
			gt := j(must(s.GetTask(ctx, "eng-1", GetTaskArgs{TaskID: id})))
			if gt["status"] != "in_progress" || len(gt["review_comments"].([]any)) != round {
				t.Fatalf("%v", gt)
			}
		} else if r["status"] != "blocked" {
			t.Fatal(r)
		}
	}
	oin := j(must(s.GetInbox(ctx, "orch")))["items"].([]any)
	if len(oin) != 1 || !strings.Contains(oin[0].(map[string]any)["body"].(string), "blocked") {
		t.Fatalf("%v", oin)
	}
	evs, _ := s.Store.RecentEvents(100)
	var n int
	for _, e := range evs {
		if e.Kind == "ask_user" {
			n++
			if !strings.Contains(e.Data, id) {
				t.Fatal(e.Data)
			}
		}
	}
	if n != 1 {
		t.Fatalf("ask_user events %d", n)
	}
	_, err := s.RequestChanges(ctx, "prin", RequestChangesArgs{TaskID: id, Comments: "x"})
	wantErr(t, err, "not in_review")
}

func TestOtherTools(t *testing.T) {
	s := newSvc(t, true)
	id := createTask(t, s)
	// validation
	_, err := s.CreateTask(ctx, "orch", CreateTaskArgs{Title: "x", Assignee: "nobody"})
	wantErr(t, err, "not an agent")
	_, err = s.CreateTask(ctx, "orch", CreateTaskArgs{Title: "x", Assignee: "eng-1", ParentID: "T-77"})
	wantErr(t, err, "not found")
	// board
	b := j(must(s.GetBoard(ctx, "fg", StatusFilter{})))["tasks"].([]any)
	if len(b) != 1 {
		t.Fatal(b)
	}
	if b := j(must(s.GetBoard(ctx, "fg", StatusFilter{Status: "merged"})))["tasks"].([]any); len(b) != 0 {
		t.Fatal(b)
	}
	// get_task only for the assignee, even for roles that may call it
	_, err = s.Call(ctx, "pm", "get_task", json.RawMessage(`{"task_id":"`+id+`"}`))
	wantErr(t, err, "assigned to eng-1")
	_, err = s.Call(ctx, "prin", "get_task", json.RawMessage(`{"task_id":"`+id+`"}`))
	wantErr(t, err, "not allowed")
	// update_task
	must(s.UpdateTask(ctx, "orch", UpdateTaskArgs{TaskID: id, Title: "New", Files: []string{"x", "y"}}))
	task := must(s.Store.GetTask(id))
	if task.Title != "New" || task.Files != `["x","y"]` || task.Description != "desc" {
		t.Fatalf("%+v", task)
	}
	// report blocked / done
	_, err = s.Report(ctx, "eng-1", ReportArgs{TaskID: id, Status: "weird", Summary: "s"})
	wantErr(t, err, "done")
	s.GetInbox(ctx, "orch")
	r := j(must(s.Report(ctx, "eng-1", ReportArgs{TaskID: id, Status: "blocked", Summary: "need api key"})))
	if r["status"] != "blocked" {
		t.Fatal(r)
	}
	oin := j(must(s.GetInbox(ctx, "orch")))["items"].([]any)
	if len(oin) != 1 || !strings.Contains(oin[0].(map[string]any)["body"].(string), "need api key") {
		t.Fatal(oin)
	}
	_, err = s.Report(ctx, "pm", ReportArgs{TaskID: id, Status: "done", Summary: "s"})
	wantErr(t, err, "assigned to")
	// cancel
	must(s.CancelTask(ctx, "orch", CancelTaskArgs{TaskID: id, Reason: "dropped"}))
	if must(s.Store.GetTask(id)).Status != "cancelled" {
		t.Fatal("not cancelled")
	}
	ein := j(must(s.GetInbox(ctx, "eng-1")))["items"].([]any)
	if !strings.Contains(ein[len(ein)-1].(map[string]any)["body"].(string), "dropped") {
		t.Fatal(ein)
	}
	_, err = s.UpdateTask(ctx, "orch", UpdateTaskArgs{TaskID: id, Title: "z"})
	wantErr(t, err, "cannot be edited")
}

func TestMessagingAndAskUser(t *testing.T) {
	s := newSvc(t, true)
	r := j(must(s.SendMessage(ctx, "orch", SendMessageArgs{To: "all", Body: "hello"})))
	if r["sent"].(float64) != 3 { // everyone but the sender and the focus-group persona
		t.Fatal(r)
	}
	if in := j(must(s.GetInbox(ctx, "orch")))["items"].([]any); len(in) != 0 {
		t.Fatal("sender got own broadcast")
	}
	if in := j(must(s.GetInbox(ctx, "fg")))["items"].([]any); len(in) != 0 {
		t.Fatal("focus-group persona woken by a broadcast:", in)
	}
	_, err := s.SendMessage(ctx, "orch", SendMessageArgs{To: "ghost", Body: "x"})
	wantErr(t, err, "unknown recipient")
	_, err = s.SendMessage(ctx, "orch", SendMessageArgs{To: "pm", Body: strings.Repeat("x", 4001)})
	wantErr(t, err, "reference")
	must(s.SendMessage(ctx, "orch", SendMessageArgs{To: "pm", Body: strings.Repeat("é", 4000)}))
	_, err = s.CreateTask(ctx, "orch", CreateTaskArgs{Title: "t", Description: strings.Repeat("x", 4001), Assignee: "eng-1"})
	wantErr(t, err, "too long")

	// ask / reply
	a := j(must(s.Ask(ctx, "prin", AskArgs{To: "fg", Question: "would you buy?"})))
	mid := int64(a["message_id"].(float64))
	_, err = s.Reply(ctx, "fg", ReplyArgs{MessageID: 9999, Body: "x"})
	wantErr(t, err, "not found")
	_, err = s.Call(ctx, "fg", "reply", json.RawMessage(`{"message_id":4,"body":"x"}`)) // message 4 was not sent to fg
	wantErr(t, err, "not addressed to fg")
	// a persona can answer a plain note too (the orchestrator has no 'ask' tool)
	must(s.SendMessage(ctx, "orch", SendMessageArgs{To: "fg", Body: "what do you think of the spec?"}))
	note := j(must(s.GetInbox(ctx, "fg")))["items"].([]any)
	nid := int64(note[len(note)-1].(map[string]any)["id"].(float64))
	rep := j(must(s.Reply(ctx, "fg", ReplyArgs{MessageID: nid, Body: "too long"})))
	if rep["delivered_to"] != "orch" || rep["in_reply_to"].(float64) != float64(nid) || rep["message_id"] == nil {
		t.Fatalf("reply confirmation: %v", rep)
	}
	must(s.Reply(ctx, "fg", ReplyArgs{MessageID: mid, Body: "yes"}))
	in := j(must(s.GetInbox(ctx, "prin")))["items"].([]any)
	last := in[len(in)-1].(map[string]any)
	if last["kind"] != "reply" || last["body"] != "yes" || int64(last["reply_to"].(float64)) != mid {
		t.Fatal(in)
	}
	_, err = s.Reply(ctx, "fg", ReplyArgs{MessageID: mid, Body: "again"})
	if err != nil {
		t.Fatal(err) // replying twice is allowed
	}
	// ask_user
	must(s.AskUser(ctx, "pm", AskUserArgs{Question: "which db?"}))
	ags, _ := s.Store.GetAgents()
	for _, ag := range ags {
		if ag.ID == "pm" && ag.Status != "waiting_on_user" {
			t.Fatal(ag.Status)
		}
	}
	evs, _ := s.Store.RecentEvents(1)
	if evs[0].Kind != "ask_user" || evs[0].Agent != "pm" || !strings.Contains(evs[0].Data, "which db?") {
		t.Fatalf("%+v", evs[0])
	}
}

func TestCallDispatch(t *testing.T) {
	s := newSvc(t, true)
	out, err := s.Call(ctx, "orch", "create_task", json.RawMessage(`{"title":"a","description":"b","assignee":"eng-1"}`))
	if err != nil || j(out)["task_id"] != "T-0001" {
		t.Fatal(out, err)
	}
	_, err = s.Call(ctx, "orch", "create_task", json.RawMessage(`{"bogus":1}`))
	wantErr(t, err, "invalid arguments")
	_, err = s.Call(ctx, "orch", "nope", nil)
	wantErr(t, err, "not allowed")
	if _, err := s.Call(ctx, "orch", "get_board", nil); err != nil {
		t.Fatal(err)
	}
}

func TestGetTaskSpecPathAbsolute(t *testing.T) {
	s := newSvc(t, true)
	tk := j(must(s.CreateTask(ctx, "orch", CreateTaskArgs{Title: "T", Description: "d", Assignee: "eng-1", SpecPath: ".slai/specs/x.md"})))
	got := j(must(s.GetTask(ctx, "eng-1", GetTaskArgs{TaskID: tk["task_id"].(string)})))
	if want := filepath.Join(s.Dir, ".slai/specs/x.md"); got["spec_path"] != want {
		t.Fatalf("spec_path = %v, want %s", got["spec_path"], want)
	}
}
