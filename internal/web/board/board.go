package board

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/web/server"
	"github.com/oorbt/slai/internal/web/templates"
)

// RefreshTrigger is the hx-trigger of the board's safety refetch. The SSE hub (W2) widens
// it with a topic event once it exists; until then the board refetches every 30 s, as the
// spec's fallback says.
var RefreshTrigger = "every 30s"

type handler struct {
	d *server.Deps
}

// Module serves the board and the task detail page.
func Module(d *server.Deps) []server.Route {
	h := &handler{d: d}
	return []server.Route{
		{Method: "GET", Pattern: "/board", Calls: "reads tasks, dependencies and agents from the store", Handler: h.page},
		{Method: "GET", Pattern: "/f/board", Calls: "reads tasks, dependencies and agents from the store (the board's refetch)", Handler: h.fragment},
		{Method: "POST", Pattern: "/tasks", Action: "create_task", Target: func(*http.Request) string { return "" },
			Calls: "the service's create_task, called as the user (the orchestrator's tool, same checks)", Handler: h.create},
		{Method: "POST", Pattern: "/tasks/{id}/priority", Action: "priority",
			Calls: "Supervisor.SetPriority, the function `slai priority` calls", Handler: h.priority},
		{Method: "POST", Pattern: "/tasks/{id}/cancel", Action: "cancel_task",
			Calls: "the service's cancel_task, called as the user", Handler: h.cancel},
		{Method: "POST", Pattern: "/tasks/{id}/unblock", Action: "unblock_task",
			Calls: "the service's update_task (status in_progress on a blocked task), called as the user", Handler: h.unblock},
		{Method: "POST", Pattern: "/tasks/{id}/move", Action: "move_task",
			Calls: "the board's move rules, then Supervisor.SetPriority (reorder), cancel_task (drop on cancelled) or update_task (unblock); every other move is refused", Handler: h.move},
		{Method: "POST", Pattern: "/tasks/{id}/redirect", Action: "redirect",
			Calls: "supervisor.RequestRedirect, the TUI's `c` key path; the supervisor re-checks the pane before pressing Escape", Handler: h.redirect},
		{Method: "GET", Pattern: "/tasks/{id}", Calls: "reads the task, its claims, events and messages, the spec file under .slai/specs/ and the branch diff (read-only)", Handler: h.task},
	}
}

func filterOf(q url.Values) Filter {
	return Filter{Assignee: strings.TrimSpace(q.Get("assignee")), Text: strings.TrimSpace(q.Get("q")), NeedsMe: q.Get("needs") == "1", All: q.Get("all") == "1"}
}

func (f Filter) values() url.Values {
	v := url.Values{}
	if f.Assignee != "" {
		v.Set("assignee", f.Assignee)
	}
	if f.Text != "" {
		v.Set("q", f.Text)
	}
	if f.NeedsMe {
		v.Set("needs", "1")
	}
	if f.All {
		v.Set("all", "1")
	}
	return v
}

// Query is the filter as a query string (no leading ?), the part of the URL that makes a view
// bookmarkable.
func (f Filter) Query() string { return f.values().Encode() }

// ToggleHref is the board URL with the needs-me filter flipped and the others kept (the
// toolbar's one-click button).
func (f Filter) ToggleHref() string {
	f.NeedsMe = !f.NeedsMe
	return f.Href()
}

// Href is the board URL for this filter.
func (f Filter) Href() string {
	if q := f.Query(); q != "" {
		return "/board?" + q
	}
	return "/board"
}

func (h *handler) view(f Filter) (BoardView, error) {
	v, err := buildBoard(h.d.Store, f)
	v.View = f.Query()
	v.Refresh = RefreshTrigger
	v.ReadOnly = h.d.ReadOnly
	return v, err
}

type pageData struct {
	BoardView
	Assignees []string
	Legend    []legendItem
}

type legendItem struct{ Chip, Class, Text string }

var legend = []legendItem{
	{"waiting on T-0083", "waiting", "amber: dependencies that have not merged yet. Nobody needs to act; it starts by itself."},
	{"blocked", "blocked", "red: stuck on a problem. An engineer asked for something or a human must decide: see the Inbox."},
	{"p0 … p3", "prio", "priority, higher first. p0 is the default; p2 and above are highlighted. Change it from the card’s menu."},
	{"r1, r2", "round", "review rounds so far (how many times it was sent to the principal)."},
}

func (h *handler) assignees() []string {
	var out []string
	if t := h.d.Team(); t != nil {
		for _, a := range t.Agents {
			if a.Role == config.RoleEngineer || a.Role == config.RolePM {
				out = append(out, a.ID)
			}
		}
	}
	return out
}

func (h *handler) page(w http.ResponseWriter, r *http.Request) {
	f := filterOf(r.URL.Query())
	v, err := h.view(f)
	if err != nil {
		h.d.Fail(w, r, err)
		return
	}
	tpl, err := h.d.Template("board")
	if err != nil {
		h.d.Fail(w, r, err)
		return
	}
	h.d.Render(w, r, tpl, templates.Page{Title: "Board", Active: "board", Data: pageData{BoardView: v, Assignees: h.assignees(), Legend: legend}})
}

// fragment is the columns alone, for the htmx refetch.
func (h *handler) fragment(w http.ResponseWriter, r *http.Request) {
	v, err := h.view(filterOf(r.URL.Query()))
	if err != nil {
		h.d.Fail(w, r, err)
		return
	}
	h.exec(w, r, "board-columns", v, http.StatusOK)
}

func (h *handler) exec(w http.ResponseWriter, r *http.Request, name string, data any, code int) {
	tpl, err := h.d.Template("board")
	if err != nil {
		h.d.Fail(w, r, err)
		return
	}
	var b bytes.Buffer
	if err := tpl.ExecuteTemplate(&b, name, data); err != nil {
		h.d.Fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	w.Write(b.Bytes())
}

type resultData struct {
	Msg   string
	Board BoardView
}

// ok answers a successful write: the message plus the refreshed columns (out of band) for
// htmx, a redirect back to the board for a plain form post.
func (h *handler) ok(w http.ResponseWriter, r *http.Request, msg string) {
	q := url.Values{}
	if v := r.PostFormValue("view"); v != "" {
		q, _ = url.ParseQuery(v)
	}
	f := filterOf(q)
	if r.PostFormValue("ret") == "task" { // from the task page: no board to refresh
		if r.Header.Get("HX-Request") == "true" {
			h.d.OK(w, r, msg, "")
			return
		}
		http.Redirect(w, r, "/tasks/"+r.PathValue("id"), http.StatusSeeOther)
		return
	}
	if r.Header.Get("HX-Request") != "true" {
		back := "/board"
		if s := f.Query(); s != "" {
			back += "?" + s
		}
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	v, err := h.view(f)
	if err != nil {
		h.d.Fail(w, r, err)
		return
	}
	h.exec(w, r, "write-result", resultData{Msg: msg, Board: v}, http.StatusOK)
}

func (h *handler) actions() (Actions, error) {
	a, ok := h.d.Actions.(Actions)
	if !ok || a == nil {
		return nil, errNoActions
	}
	return a, nil
}

func (h *handler) taskOf(r *http.Request) (store.Task, error) {
	id := r.PathValue("id")
	t, err := h.d.Store.GetTask(id)
	if errors.Is(err, store.ErrNotFound) {
		return t, fmt.Errorf("task %s not found", id)
	}
	return t, err
}

func (h *handler) priority(w http.ResponseWriter, r *http.Request) {
	t, err := h.taskOf(r)
	if err != nil {
		h.d.Fail(w, r, err)
		return
	}
	spec, err := PriorityValue(r.PostFormValue("spec"))
	if err != nil {
		h.d.Fail(w, r, err)
		return
	}
	h.setPriority(w, r, t, spec)
}

func (h *handler) setPriority(w http.ResponseWriter, r *http.Request, t store.Task, spec string) {
	if isFinal(ColumnOf(t.Status)) {
		h.d.Fail(w, r, fmt.Errorf("%s is %s: priority only orders open tasks", t.ID, t.Status))
		return
	}
	a, err := h.actions()
	if err != nil {
		h.d.Fail(w, r, err)
		return
	}
	p, err := a.SetPriority(t.ID, spec)
	if err != nil {
		h.d.Fail(w, r, err)
		return
	}
	h.ok(w, r, fmt.Sprintf("%s priority is now p%d (same as `slai priority %s %s`).", t.ID, p, t.ID, spec))
}

func (h *handler) cancel(w http.ResponseWriter, r *http.Request) {
	t, err := h.taskOf(r)
	if err != nil {
		h.d.Fail(w, r, err)
		return
	}
	h.cancelTask(w, r, t, strings.TrimSpace(r.PostFormValue("reason")))
}

func (h *handler) cancelTask(w http.ResponseWriter, r *http.Request, t store.Task, reason string) {
	if v := MoveVerdict(t.ID, ColumnOf(t.Status), "cancelled", nil); !v.OK {
		h.d.Fail(w, r, errors.New(v.Reason))
		return
	}
	a, err := h.actions()
	if err != nil {
		h.d.Fail(w, r, err)
		return
	}
	if reason == "" {
		reason = "cancelled from the web UI"
	}
	if err := a.CancelTask(t.ID, reason); err != nil {
		h.d.Fail(w, r, err)
		return
	}
	h.ok(w, r, t.ID+" cancelled. Its engineer is told; the worktree is left alone.")
}

func (h *handler) unblock(w http.ResponseWriter, r *http.Request) {
	t, err := h.taskOf(r)
	if err != nil {
		h.d.Fail(w, r, err)
		return
	}
	h.unblockTask(w, r, t, strings.TrimSpace(r.PostFormValue("note")))
}

func (h *handler) unblockTask(w http.ResponseWriter, r *http.Request, t store.Task, note string) {
	waiting, _ := h.d.Store.WaitingOn(t.ID)
	if t.Status != store.StatusBlocked {
		h.d.Fail(w, r, fmt.Errorf("%s is %s, not blocked: there is nothing to unblock", t.ID, t.Status))
		return
	}
	if v := MoveVerdict(t.ID, "blocked", "in_progress", waiting); !v.OK {
		h.d.Fail(w, r, errors.New(v.Reason))
		return
	}
	a, err := h.actions()
	if err != nil {
		h.d.Fail(w, r, err)
		return
	}
	if err := a.UnblockTask(t.ID, note); err != nil {
		h.d.Fail(w, r, err)
		return
	}
	h.ok(w, r, t.ID+" unblocked and handed back to its engineer.")
}

// move is a drag: exactly one POST per drop. The verdict is the same table the page's legend
// describes; a refused move answers 422 with the reason and changes nothing, and the page
// puts the card back.
func (h *handler) move(w http.ResponseWriter, r *http.Request) {
	t, err := h.taskOf(r)
	if err != nil {
		h.d.Fail(w, r, err)
		return
	}
	from, to := ColumnOf(t.Status), strings.TrimSpace(r.PostFormValue("to"))
	waiting, _ := h.d.Store.WaitingOn(t.ID)
	v := MoveVerdict(t.ID, from, to, waiting)
	if !v.OK {
		h.d.Fail(w, r, errors.New(v.Reason))
		return
	}
	switch v.Kind {
	case MoveCancel:
		h.cancelTask(w, r, t, "")
	case MoveUnblock:
		h.unblockTask(w, r, t, "")
	default:
		spec, err := h.reorderSpec(t, r.PostFormValue("before"), r.PostFormValue("after"))
		if err != nil {
			h.d.Fail(w, r, err)
			return
		}
		if spec == "" { // dropped where it was
			h.ok(w, r, t.ID+" stays where it is.")
			return
		}
		h.setPriority(w, r, t, spec)
	}
}

// reorderSpec turns "dropped above before" / "dropped below after" into a priority number.
// Priorities are plain numbers (higher first), so a drop above X means one more than X, and
// a drop below X means one less. The same function as `slai priority` applies it.
func (h *handler) reorderSpec(t store.Task, before, after string) (string, error) {
	switch {
	case before != "":
		o, err := h.d.Store.GetTask(before)
		if err != nil {
			return "", fmt.Errorf("task %s not found", before)
		}
		if ColumnOf(o.Status) != ColumnOf(t.Status) {
			return "", fmt.Errorf("%s is not in the same column as %s", o.ID, t.ID)
		}
		if o.ID == t.ID || t.Priority > o.Priority {
			return "", nil
		}
		return fmt.Sprint(o.Priority + 1), nil
	case after != "":
		o, err := h.d.Store.GetTask(after)
		if err != nil {
			return "", fmt.Errorf("task %s not found", after)
		}
		if ColumnOf(o.Status) != ColumnOf(t.Status) {
			return "", fmt.Errorf("%s is not in the same column as %s", o.ID, t.ID)
		}
		if o.ID == t.ID || t.Priority < o.Priority {
			return "", nil
		}
		return fmt.Sprint(o.Priority - 1), nil
	}
	return "", nil
}

func (h *handler) create(w http.ResponseWriter, r *http.Request) {
	a, err := h.actions()
	if err != nil {
		h.d.Fail(w, r, err)
		return
	}
	args := CreateArgs{
		Title: strings.TrimSpace(r.PostFormValue("title")), Description: strings.TrimSpace(r.PostFormValue("description")),
		Assignee: strings.TrimSpace(r.PostFormValue("assignee")), SpecPath: strings.TrimSpace(r.PostFormValue("spec_path")),
		DependsOn: splitList(r.PostFormValue("depends_on")), Files: splitList(r.PostFormValue("files")),
	}
	id, err := a.CreateTask(args)
	if err != nil {
		h.d.Fail(w, r, err)
		return
	}
	h.ok(w, r, fmt.Sprintf("%s created and assigned to %s.", id, args.Assignee))
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
