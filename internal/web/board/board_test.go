package board_test

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"html"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/web/assets"
	"github.com/oorbt/slai/internal/web/board"
	"github.com/oorbt/slai/internal/web/server"
	"github.com/oorbt/slai/internal/web/webtest"
)

var update = flag.Bool("update", false, "rewrite golden files")

// fake is ServiceActions over the real service and store, with the pieces that need tmux and
// git replaced by recorders.
type fake struct {
	board.ServiceActions
	redirected []string
	diff       *board.Diff
	diffErr    error
}

func (f *fake) SendUser(to, body string) (int64, error) { return 1, nil }

// The board does not use the previews; they exist to satisfy server.Actions.
func (f *fake) CommitPreview(context.Context) (string, error)         { return "", nil }
func (f *fake) RevertPreview(context.Context, string) (string, error) { return "", nil }
func (f *fake) Redirect(a store.Agent, text string) error {
	f.redirected = append(f.redirected, a.ID+": "+text)
	return nil
}
func (f *fake) TaskDiff(ctx context.Context, t store.Task) (*board.Diff, error) {
	return f.diff, f.diffErr
}

type fix struct {
	h   *webtest.Harness
	f   *fake
	ids map[string]string // role in the story -> task id
}

func mustTask(t *testing.T, st *store.Store, title, who string) string {
	t.Helper()
	tk, err := st.CreateTask(store.Task{Title: title, Assignee: who, Branch: "slai/" + who + "/x", CreatedBy: "orch"})
	if err != nil {
		t.Fatal(err)
	}
	return tk.ID
}

func step(t *testing.T, st *store.Store, id string, to ...string) {
	t.Helper()
	for _, s := range to {
		if err := st.UpdateTaskStatus(id, s); err != nil {
			t.Fatalf("%s -> %s: %v", id, s, err)
		}
	}
}

// newFix seeds one task in every status the board shows.
func newFix(t *testing.T, opts ...webtest.Option) *fix {
	t.Helper()
	fx := &fix{f: &fake{}, ids: map[string]string{}}
	setup := webtest.Setup(func(st *store.Store, team *config.Team) {
		team.Agents = append(team.Agents, config.Agent{ID: "eng-2", Role: config.RoleEngineer, Model: "sonnet"})
		for _, a := range []store.Agent{{ID: "orch", Role: "orchestrator", Status: "idle"}, {ID: "eng-1", Role: "engineer", Status: "working"}, {ID: "eng-2", Role: "engineer", Status: "waiting_on_user"}} {
			if err := st.UpsertAgent(a); err != nil {
				t.Fatal(err)
			}
			_ = st.SetAgentStatus(a.ID, a.Status)
		}
		ids := fx.ids
		ids["assigned"] = mustTask(t, st, "Assigned one", "eng-1")
		ids["working"] = mustTask(t, st, "Working one", "eng-1")
		step(t, st, ids["working"], store.StatusInProgress)
		ids["stuck"] = mustTask(t, st, "Stuck on a problem", "eng-2")
		step(t, st, ids["stuck"], store.StatusBlocked)
		ids["waiting"] = mustTask(t, st, "Waits for the working one", "eng-1")
		if err := st.SetTaskDeps(ids["waiting"], []string{ids["working"]}); err != nil {
			t.Fatal(err)
		}
		step(t, st, ids["waiting"], store.StatusBlocked)
		ids["review"] = mustTask(t, st, "In review one", "eng-1")
		step(t, st, ids["review"], store.StatusInProgress, store.StatusInReview)
		ids["merged"] = mustTask(t, st, "Merged one", "eng-1")
		step(t, st, ids["merged"], store.StatusInProgress, store.StatusInReview, store.StatusMerged)
		ids["cancelled"] = mustTask(t, st, "Cancelled one", "eng-1")
		step(t, st, ids["cancelled"], store.StatusCancelled)
		ids["reverted"] = mustTask(t, st, "Reverted one", "eng-1")
		step(t, st, ids["reverted"], store.StatusInProgress, store.StatusInReview, store.StatusMerged)
		if err := st.MarkReverted(ids["reverted"]); err != nil {
			t.Fatal(err)
		}
		ids["changes"] = mustTask(t, st, "Changes requested one", "eng-1")
		step(t, st, ids["changes"], store.StatusInProgress, store.StatusInReview, store.StatusChangesRequested)
		ids["done"] = mustTask(t, st, "Finished spec", "eng-1")
		step(t, st, ids["done"], store.StatusInProgress, store.StatusDone)
	})
	fn := webtest.WithActionsFunc(func(h *webtest.Harness) server.Actions {
		fx.f.ServiceActions = board.ServiceActions{
			Svc:      &service.Service{Store: h.Store, Team: h.Team, Dir: h.Dir},
			Priority: func(id, spec string) (int, error) { return h.Store.ApplyPriority(id, spec) },
		}
		return fx.f
	})
	all := append([]webtest.Option{webtest.WithModules(board.Module), setup, fn}, opts...)
	fx.h = webtest.New(t, all...)
	fx.h.Login()
	return fx
}

// post is what the page's htmx sends: the CSRF header, the Origin and HX-Request.
func (fx *fix) post(path string, form url.Values) *webtest.Resp {
	req := fx.h.Browser("POST", path, form)
	req.Header.Set("HX-Request", "true")
	return fx.h.Do(req)
}

func colOf(body, id string) string {
	// the column section that contains the card
	i := strings.Index(body, `data-task="`+id+`"`)
	if i < 0 {
		return ""
	}
	j := strings.LastIndex(body[:i], `data-col="`)
	m := regexp.MustCompile(`data-col="([a-z_]+)" aria-label`).FindAllStringSubmatch(body[:i], -1)
	_ = j
	if len(m) == 0 {
		return ""
	}
	return m[len(m)-1][1]
}

// P4 AC1: one card per task, in the right column, for a fixture of every status.
func TestBoardOneCardPerTaskInTheRightColumn(t *testing.T) {
	fx := newFix(t)
	r := fx.h.Get("/board")
	fx.h.CheckPage(r)
	want := map[string]string{"assigned": "assigned", "working": "in_progress", "stuck": "blocked", "waiting": "blocked", "review": "in_review",
		"merged": "merged", "cancelled": "cancelled", "reverted": "reverted", "changes": "in_progress", "done": "merged"}
	for role, col := range want {
		id := fx.ids[role]
		if n := strings.Count(r.Body, `data-task="`+id+`"`); n != 1 {
			t.Errorf("%s (%s): %d cards, want 1", role, id, n)
		}
		if got := colOf(r.Body, id); got != col {
			t.Errorf("%s (%s) is in column %q, want %q", role, id, got, col)
		}
	}
}

// P4 AC6 and focus-group AC12: waiting on (amber, no action) is not blocked (red, a problem),
// and the legend explains both plus p0..p3 and r1/r2.
func TestBoardSeparatesWaitingFromBlockedAndExplainsIt(t *testing.T) {
	fx := newFix(t)
	r := fx.h.Get("/board")
	card := func(id string) string {
		i := strings.Index(r.Body, `data-task="`+id+`"`)
		j := strings.Index(r.Body[i:], "</article>")
		return r.Body[i : i+j]
	}
	if c := card(fx.ids["waiting"]); !strings.Contains(c, "slai-chip-waiting") || !strings.Contains(c, "waiting on "+fx.ids["working"]) || strings.Contains(c, "slai-chip-blocked") {
		t.Errorf("waiting card:\n%s", c)
	}
	if c := card(fx.ids["stuck"]); !strings.Contains(c, "slai-chip-blocked") || strings.Contains(c, "slai-chip-waiting") {
		t.Errorf("stuck card:\n%s", c)
	}
	for _, w := range []string{"waiting on T-", "Nobody needs to act", "stuck on a problem", "p0", "p3", "higher first", "r1", "review rounds"} {
		if !strings.Contains(r.Body, w) {
			t.Errorf("legend lacks %q", w)
		}
	}
}

// P4 AC5: filters by assignee, text and needs-me; the state is in the URL.
func TestBoardFiltersLiveInTheURL(t *testing.T) {
	fx := newFix(t)
	r := fx.h.Get("/board?assignee=eng-2")
	if !strings.Contains(r.Body, fx.ids["stuck"]) || strings.Contains(r.Body, `data-task="`+fx.ids["assigned"]+`"`) {
		t.Errorf("assignee filter: %.300s", r.Body)
	}
	r = fx.h.Get("/board?q=Reverted")
	if strings.Count(r.Body, `data-task="`) != 1 || !strings.Contains(r.Body, fx.ids["reverted"]) {
		t.Errorf("text filter shows %d cards", strings.Count(r.Body, `data-task="`))
	}
	// needs me: eng-2 is waiting on the user, and the stuck task is blocked by a problem
	r = fx.h.Get("/board?needs=1")
	if strings.Count(r.Body, `data-task="`) != 1 || !strings.Contains(r.Body, fx.ids["stuck"]) {
		t.Errorf("needs-me shows %d cards", strings.Count(r.Body, `data-task="`))
	}
	// focus-group AC10: the toggle is a one-click button on the toolbar, and keeps the other filters
	r = fx.h.Get("/board?assignee=eng-2")
	if !strings.Contains(r.Body, `id="needs-toggle"`) || !strings.Contains(r.Body, `href="/board?assignee=eng-2&amp;needs=1"`) {
		t.Errorf("needs-me toggle missing or loses the filters: %s", regexp.MustCompile(`<a[^>]*needs-toggle[^>]*>`).FindString(r.Body))
	}
	// the refetch keeps the filter too
	f := fx.h.Get("/f/board?assignee=eng-2")
	if strings.Contains(f.Body, `data-task="`+fx.ids["assigned"]+`"`) || strings.Contains(f.Body, "<html") {
		t.Errorf("fragment is not the filtered columns alone")
	}
}

// Focus-group AC13: priority from the card menu, through the same function as `slai priority`.
func TestPriorityFromTheCardMenu(t *testing.T) {
	fx := newFix(t)
	id := fx.ids["assigned"]
	if r := fx.h.Get("/board"); !strings.Contains(r.Body, `hx-post="/tasks/`+id+`/priority"`) || !strings.Contains(r.Body, `value="p3"`) || !strings.Contains(r.Body, `value="top"`) {
		t.Fatal("the card menu has no priority buttons")
	}
	r := fx.post("/tasks/"+id+"/priority", url.Values{"spec": {"p2"}})
	if r.Code != 200 || !strings.Contains(r.Body, id+" priority is now p2") {
		t.Fatalf("p2: %d %s", r.Code, r.Body)
	}
	if tk, _ := fx.h.Store.GetTask(id); tk.Priority != 2 {
		t.Errorf("priority = %d, want 2", tk.Priority)
	}
	// top: above every other open task, exactly what `slai priority top` computes
	other := fx.ids["working"]
	_ = fx.h.Store.SetTaskPriority(other, 5)
	fx.post("/tasks/"+id+"/priority", url.Values{"spec": {"top"}})
	if tk, _ := fx.h.Store.GetTask(id); tk.Priority != 6 {
		t.Errorf("top = %d, want 6", tk.Priority)
	}
	if r := fx.post("/tasks/"+id+"/priority", url.Values{"spec": {"banana"}}); r.Code != 422 || !strings.Contains(r.Body, "priority must be") {
		t.Errorf("bad spec: %d %s", r.Code, r.Body)
	}
	if r := fx.post("/tasks/"+fx.ids["merged"]+"/priority", url.Values{"spec": {"p1"}}); r.Code != 422 {
		t.Errorf("a merged task has no priority to change: %d", r.Code)
	}
	if got := len(fx.h.WebActions()); got < 2 {
		t.Errorf("web_action events = %d", got)
	}
}

// P4 AC2/AC3: one POST per drop; reorder uses the priority function; cancel and unblock work;
// illegal drops are refused with the reason and change nothing.
func TestMoveAllowedAndRefused(t *testing.T) {
	fx := newFix(t)
	st := fx.h.Store
	// reorder: dropped above another card in the same column sets one more than it
	a, w := fx.ids["assigned"], fx.ids["working"]
	_ = st.SetTaskPriority(w, 3)
	// a neighbour from another column is not a reorder target
	r := fx.post("/tasks/"+a+"/move", url.Values{"to": {"assigned"}, "before": {w}})
	if r.Code != 422 || !strings.Contains(r.Body, "not in the same column") {
		t.Fatalf("neighbour from another column: %d %s", r.Code, r.Body)
	}
	// real reorder inside the in_progress column
	chg := fx.ids["changes"]
	r = fx.post("/tasks/"+chg+"/move", url.Values{"to": {"in_progress"}, "before": {w}})
	if r.Code != 200 {
		t.Fatalf("reorder: %d %s", r.Code, r.Body)
	}
	if tk, _ := st.GetTask(chg); tk.Priority != 4 {
		t.Errorf("dropped above p3 -> %d, want 4", tk.Priority)
	}
	// cancel by dropping on cancelled
	if r = fx.post("/tasks/"+a+"/move", url.Values{"to": {"cancelled"}}); r.Code != 200 {
		t.Fatalf("cancel: %d %s", r.Code, r.Body)
	}
	if tk, _ := st.GetTask(a); tk.Status != store.StatusCancelled {
		t.Errorf("status = %s", tk.Status)
	}
	// unblock: a task stuck on a problem goes back to its engineer
	s := fx.ids["stuck"]
	if r = fx.post("/tasks/"+s+"/move", url.Values{"to": {"in_progress"}}); r.Code != 200 {
		t.Fatalf("unblock: %d %s", r.Code, r.Body)
	}
	if tk, _ := st.GetTask(s); tk.Status != store.StatusInProgress {
		t.Errorf("status = %s", tk.Status)
	}
	// refused: nothing changes
	for _, c := range []struct{ id, to, want string }{
		{fx.ids["review"], "merged", "only the principal's approve"},
		{fx.ids["review"], "in_progress", "only the principal sends a review back"},
		{fx.ids["merged"], "assigned", "final"},
		{fx.ids["waiting"], "in_progress", "waiting on"},
		{fx.ids["working"], "blocked", "blocks itself"},
	} {
		before, _ := st.GetTask(c.id)
		r := fx.post("/tasks/"+c.id+"/move", url.Values{"to": {c.to}})
		if r.Code != 422 || !strings.Contains(html.UnescapeString(r.Body), c.want) {
			t.Errorf("%s -> %s: %d %q, want 422 with %q", c.id, c.to, r.Code, r.Body, c.want)
		}
		if after, _ := st.GetTask(c.id); after.Status != before.Status || after.Priority != before.Priority {
			t.Errorf("%s changed after a refused move: %+v -> %+v", c.id, before, after)
		}
	}
	// every POST is exactly one web_action; three of them (reorder, cancel, unblock) took effect
	ok := 0
	evs := fx.h.WebActions()
	for _, e := range evs {
		if e["status"] == float64(200) {
			ok++
		}
	}
	if ok != 3 || len(evs) != 3+6 { // 3 accepted + 5 refused moves + the neighbour from another column
		t.Errorf("web_action events: %d total, %d accepted; want 9 and 3", len(evs), ok)
	}
}

// Focus-group AC14: the refusal names who may do what, for every illegal pair.
func TestMoveVerdictTableNamesWhoMayDoWhat(t *testing.T) {
	cols := []string{"assigned", "in_progress", "blocked", "in_review", "merged", "cancelled", "reverted"}
	for _, from := range cols {
		for _, to := range cols {
			v := board.MoveVerdict("T-1", from, to, nil)
			switch {
			case from == to:
				if !v.OK || v.Kind != board.MoveReorder {
					t.Errorf("%s->%s: %+v", from, to, v)
				}
			case v.OK:
				if !(to == "cancelled" && from != "merged" && from != "reverted" && from != "cancelled") && !(from == "blocked" && (to == "assigned" || to == "in_progress")) {
					t.Errorf("%s->%s must be refused", from, to)
				}
			default:
				if !strings.Contains(v.Reason, "You can: cancel, unblock, reorder.") {
					t.Errorf("%s->%s reason does not say what the user can do: %q", from, to, v.Reason)
				}
				if v.Reason == "" {
					t.Errorf("%s->%s has no reason", from, to)
				}
			}
		}
	}
	if r := board.MoveVerdict("T-1", "in_review", "merged", nil).Reason; !strings.Contains(r, "principal's approve") {
		t.Errorf("merged reason: %q", r)
	}
	if r := board.MoveVerdict("T-1", "in_progress", "in_review", nil).Reason; !strings.Contains(r, "engineer calls request_review") {
		t.Errorf("in_review reason: %q", r)
	}
	if r := board.MoveVerdict("T-1", "merged", "reverted", nil).Reason; !strings.Contains(r, "slai revert T-1") {
		t.Errorf("final reason: %q", r)
	}
}

// P4 AC4: the create form calls create_task with every field, and a service error is shown verbatim.
func TestCreateTaskThroughTheForm(t *testing.T) {
	fx := newFix(t)
	dep := fx.ids["working"]
	r := fx.post("/tasks", url.Values{"title": {"New <b>thing</b>"}, "description": {"do it"}, "assignee": {"eng-2"}, "depends_on": {dep + ", "}, "files": {"a.go b.go"}, "spec_path": {".slai/specs/x.md"}})
	if r.Code != 200 || !strings.Contains(r.Body, "created and assigned to eng-2") {
		t.Fatalf("create: %d %s", r.Code, r.Body)
	}
	tasks, _ := fx.h.Store.ListTasks("")
	nt := tasks[len(tasks)-1]
	if nt.Title != "New <b>thing</b>" || nt.Assignee != "eng-2" || nt.CreatedBy != "user" || nt.SpecPath != ".slai/specs/x.md" || len(nt.DependsOn) != 1 || nt.DependsOn[0] != dep {
		t.Errorf("task = %+v", nt)
	}
	if !strings.Contains(nt.Files, "a.go") || !strings.Contains(nt.Files, "b.go") {
		t.Errorf("files = %q", nt.Files)
	}
	// unknown dependency: the service's own words
	r = fx.post("/tasks", url.Values{"title": {"x"}, "assignee": {"eng-2"}, "depends_on": {"T-9999"}})
	if r.Code != 422 || !strings.Contains(r.Body, "depends_on T-9999") {
		t.Errorf("unknown id: %d %s", r.Code, r.Body)
	}
	// a non-worker assignee, again verbatim
	r = fx.post("/tasks", url.Values{"title": {"x"}, "assignee": {"orch"}})
	if r.Code != 422 || !strings.Contains(r.Body, "cannot work a task") {
		t.Errorf("bad assignee: %d %s", r.Code, r.Body)
	}
	// nothing half-made is left behind by the failed ones
	after, _ := fx.h.Store.ListTasks("")
	for _, tk := range after[len(tasks):] {
		if tk.Status != store.StatusCancelled {
			t.Errorf("failed create left %s %s", tk.ID, tk.Status)
		}
	}
}

// --read-only: the page shows no write control and every write is refused.
func TestReadOnlyBoardHasNoWriteControls(t *testing.T) {
	fx := newFix(t, webtest.ReadOnly())
	r := fx.h.Get("/board")
	fx.h.CheckPage(r)
	for _, bad := range []string{`hx-post=`, "New task", "slai-menu"} {
		if strings.Contains(r.Body, bad) {
			t.Errorf("read-only board contains %q", bad)
		}
	}
	before, _ := fx.h.Store.GetTask(fx.ids["assigned"])
	if r := fx.post("/tasks/"+fx.ids["assigned"]+"/cancel", nil); r.Code != 403 {
		t.Errorf("cancel in read-only: %d", r.Code)
	}
	if after, _ := fx.h.Store.GetTask(fx.ids["assigned"]); after.Status != before.Status {
		t.Error("a refused write changed the task")
	}
}

// Focus-group AC17: nothing prototype-only reaches the real UI.
func TestNoPrototypeControlsInTemplates(t *testing.T) {
	for _, dir := range []string{"../templates/board", "../templates/task"} {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
			if strings.Contains(string(b), "Demo") || strings.Contains(string(b), "Show an error state") {
				t.Errorf("%s/%s carries a prototype-only control", dir, e.Name())
			}
		}
	}
}

// Security AC5: a golden file pins the board markup for the seeded fixture.
func TestBoardMarkupGolden(t *testing.T) {
	fx := newFix(t)
	got := fx.h.Get("/f/board").Body
	got = regexp.MustCompile(`\n\s*\n+`).ReplaceAllString(got, "\n")
	path := filepath.Join("testdata", "board.golden.html")
	if *update {
		os.MkdirAll("testdata", 0o755)
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/web/board -update)", err)
	}
	if got != string(want) {
		t.Errorf("board markup changed; run go test ./internal/web/board -update and review the diff\n got %d bytes, want %d", len(got), len(want))
	}
}

// Agent text is data: titles and descriptions render escaped on both pages.
func TestAgentTextIsEscaped(t *testing.T) {
	fx := newFix(t)
	evil := `<script>alert(1)</script>`
	tk, _ := fx.h.Store.CreateTask(store.Task{Title: evil, Description: evil, Assignee: "eng-1"})
	for _, p := range []string{"/board", "/tasks/" + tk.ID} {
		r := fx.h.Get(p)
		fx.h.CheckPage(r)
		if strings.Contains(r.Body, evil) {
			t.Errorf("%s renders the script unescaped", p)
		}
		if !strings.Contains(r.Body, "&lt;script&gt;") {
			t.Errorf("%s lost the text", p)
		}
	}
}

var _ = errors.New

// A long run does not make the merged column hundreds of cards tall: the newest few show, the
// rest sit behind a link, and ?all=1 shows everything.
func TestFinishedColumnsFoldTheOldestCards(t *testing.T) {
	fx := newFix(t)
	for i := 0; i < 20; i++ {
		id := mustTask(t, fx.h.Store, fmt.Sprintf("Old %02d", i), "eng-1")
		step(t, fx.h.Store, id, store.StatusInProgress, store.StatusInReview, store.StatusMerged)
	}
	r := fx.h.Get("/board")
	merged := between(r.Body, `data-col="merged" aria-label`, "</section>")
	if n := strings.Count(merged, `data-task="`); n != 12 {
		t.Errorf("merged shows %d cards, want 12", n)
	}
	if !strings.Contains(merged, "Show 10 older") || !strings.Contains(merged, `href="/board?all=1"`) {
		t.Errorf("no way to see the rest: %s", between(merged, "slai-col-more", "</p>"))
	}
	r = fx.h.Get("/board?all=1")
	merged = between(r.Body, `data-col="merged" aria-label`, "</section>")
	if n := strings.Count(merged, `data-task="`); n != 22 || !strings.Contains(merged, "Show fewer") {
		t.Errorf("all=1 shows %d cards", n)
	}
	// open columns are never folded
	if open := between(r.Body, `data-col="assigned" aria-label`, "</section>"); strings.Contains(open, "slai-col-more") {
		t.Error("an open column has a fold link")
	}
}

// "needs you" follows the task's own state, not the assignee's: an agent waiting on the user
// puts the badge on the task it is working on and on tasks blocked by a problem, never on its
// merged, cancelled or reverted cards, nor on its other tasks.
func TestNeedsYouBadgeFollowsTheTasksOwnState(t *testing.T) {
	fx := newFix(t)
	st := fx.h.Store
	// eng-2 (waiting_on_user in the fixture) already owns the blocked-by-a-problem task
	current := mustTask(t, st, "What eng-2 is doing", "eng-2")
	step(t, st, current, store.StatusInProgress)
	other := mustTask(t, st, "Another of eng-2's tasks", "eng-2")
	step(t, st, other, store.StatusInProgress)
	merged := mustTask(t, st, "Merged long ago", "eng-2")
	step(t, st, merged, store.StatusInProgress, store.StatusInReview, store.StatusMerged)
	cancelled := mustTask(t, st, "Dropped", "eng-2")
	step(t, st, cancelled, store.StatusCancelled)
	reverted := mustTask(t, st, "Taken back", "eng-2")
	step(t, st, reverted, store.StatusInProgress, store.StatusInReview, store.StatusMerged)
	if err := st.MarkReverted(reverted); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAgentTask("eng-2", current); err != nil {
		t.Fatal(err)
	}
	body := fx.h.Get("/board").Body
	card := func(id string) string {
		i := strings.Index(body, `data-task="`+id+`"`)
		j := strings.Index(body[i:], "</article>")
		return body[i : i+j]
	}
	badge := func(id string) bool { return strings.Contains(card(id), "slai-chip-user") }
	want := map[string]bool{fx.ids["stuck"]: true, current: true, other: false, merged: false, cancelled: false, reverted: false}
	for id, w := range want {
		if badge(id) != w {
			t.Errorf("%s: badge = %v, want %v", id, badge(id), w)
		}
	}
	// the needs-me filter agrees with the badges
	r := fx.h.Get("/board?needs=1")
	for id, w := range want {
		if got := strings.Contains(r.Body, `data-task="`+id+`"`); got != w {
			t.Errorf("needs=1 lists %s: %v, want %v", id, got, w)
		}
	}
}

// At phone width the page must not scroll sideways: the columns scroll inside the board, and
// the board is the containing block of its absolutely positioned bits (the screen-reader-only
// dot labels), which would otherwise sit far to the right and widen the whole page.
func TestBoardCSSKeepsAbsoluteLabelsInsideTheScrollingBoard(t *testing.T) {
	b, err := assets.FS.ReadFile("slai.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	m := regexp.MustCompile(`(?s)\.slai-board \{([^}]*)\}`).FindStringSubmatch(css)
	if m == nil || !strings.Contains(m[1], "position: relative") {
		t.Errorf(".slai-board rule must set position: relative, got %q", m)
	}
	if !regexp.MustCompile(`(?s)@media \(max-width: 575\.98px\) \{ \.slai-board \{[^}]*overflow-x: auto`).MatchString(css) {
		t.Error("the board must scroll inside itself at phone width")
	}
}

// Focus group: the card's "waiting on" shows every id in full (wrapping, never truncating), and
// the review-round badge says what it is on hover.
func TestCardShowsEveryWaitingIdAndExplainsTheRoundBadge(t *testing.T) {
	fx := newFix(t)
	st := fx.h.Store
	var deps []string
	for i := 0; i < 3; i++ {
		d := mustTask(t, st, fmt.Sprintf("Dependency %d", i), "eng-1")
		step(t, st, d, store.StatusInProgress)
		deps = append(deps, d)
	}
	waiter := mustTask(t, st, "Waits for three", "eng-1")
	if err := st.SetTaskDeps(waiter, deps); err != nil {
		t.Fatal(err)
	}
	step(t, st, waiter, store.StatusBlocked)
	rounds := mustTask(t, st, "Sent back twice", "eng-1")
	step(t, st, rounds, store.StatusInProgress, store.StatusInReview, store.StatusChangesRequested, store.StatusInProgress, store.StatusInReview, store.StatusChangesRequested)
	body := fx.h.Get("/board").Body
	card := func(id string) string {
		i := strings.Index(body, `data-task="`+id+`"`)
		return body[i : i+strings.Index(body[i:], "</article>")]
	}
	w := card(waiter)
	want := "waiting on " + strings.Join(deps, ", ")
	if !strings.Contains(w, want) {
		t.Errorf("the card does not list every id in full (%q):\n%s", want, w)
	}
	if strings.Contains(w, "…") {
		t.Error("the waiting-on chip is truncated")
	}
	if r := card(rounds); !strings.Contains(r, `title="review round 2"`) || !strings.Contains(r, ">r2<") {
		t.Errorf("the round badge has no 'review round 2' tooltip:\n%s", r)
	}
	// the stylesheet lets the chip wrap instead of clipping it
	b, _ := assets.FS.ReadFile("slai.css")
	m := regexp.MustCompile(`\.slai-card-meta \.slai-chip-waiting \{([^}]*)\}`).FindStringSubmatch(string(b))
	if m == nil || !strings.Contains(m[1], "white-space: normal") || strings.Contains(m[1], "ellipsis") || strings.Contains(m[1], "nowrap") {
		t.Errorf("the waiting chip must wrap, got %q", m)
	}
	// and the legend's example is not itself cut off
	if strings.Contains(body, "waiting on T-…") {
		t.Error("the legend example is truncated")
	}
}
