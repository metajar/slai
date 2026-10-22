// Package review is the Reviews page of `slai serve` and the commit/revert preview: what
// the principal sees (the in-review queue, the diff, per-AC claims and verdicts, merge
// refusals) and the plans for `slai commit` and `slai revert`. Everything here is read-only:
// approving, requesting changes, merging and applying stay with the principal and the CLI,
// and this package registers no write route.
package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/web/server"
	"github.com/oorbt/slai/internal/web/templates"
)

// Module registers /reviews and /commit-preview (both GET).
func Module(d *server.Deps) []server.Route {
	m := &mod{d: d}
	return []server.Route{
		{Method: http.MethodGet, Pattern: "/reviews", Calls: "reads in-review tasks, their diff (git diff integration...branch), AC claims and verdicts, and merge_refused events; changes nothing (it does not pin the reviewed commit the way review_diff does)", Handler: m.page},
		{Method: http.MethodGet, Pattern: "/commit-preview", Calls: "the plan `slai commit --dry-run` prints, and `slai revert <task> --dry-run` for a chosen merged task; read-only", Handler: m.preview},
	}
}

type mod struct{ d *server.Deps }

type queueRow struct {
	ID, Title, Engineer, Reviewer, Waiting string
	Round                                  int
	Selected                               bool
	Warnings                               []string
	at                                     time.Time
}

type diffLine struct{ Class, Text string }

type diffFile struct {
	Name       string
	Added, Del int
	Lines      []diffLine
	Open       bool
}

type acRow struct {
	AC, ClaimNote, VerdictNote, State, Chip string
	Met                                     bool
}

type histRow struct {
	Dot, Title, Detail string
}

type refusal struct {
	Task, When, Cause, Command, Tail string
	Exit                             string
}

type view struct {
	Queue      []queueRow
	Reviewers  string
	Task       *queueRow
	Diff       []diffFile
	DiffNote   string
	Truncated  bool
	ReviewNo   int
	Claims     []acRow
	History    []histRow
	Refusals   []refusal
	Warnings   []string
	GitMissing bool
}

func (m *mod) page(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := m.d.Now()
	var v view
	if m.d.Team != nil {
		var ids []string
		for _, a := range m.d.Team().Agents {
			if a.Role == "principal_engineer" {
				ids = append(ids, a.ID)
			}
		}
		v.Reviewers = strings.Join(ids, ", ")
	}
	tasks, err := m.d.Store.ListTasks(store.StatusInReview)
	if err != nil {
		m.d.Fail(w, r, err)
		return
	}
	for _, t := range tasks {
		row := queueRow{ID: t.ID, Title: t.Title, Engineer: t.Assignee, Reviewer: v.Reviewers, Round: t.ReviewRounds + 1, at: time.Unix(t.UpdatedAt, 0)}
		evs, _ := m.d.Store.TaskEvents(t.ID, 300)
		for _, e := range evs {
			switch e.Kind {
			case "request_review":
				row.at = time.Unix(e.At, 0)
			case "spec_rewritten":
				row.Warnings = appendOnce(row.Warnings, "spec rewritten")
			case "spec_addendum":
				row.Warnings = appendOnce(row.Warnings, "spec addendum")
			}
		}
		row.Waiting = templates.Age(now.Sub(row.at))
		v.Queue = append(v.Queue, row)
	}
	sort.SliceStable(v.Queue, func(i, j int) bool { return v.Queue[i].at.Before(v.Queue[j].at) })
	if len(v.Queue) > 0 {
		sel := 0
		if want := r.URL.Query().Get("task"); want != "" {
			for i, q := range v.Queue {
				if q.ID == want {
					sel = i
				}
			}
		}
		v.Queue[sel].Selected = true
		v.Task = &v.Queue[sel]
		m.fillTask(ctx, &v, tasks, v.Task.ID)
	}
	for _, q := range v.Queue {
		for _, w := range q.Warnings {
			v.Warnings = append(v.Warnings, q.ID+": "+w)
		}
	}
	v.Refusals = m.refusals(now)
	tpl, err := m.d.Template("reviews")
	if err != nil {
		m.d.Fail(w, r, err)
		return
	}
	m.d.Render(w, r, tpl, templates.Page{Title: "Reviews", Active: "reviews", Data: v})
}

func appendOnce(s []string, x string) []string {
	for _, e := range s {
		if e == x {
			return s
		}
	}
	return append(s, x)
}

// fillTask loads the diff, claims, verdicts and history of the selected task.
func (m *mod) fillTask(ctx context.Context, v *view, tasks []store.Task, id string) {
	var t store.Task
	for _, x := range tasks {
		if x.ID == id {
			t = x
		}
	}
	if m.d.Git == nil || t.Branch == "" {
		v.GitMissing = true
		v.DiffNote = "No diff to show: this task has no branch, or the server has no repository."
	} else if d, err := m.d.Git.Diff(ctx, m.d.Integration(), t.Branch, service.MaxDiffLines); err != nil {
		v.DiffNote = "Could not read the diff: " + err.Error()
	} else {
		v.Diff = parseDiff(d.Patch)
		v.Truncated = d.Truncated
		if len(v.Diff) == 0 {
			v.DiffNote = "The branch has no changes against " + m.d.Integration() + " yet."
		}
	}
	no, claims, _ := m.d.Store.LatestACClaims(id)
	v.ReviewNo = no
	verdicts, _ := m.d.Store.LatestACVerdicts(id)
	byKey := map[string]store.ACVerdict{}
	for _, vd := range verdicts {
		byKey[store.ACKey(vd.AC)] = vd
	}
	for _, c := range claims {
		row := acRow{AC: c.AC, Met: c.Met, ClaimNote: c.Note, State: "unanswered", Chip: "chip-idle"}
		if vd, ok := byKey[store.ACKey(c.AC)]; ok {
			row.VerdictNote = vd.Note
			if vd.Pass {
				row.State, row.Chip = "pass", "chip-ok"
			} else {
				row.State, row.Chip = "fail", "chip-bad"
			}
		}
		v.Claims = append(v.Claims, row)
	}
	evs, _ := m.d.Store.TaskEvents(id, 300)
	for i := len(evs) - 1; i >= 0; i-- { // newest first
		e := evs[i]
		var d map[string]any
		_ = json.Unmarshal([]byte(e.Data), &d)
		switch e.Kind {
		case "request_changes":
			v.History = append(v.History, histRow{"warn", "Changes requested by " + e.Agent, str(d["comments"])})
		case "approve", "merge_requested":
			v.History = append(v.History, histRow{"ok", "Approved by " + e.Agent, ""})
		case "merge_refused":
			v.History = append(v.History, histRow{"bad", "Merge refused", str(d["cause"])})
		case "merged":
			v.History = append(v.History, histRow{"ok", "Merged", ""})
		case "request_review":
			v.History = append(v.History, histRow{"info", "Review requested by " + e.Agent, ""})
		}
	}
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// refusals are the newest merge_refused events, with the output tail the event carries.
func (m *mod) refusals(now time.Time) []refusal {
	evs, err := m.d.Store.RecentEvents(2000)
	if err != nil {
		return nil
	}
	var out []refusal
	for _, e := range evs { // newest first
		if e.Kind != "merge_refused" {
			continue
		}
		var d struct {
			Task     string `json:"task_id"`
			Cause    string `json:"cause"`
			Command  string `json:"command"`
			Tail     string `json:"tail"`
			Exit     *int   `json:"exit"`
			TimedOut bool   `json:"timed_out"`
		}
		_ = json.Unmarshal([]byte(e.Data), &d)
		r := refusal{Task: d.Task, When: templates.Stamp(now, time.Unix(e.At, 0)), Cause: d.Cause, Command: d.Command, Tail: strings.TrimRight(d.Tail, "\n")}
		if d.Exit != nil {
			r.Exit = fmt.Sprint(*d.Exit)
		}
		if r.Cause == "" {
			r.Cause = "the merge was refused"
		}
		out = append(out, r)
		if len(out) == 5 {
			break
		}
	}
	return out
}

// parseDiff splits a unified patch into files with classified lines. The text of every
// line is data and reaches the page only through html/template escaping.
func parseDiff(patch string) []diffFile {
	var files []diffFile
	for _, l := range strings.Split(strings.TrimRight(patch, "\n"), "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			name := l
			if i := strings.LastIndex(l, " b/"); i >= 0 {
				name = l[i+3:]
			}
			files = append(files, diffFile{Name: name})
			continue
		case len(files) == 0:
			continue
		case strings.HasPrefix(l, "index "), strings.HasPrefix(l, "--- "), strings.HasPrefix(l, "+++ "):
			continue
		}
		f := &files[len(files)-1]
		class := "ctx"
		switch {
		case strings.HasPrefix(l, "@@"):
			class = "hunk"
		case strings.HasPrefix(l, "+"):
			class = "add"
			f.Added++
		case strings.HasPrefix(l, "-"):
			class = "del"
			f.Del++
		case strings.HasPrefix(l, "... [truncated"):
			class = "hunk"
		}
		if l == "" {
			l = " "
		}
		f.Lines = append(f.Lines, diffLine{class, l})
	}
	for i := range files {
		files[i].Open = len(files) <= 30 || i < 3 // a large change opens only its first files
	}
	return files
}

type previewView struct {
	Commit, CommitErr string
	Merged            []store.Task
	Revert, RevertErr string
	Chosen            string
}

func (m *mod) preview(w http.ResponseWriter, r *http.Request) {
	var v previewView
	if m.d.Actions == nil {
		v.CommitErr = "this server has no supervisor connection, so it cannot compute the plan"
	} else if text, err := m.d.Actions.CommitPreview(r.Context()); err != nil {
		v.CommitErr = err.Error()
	} else {
		v.Commit = text
	}
	merged, _ := m.d.Store.ListTasks(store.StatusMerged)
	v.Merged = merged
	if want := r.URL.Query().Get("revert"); want != "" {
		v.Chosen = want
		if m.d.Actions == nil {
			v.RevertErr = "no supervisor connection"
		} else if text, err := m.d.Actions.RevertPreview(r.Context(), want); err != nil {
			v.RevertErr = err.Error()
		} else {
			v.Revert = text
		}
	}
	tpl, err := m.d.Template("commit")
	if err != nil {
		m.d.Fail(w, r, errors.New(err.Error()))
		return
	}
	m.d.Render(w, r, tpl, templates.Page{Title: "Commit preview", Active: "", Data: v})
}
