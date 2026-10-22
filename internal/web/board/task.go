package board

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/web/templates"
)

// maxDiffFilesOpen is how many files of a diff start expanded when the diff is large: past it
// only the first file opens.
const maxDiffFilesOpen = 30

type depView struct {
	ID, Status string
	Merged     bool
}

type acRow struct {
	AC, ClaimNote, Verdict, VerdictNote, By string
	Claimed                                 bool
}

type tlRow struct {
	At, Agent, Kind, Detail, Cause string
}

type msgRow struct {
	At, From, To, Kind, Body string
}

type diffLine struct{ Kind, Text string }

type diffFile struct {
	Name     string
	Add, Del int
	Open     bool
	Lines    []diffLine
}

// DiffView is a unified diff split per file.
type DiffView struct {
	Files     []diffFile
	Against   string
	Truncated bool
}

type taskPage struct {
	Task                          store.Task
	StatusLabel, Col, Prio        string
	ChangesRequested, DoneNonCode bool
	Waiting                       []string
	DependsOn                     []depView
	WaitedOnBy                    []string
	Round                         int
	Summary                       string
	ACs                           []acRow
	Spec                          SpecView
	Diff                          *DiffView
	DiffNote                      string
	Timeline                      []tlRow
	Messages                      []msgRow
	CanCancel, CanUnblock         bool
	AssigneeDot, AssigneeText     string
	RedirectShown, RedirectOK     bool
	RedirectWhy                   string
	ReadOnly                      bool
}

func (h *handler) task(w http.ResponseWriter, r *http.Request) {
	t, err := h.taskOf(r)
	if err != nil {
		h.d.Fail(w, r, err)
		return
	}
	st := h.d.Store
	col := ColumnOf(t.Status)
	p := taskPage{Task: t, Col: col, StatusLabel: statusLabel(t.Status), Prio: PrioLabel(t.Priority), Round: t.ReviewRounds,
		ChangesRequested: t.Status == store.StatusChangesRequested, DoneNonCode: t.Status == store.StatusDone, ReadOnly: h.d.ReadOnly}
	if t.Status == store.StatusBlocked {
		p.Waiting, _ = st.WaitingOn(t.ID)
	}
	p.CanCancel = !isFinal(col)
	p.CanUnblock = t.Status == store.StatusBlocked && len(p.Waiting) == 0
	for _, dep := range t.DependsOn {
		dt, err := st.GetTask(dep)
		if err != nil {
			p.DependsOn = append(p.DependsOn, depView{ID: dep, Status: "unknown"})
			continue
		}
		p.DependsOn = append(p.DependsOn, depView{ID: dep, Status: dt.Status, Merged: dt.Status == store.StatusMerged})
	}
	if all, err := st.ListTasks(""); err == nil {
		for _, o := range all {
			for _, d := range o.DependsOn {
				if d == t.ID {
					p.WaitedOnBy = append(p.WaitedOnBy, o.ID)
				}
			}
		}
	}
	p.ACs, p.Summary = h.acTable(t)
	p.Spec = ReadSpec(h.d.Dir, t.SpecPath)
	h.fillDiff(r.Context(), &p, t)
	h.fillTimeline(&p, t)
	h.fillRedirect(&p, t)
	tpl, err := h.d.Template("task")
	if err != nil {
		h.d.Fail(w, r, err)
		return
	}
	h.d.Render(w, r, tpl, templates.Page{Title: t.ID + " · " + t.Title, Active: "board", Data: p})
}

func statusLabel(s string) string {
	if s == store.StatusChangesRequested {
		return "changes requested"
	}
	return strings.ReplaceAll(s, "_", " ")
}

// acTable joins the latest claims with the latest verdicts and writes the one-line summary
// ("2 of 3 criteria pass, 1 unanswered").
func (h *handler) acTable(t store.Task) ([]acRow, string) {
	_, claims, err := h.d.Store.LatestACClaims(t.ID)
	if err != nil || len(claims) == 0 {
		return nil, "No acceptance criteria have been claimed yet."
	}
	verdicts, _ := h.d.Store.LatestACVerdicts(t.ID)
	byKey := map[string]store.ACVerdict{}
	for _, v := range verdicts {
		byKey[store.ACKey(v.AC)] = v
	}
	var rows []acRow
	pass, fail, open := 0, 0, 0
	for _, c := range claims {
		row := acRow{AC: c.AC, Claimed: c.Met, ClaimNote: c.Note, Verdict: "unanswered"}
		if v, ok := byKey[store.ACKey(c.AC)]; ok {
			row.VerdictNote, row.By = v.Note, v.By
			if v.Pass {
				row.Verdict = "pass"
				pass++
			} else {
				row.Verdict = "fail"
				fail++
			}
		} else {
			open++
		}
		rows = append(rows, row)
	}
	s := fmt.Sprintf("%d of %d criteria pass", pass, len(claims))
	if fail > 0 {
		s += fmt.Sprintf(", %d fail", fail)
	}
	if open > 0 {
		s += fmt.Sprintf(", %d unanswered", open)
	}
	return rows, s
}

func (h *handler) fillDiff(ctx context.Context, p *taskPage, t store.Task) {
	if t.Status != store.StatusInReview {
		p.DiffNote = "The diff is shown while a task is in review."
		return
	}
	d, ok := h.d.Actions.(Differ)
	if !ok || d == nil {
		p.DiffNote = "This server cannot read the repository, so the diff is not shown."
		return
	}
	res, err := d.TaskDiff(ctx, t)
	if err != nil {
		p.DiffNote = "The diff could not be read: " + err.Error()
		return
	}
	if res == nil || strings.TrimSpace(res.Patch) == "" {
		p.DiffNote = "There are no changes on the branch yet."
		return
	}
	p.Diff = parseDiff(res)
}

// parseDiff splits a unified patch per file. Every line is data: the template escapes it.
func parseDiff(d *Diff) *DiffView {
	v := &DiffView{Against: d.Base, Truncated: d.Truncated}
	var cur *diffFile
	for _, l := range strings.Split(d.Patch, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			name := l
			if i := strings.LastIndex(l, " b/"); i >= 0 {
				name = l[i+3:]
			}
			v.Files = append(v.Files, diffFile{Name: name})
			cur = &v.Files[len(v.Files)-1]
		case cur == nil:
		case strings.HasPrefix(l, "@@"):
			cur.Lines = append(cur.Lines, diffLine{"hunk", l})
		case strings.HasPrefix(l, "+++") || strings.HasPrefix(l, "---") || strings.HasPrefix(l, "index ") || strings.HasPrefix(l, "new file") || strings.HasPrefix(l, "deleted file") || strings.HasPrefix(l, "similarity") || strings.HasPrefix(l, "rename "):
			cur.Lines = append(cur.Lines, diffLine{"meta", l})
		case strings.HasPrefix(l, "+"):
			cur.Add++
			cur.Lines = append(cur.Lines, diffLine{"add", l})
		case strings.HasPrefix(l, "-"):
			cur.Del++
			cur.Lines = append(cur.Lines, diffLine{"del", l})
		default:
			cur.Lines = append(cur.Lines, diffLine{"ctx", l})
		}
	}
	for i := range v.Files {
		v.Files[i].Open = len(v.Files) <= maxDiffFilesOpen || i == 0
	}
	return v
}

func (h *handler) fillTimeline(p *taskPage, t store.Task) {
	evs, _ := h.d.Store.TaskEvents(t.ID, 200)
	for i := len(evs) - 1; i >= 0; i-- { // newest first
		e := evs[i]
		row := tlRow{At: time.Unix(e.At, 0).Format("2006-01-02 15:04"), Agent: e.Agent, Kind: e.Kind}
		var m map[string]any
		if json.Unmarshal([]byte(e.Data), &m) == nil {
			if c, ok := m["cause"].(string); ok {
				row.Cause = clip(c, 300)
			}
			for _, k := range []string{"summary", "reason", "notes", "comments", "reason_text", "message"} {
				if s, ok := m[k].(string); ok && s != "" {
					row.Detail = clip(s, 300)
					break
				}
			}
		}
		p.Timeline = append(p.Timeline, row)
	}
	msgs, _ := h.d.Store.MessagesForTask(t.ID)
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		p.Messages = append(p.Messages, msgRow{At: time.Unix(m.CreatedAt, 0).Format("2006-01-02 15:04"), From: m.From, To: m.To, Kind: m.Kind, Body: clip(m.Body, 400)})
	}
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// redirectBlock says why a course correction cannot go to a now, "" when it can: the same
// rules the TUI's `c` key applies before it queues anything. The supervisor still re-checks
// the pane (and refuses over a permission dialog) before it presses a key.
func redirectBlock(a store.Agent, held bool) string {
	switch {
	case held:
		return a.ID + " is paused: resume it first"
	case a.Status == "waiting_on_user":
		return a.ID + " is waiting for your answer: answer it from the Inbox instead"
	case a.Status != "working":
		return a.ID + " is not busy (" + strings.ReplaceAll(a.Status, "_", " ") + "): send it an ordinary message instead"
	}
	return ""
}

func (h *handler) agentOf(id string) (store.Agent, bool) {
	agents, _ := h.d.Store.GetAgents()
	for _, a := range agents {
		if a.ID == id {
			return a, true
		}
	}
	return store.Agent{}, false
}

func (h *handler) fillRedirect(p *taskPage, t store.Task) {
	if isFinal(ColumnOf(t.Status)) || t.Assignee == "" {
		return
	}
	a, ok := h.agentOf(t.Assignee)
	if !ok || a.Status == "dead" || a.Status == "errored" || a.Status == "" {
		return // no live assignee: no button
	}
	p.AssigneeDot, p.AssigneeText = dot(a.Status)
	_, isR := h.d.Actions.(Redirector)
	held, _ := h.d.Store.PausedAgents()
	p.RedirectShown = isR && !h.d.ReadOnly
	if why := redirectBlock(a, held[a.ID]); why != "" {
		p.RedirectWhy = why
		return
	}
	p.RedirectOK = true
}

func (h *handler) redirect(w http.ResponseWriter, r *http.Request) {
	t, err := h.taskOf(r)
	if err != nil {
		h.d.Fail(w, r, err)
		return
	}
	text := strings.TrimSpace(r.PostFormValue("text"))
	if text == "" {
		h.d.Fail(w, r, errors.New("write the correction first"))
		return
	}
	if err := service.CheckText("text", text); err != nil {
		h.d.Fail(w, r, err)
		return
	}
	a, ok := h.agentOf(t.Assignee)
	if !ok {
		h.d.Fail(w, r, fmt.Errorf("%s has no live assignee to redirect", t.ID))
		return
	}
	held, _ := h.d.Store.PausedAgents()
	if why := redirectBlock(a, held[a.ID]); why != "" {
		h.d.Fail(w, r, errors.New("can't redirect: "+why))
		return
	}
	rd, ok := h.d.Actions.(Redirector)
	if !ok || rd == nil {
		h.d.Fail(w, r, errNoActions)
		return
	}
	if err := rd.Redirect(a, text); err != nil {
		h.d.Fail(w, r, err)
		return
	}
	msg := "Course correction queued for " + a.ID + ". The supervisor interrupts its turn at the next safe moment and refuses if a permission dialog is open."
	if r.Header.Get("HX-Request") == "true" {
		h.d.OK(w, r, msg, "")
		return
	}
	http.Redirect(w, r, "/tasks/"+t.ID, http.StatusSeeOther)
}
