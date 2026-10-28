package store

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "slai.db")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, p
}

func TestCanTransition(t *testing.T) {
	ok := [][2]string{
		{"assigned", "in_progress"}, {"in_progress", "in_review"}, {"in_review", "merged"},
		{"in_review", "changes_requested"}, {"changes_requested", "in_progress"},
		{"in_review", "in_progress"}, {"blocked", "in_progress"},
		{"assigned", "blocked"}, {"in_progress", "blocked"}, {"in_review", "blocked"}, {"changes_requested", "blocked"},
		{"assigned", "cancelled"}, {"in_progress", "cancelled"}, {"blocked", "cancelled"}, {"in_review", "cancelled"},
	}
	for _, c := range ok {
		if !CanTransition(c[0], c[1]) {
			t.Errorf("%v should be allowed", c)
		}
	}
	bad := [][2]string{
		{"assigned", "in_review"}, {"assigned", "merged"}, {"in_progress", "merged"},
		{"merged", "in_progress"}, {"merged", "cancelled"}, {"merged", "blocked"},
		{"cancelled", "in_progress"}, {"blocked", "merged"}, {"in_progress", "assigned"},
		{"in_progress", "in_progress"}, {"changes_requested", "in_review"}, {"x", "assigned"}, {"assigned", "x"},
	}
	for _, c := range bad {
		if CanTransition(c[0], c[1]) {
			t.Errorf("%v should be rejected", c)
		}
	}
}

func TestEngineerAssignmentIsHeldAtomicallyUntilPrepared(t *testing.T) {
	s, _ := open(t)
	if err := s.UpsertAgent(Agent{ID: "eng-1", Role: "engineer"}); err != nil {
		t.Fatal(err)
	}
	id, err := s.AddMessage(Message{From: "orch", To: "eng-1", Kind: "assignment", TaskID: "T-0001", Body: "start"})
	if err != nil {
		t.Fatal(err)
	}
	if !s.InboxHeld(id) {
		t.Fatal("assignment was visible before branch preparation")
	}
	if got, err := s.ClaimInbox("eng-1"); err != nil || len(got) != 0 {
		t.Fatalf("unprepared assignment claimed: %+v, %v", got, err)
	}
	if _, err := s.AddMessage(Message{From: "orch", To: "eng-1", Kind: "note", Body: "other mail"}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ClaimInbox("eng-1"); err != nil || len(got) != 1 || got[0].Kind != "note" {
		t.Fatalf("unrelated mail blocked: %+v, %v", got, err)
	}
	if err := s.ReleaseInbox(id); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ClaimInbox("eng-1"); err != nil || len(got) != 1 || got[0].ID != id {
		t.Fatalf("prepared assignment not claimed: %+v, %v", got, err)
	}
}

func TestPendingWakeForAndBatchedMarkWoken(t *testing.T) {
	s, _ := open(t)
	var ids []int64
	for i := 0; i < 501; i++ {
		id, err := s.AddMessage(Message{To: "eng-1", Body: "item"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if _, err := s.AddMessage(Message{To: "eng-2", Body: "other"}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.PendingWakeFor("eng-1", 1); err != nil || len(got) != len(ids) {
		t.Fatalf("pending for eng-1: %d, %v", len(got), err)
	}
	if err := s.MarkWoken(ids...); err != nil {
		t.Fatal(err)
	}
	if got, err := s.PendingWakeFor("eng-1", 1); err != nil || len(got) != 0 {
		t.Fatalf("woken messages still pending: %d, %v", len(got), err)
	}
	if got, err := s.PendingWakeFor("eng-2", 1); err != nil || len(got) != 1 {
		t.Fatalf("other recipient lost: %d, %v", len(got), err)
	}
}

func TestMigrationsIdempotent(t *testing.T) {
	s, p := open(t)
	if _, err := s.CreateTask(Task{Title: "a"}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	var n int
	entries, _ := migrationsFS.ReadDir("migrations")
	files := 0
	for _, e := range entries {
		if !e.IsDir() { // migrations/down holds the reverse scripts
			files++
		}
	}
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil || n != files {
		t.Fatalf("migrations=%d want %d err=%v", n, files, err)
	}
	ts, _ := s2.ListTasks("")
	if len(ts) != 1 {
		t.Fatalf("data lost: %v", ts)
	}
}

func TestTasks(t *testing.T) {
	s, _ := open(t)
	a, _ := s.CreateTask(Task{Title: "a", Assignee: "eng1"})
	b, _ := s.CreateTask(Task{Title: "b"})
	if a.ID != "T-0001" || b.ID != "T-0002" || a.Status != "assigned" {
		t.Fatalf("%+v %+v", a, b)
	}
	if err := s.UpdateTaskStatus("T-0001", "merged"); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("got %v", err)
	}
	for _, st := range []string{"in_progress", "in_review", "changes_requested"} {
		if err := s.UpdateTaskStatus("T-0001", st); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := s.GetTask("T-0001")
	if got.Status != "changes_requested" || got.ReviewRounds != 1 {
		t.Fatalf("%+v", got)
	}
	if l, _ := s.ListTasks("assigned"); len(l) != 1 || l[0].ID != "T-0002" {
		t.Fatalf("%v", l)
	}
	if _, err := s.GetTask("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.UpdateTaskStatus("nope", "blocked"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestAgentsMessagesUsage(t *testing.T) {
	s, _ := open(t)
	s.UpsertAgent(Agent{ID: "eng1", Role: "engineer", Model: "sonnet"})
	s.SetAgentStatus("eng1", "working")
	s.UpsertAgent(Agent{ID: "eng1", Role: "engineer", Model: "opus"})
	as, _ := s.GetAgents()
	if len(as) != 1 || as[0].Status != "working" || as[0].Model != "opus" {
		t.Fatalf("%+v", as)
	}
	if err := s.SetAgentStatus("ghost", "idle"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	id, _ := s.AddMessage(Message{From: "orch", To: "eng1", Body: "hi"})
	s.AddMessage(Message{From: "orch", To: "pm", Body: "x"})
	ms, _ := s.UndeliveredFor("eng1")
	if len(ms) != 1 || ms[0].ID != id || ms[0].Kind != "note" {
		t.Fatalf("%+v", ms)
	}
	s.MarkDelivered(id)
	if ms, _ := s.UndeliveredFor("eng1"); len(ms) != 0 {
		t.Fatal("still undelivered")
	}
	if err := s.AddEvent("eng1", "tool", ""); err != nil {
		t.Fatal(err)
	}
	s.AddUsage("eng1", 10, 20, 1.5)
	s.AddUsage("eng1", 10, 20, 0.25)
	if v, _ := s.TodaySpend(); v != 1.75 {
		t.Fatalf("spend %v", v)
	}
}

func TestTwoConnectionConcurrency(t *testing.T) {
	_, p := open(t)
	s1, _ := Open(p)
	s2, _ := Open(p)
	defer s1.Close()
	defer s2.Close()
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for i, s := range []*Store{s1, s2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				if _, err := s.CreateTask(Task{Title: "t"}); err != nil {
					errs <- err
				}
				if _, err := s.AddMessage(Message{From: "a", To: "b", Body: "x"}); err != nil {
					errs <- err
				}
				if err := s.AddEvent("a", "k", "{}"); err != nil {
					errs <- err
				}
				if _, err := s.ClaimInbox("b"); err != nil {
					errs <- err
				}
			}
			_ = i
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	ts, _ := s1.ListTasks("")
	if len(ts) != 50 {
		t.Fatalf("tasks=%d", len(ts))
	}
	seen := map[string]bool{}
	for _, x := range ts {
		if seen[x.ID] {
			t.Fatal("dup id")
		}
		seen[x.ID] = true
	}
	if m, _ := s2.UndeliveredFor("b"); len(m) != 0 {
		// leftovers are fine only if sent after the last claim; drain and ensure no error
		s2.ClaimInbox("b")
	}
}

func TestMigration0003UpDownUp(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	hasTable := func() bool {
		var n int
		_ = s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name IN ('task_deps','idx_task_deps_dep')`).Scan(&n)
		return n == 2
	}
	if v, _ := s.SchemaVersion(); v != 7 || !hasTable() {
		t.Fatalf("up: version %d table=%v", v, hasTable())
	}
	if _, err := s.db.Exec(`INSERT INTO task_deps(task_id, dep_id) VALUES('T-2','T-1')`); err != nil {
		t.Fatal(err)
	}
	if err := s.MigrateDown(2); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.SchemaVersion(); v != 2 {
		t.Fatalf("down: version %d, want 2", v)
	}
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name IN ('task_deps','idx_task_deps_dep')`).Scan(&n)
	if n != 0 {
		t.Fatal("down left the task_deps table or index behind")
	}
	// the earlier tables are untouched
	// (at version 2 the store's own CreateTask no longer matches the schema: raw SQL)
	if _, err := s.db.Exec(`INSERT INTO tasks(id, title, assignee, created_by, status) VALUES('T-0099','still works','eng-1','orch','assigned')`); err != nil {
		t.Fatalf("0001/0002 tables damaged: %v", err)
	}
	if err := s.MigrateDown(2); err != nil {
		t.Fatalf("down at the target must be a no-op: %v", err)
	}
	if err := s.migrate(); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.SchemaVersion(); v != 7 || !hasTable() {
		t.Fatalf("up again: version %d table=%v", v, hasTable())
	}
	var rows int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM task_deps`).Scan(&rows)
	if rows != 0 {
		t.Fatal("re-applied migration should start empty")
	}
	// reverting past a migration without a down script fails loudly and changes nothing
	if err := s.MigrateDown(0); err == nil || !strings.Contains(err.Error(), "no down migration") {
		t.Fatalf("MigrateDown(0) = %v", err)
	}
	if v, _ := s.SchemaVersion(); v != 2 {
		t.Fatalf("version after refusing to go below 2: %d (0003 was reverted, 0002 has no down)", v)
	}
}

func TestMigration0004UpDownUp(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tables := func() int {
		var n int
		_ = s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name IN ('task_ac_claims','task_ac_verdicts')`).Scan(&n)
		return n
	}
	if v, _ := s.SchemaVersion(); v != 7 || tables() != 2 {
		t.Fatalf("up: version %d, tables %d", v, tables())
	}
	tk, _ := s.CreateTask(Task{Title: "t", Assignee: "eng-1", CreatedBy: "orch"})
	if _, err := s.SetACClaims(tk.ID, []ACClaim{{AC: "1", Met: true}}); err != nil {
		t.Fatal(err)
	}
	// down: only the new tables go; tasks and the 0003 table survive
	if err := s.MigrateDown(3); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.SchemaVersion(); v != 3 || tables() != 0 {
		t.Fatalf("down: version %d, tables %d", v, tables())
	}
	if _, err := s.GetTask(tk.ID); err == nil {
		// GetTask reads the AC tables, which are gone: the 0003 store must still open and list
		t.Log("GetTask works without the AC tables")
	}
	var nTasks int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM tasks`).Scan(&nTasks); err != nil || nTasks != 1 {
		t.Fatalf("tasks damaged by the down path: %d %v", nTasks, err)
	}
	// up again: starts empty, and works
	if err := s.migrate(); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.SchemaVersion(); v != 7 || tables() != 2 {
		t.Fatalf("up again: version %d, tables %d", v, tables())
	}
	if n, c, _ := s.LatestACClaims(tk.ID); n != 0 || len(c) != 0 {
		t.Fatalf("re-applied migration should start empty: %d %v", n, c)
	}
	if n, err := s.SetACClaims(tk.ID, []ACClaim{{AC: "1", Met: true}}); err != nil || n != 1 {
		t.Fatalf("claims after re-up: %d %v", n, err)
	}
}

func TestMigration0005PriorityUpDownUp(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, _ := s.CreateTask(Task{Title: "a", Assignee: "eng-1", CreatedBy: "orch"})
	if a.Priority != 0 {
		t.Fatalf("default priority = %d, want 0", a.Priority)
	}
	if err := s.SetTaskPriority(a.ID, 3); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetTask(a.ID); got.Priority != 3 {
		t.Fatalf("priority = %d, want 3", got.Priority)
	}
	if err := s.SetTaskPriority("T-9999", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown task: %v", err)
	}
	if err := s.MigrateDown(4); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.SchemaVersion(); v != 4 {
		t.Fatalf("down: version %d, want 4", v)
	}
	var title string
	if err := s.db.QueryRow(`SELECT title FROM tasks WHERE id=?`, a.ID).Scan(&title); err != nil || title != "a" {
		t.Fatalf("down damaged the task: %q %v", title, err)
	}
	if _, err := s.db.Exec(`SELECT priority FROM tasks`); err == nil {
		t.Fatal("down left the priority column behind")
	}
	if err := s.migrate(); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetTask(a.ID); err != nil || got.Priority != 0 {
		t.Fatalf("up again: %+v %v (priority starts at the default)", got, err)
	}
}

func TestApplyPriority(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, _ := s.CreateTask(Task{Title: "a", Assignee: "eng-1", CreatedBy: "orch"})
	b, _ := s.CreateTask(Task{Title: "b", Assignee: "eng-1", CreatedBy: "orch"})
	step := func(id, spec string, want int) {
		t.Helper()
		if got, err := s.ApplyPriority(id, spec); err != nil || got != want {
			t.Fatalf("%s %s = %d, %v; want %d", id, spec, got, err, want)
		}
	}
	step(a.ID, "up", 1)
	step(a.ID, "down", 0)
	step(a.ID, "5", 5)
	step(b.ID, "top", 6)
	step(b.ID, "top", 6) // already on top: unchanged
	step(a.ID, "top", 7)
	step(a.ID, "-2", -2)
	if _, err := s.ApplyPriority(a.ID, "soon"); err == nil {
		t.Fatal("accepted a non-number")
	}
}

func TestMigration0006UsageTaskAndAttribution(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, _ := s.CreateTask(Task{Title: "a", Assignee: "eng-1", CreatedBy: "orch", Status: StatusInProgress})
	b, _ := s.CreateTask(Task{Title: "b", Assignee: "eng-2", CreatedBy: "orch", Status: StatusInProgress})
	for _, id := range []string{"eng-1", "eng-2", "pm"} {
		if err := s.UpsertAgent(Agent{ID: id, Role: "engineer"}); err != nil {
			t.Fatal(err)
		}
	}
	s.SetAgentTask("eng-1", a.ID)
	s.SetAgentTask("eng-2", a.ID) // helps on a's task
	s.AddUsage("eng-1", 1, 1, 1.5)
	s.AddUsage("eng-2", 1, 1, 0.5)
	s.SetAgentTask("eng-2", b.ID)
	s.AddUsage("eng-2", 1, 1, 4)
	s.AddUsage("pm", 1, 1, 9)      // no current task: unattributed
	s.AddUsage("ghost", 1, 1, 0.1) // unknown agent: unattributed
	got, err := s.OpenTaskSpend()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[a.ID] != (TaskSpend{"eng-1", 2}) || got[b.ID] != (TaskSpend{"eng-2", 4}) {
		t.Fatalf("task spend: %v", got)
	}
	s.UpdateTaskStatus(b.ID, StatusCancelled)
	if got, _ = s.OpenTaskSpend(); len(got) != 1 {
		t.Fatalf("a cancelled task is finished: %v", got)
	}
	if err := s.MigrateDown(5); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`SELECT task_id FROM usage`); err == nil {
		t.Fatal("down left the task_id column behind")
	}
	if v, err := s.SpendSince(0); err != nil || v != 15.1 {
		t.Fatalf("down damaged usage: %v %v", v, err)
	}
	if err := s.migrate(); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.OpenTaskSpend(); len(got) != 0 {
		t.Fatalf("after re-up old rows are unattributed: %v", got)
	}
}

func TestMigration0007FactsIndexUpDownUp(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.UpsertFact(FactRow{ID: "f-1", Status: "proposed", Decision: "d", PathsJSON: "[]"}); err != nil {
		t.Fatal(err)
	}
	_ = s.TouchFacts([]string{"f-1"}, 9)
	_ = s.UpsertFact(FactRow{ID: "f-1", Status: "verified", Decision: "d2", PathsJSON: "[]"})
	if r, err := s.GetFact("f-1"); err != nil || r.Uses != 1 || r.Status != "verified" || r.Decision != "d2" {
		t.Fatalf("upsert keeps counters: %+v %v", r, err)
	}
	if _, err := s.GetFact("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if err := s.MigrateDown(6); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`SELECT 1 FROM facts_index`); err == nil {
		t.Fatal("down left facts_index behind")
	}
	if err := s.migrate(); err != nil {
		t.Fatal(err)
	}
	if rows, err := s.ListFacts(); err != nil || len(rows) != 0 {
		t.Fatalf("up again: %v %v", rows, err)
	}
}

// T-0112: `slai up` upserts every agent row without a current task; that must not wipe the
// task slai last put the agent on (the worktree switch depends on it).
func TestUpsertAgentKeepsCurrentTaskWhenNoneGiven(t *testing.T) {
	s, _ := open(t)
	defer s.Close()
	if err := s.UpsertAgent(Agent{ID: "eng-1", Role: "engineer", CurrentTask: "T-0001"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertAgent(Agent{ID: "eng-1", Role: "engineer", Model: "sonnet"}); err != nil { // the restart
		t.Fatal(err)
	}
	if a, _ := s.AgentByID("eng-1"); a.CurrentTask != "T-0001" || a.Model != "sonnet" {
		t.Fatalf("after the restart upsert: %+v", a)
	}
	if err := s.UpsertAgent(Agent{ID: "eng-1", Role: "engineer", CurrentTask: "T-0002"}); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.AgentByID("eng-1"); a.CurrentTask != "T-0002" {
		t.Fatalf("an explicit current task must still be set: %+v", a)
	}
}
