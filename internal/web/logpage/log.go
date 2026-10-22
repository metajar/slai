// Package logpage is the Log page of `slai serve`: the event stream with filters by agent,
// kind, task and text, newest first, 100 per page. Hook and usage events are hidden unless
// asked for, as in the TUI. It is read-only.
package logpage

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/web/server"
	"github.com/oorbt/slai/internal/web/templates"
)

// PageSize is the events per page.
const PageSize = 100

// The log body is a live region (data-live="events", see the template): slai.js refetches
// /f/log with the same filters on an events notice, and every 30 s only while the stream is
// down. The page carries no polling trigger.

// Module registers /log and its refetch fragment.
func Module(d *server.Deps) []server.Route {
	m := &mod{d: d}
	return []server.Route{
		{Method: http.MethodGet, Pattern: "/log", Calls: "reads the events table with the chosen filters; read-only", Handler: m.page},
		{Method: http.MethodGet, Pattern: "/f/log", Calls: "the same read-only query, as the page's refetch fragment", Handler: m.fragment},
	}
}

type mod struct{ d *server.Deps }

// supervisorName stands for events with no agent (the supervisor's own).
const supervisorName = "supervisor"

type Row struct {
	Time, Agent, Kind, Chip, Task, What string
}

type Page struct {
	N       int
	Label   string
	URL     string
	Current bool
}

type View struct {
	Rows          []Row
	Agents, Kinds []string
	Agent, Kind   string
	Task, Text    string
	Hooks         bool
	Total, From   int
	To, PageNo    int
	Pages         int
	Prev, Next    string
	Numbers       []Page
	Query         string // the filters as a query string
	LiveSrc       string // the live region's refetch URL: /f/log plus Query (built here so the template does not re-escape it)
}

func filterOf(q url.Values) (store.EventFilter, int) {
	f := store.EventFilter{Kind: q.Get("kind"), Task: strings.TrimSpace(q.Get("task")), Text: strings.TrimSpace(q.Get("q")), Noise: q.Get("hooks") == "1", Limit: PageSize}
	if a := q.Get("agent"); a != "" {
		f.HasAgent = true
		f.Agent = a
		if a == supervisorName {
			f.Agent = ""
		}
	}
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	f.Offset = (page - 1) * PageSize
	return f, page
}

func (m *mod) view(r *http.Request) (View, error) {
	q := r.URL.Query()
	f, page := filterOf(q)
	evs, total, err := m.d.Store.QueryEvents(f)
	if err != nil {
		return View{}, err
	}
	pages := (total + PageSize - 1) / PageSize
	if page > pages && pages > 0 { // a stale page number: show the last page
		page = pages
		f.Offset = (page - 1) * PageSize
		if evs, total, err = m.d.Store.QueryEvents(f); err != nil {
			return View{}, err
		}
	}
	v := View{Agent: q.Get("agent"), Kind: q.Get("kind"), Task: f.Task, Text: f.Text, Hooks: f.Noise, Total: total, PageNo: page, Pages: pages}
	for _, e := range evs {
		v.Rows = append(v.Rows, rowOf(e))
	}
	if total > 0 {
		v.From, v.To = f.Offset+1, f.Offset+len(evs)
	}
	agents, _ := m.d.Store.EventAgents()
	for _, a := range agents {
		if a == "" {
			a = supervisorName
		}
		v.Agents = append(v.Agents, a)
	}
	v.Kinds, _ = m.d.Store.EventKinds()
	base := url.Values{}
	for _, k := range []string{"agent", "kind", "task", "q", "hooks"} {
		if val := q.Get(k); val != "" {
			base.Set(k, val)
		}
	}
	v.Query = base.Encode()
	v.LiveSrc = "/f/log?" + v.Query
	link := func(p int) string {
		x := url.Values{}
		for k, vs := range base {
			x[k] = vs
		}
		if p > 1 {
			x.Set("page", strconv.Itoa(p))
		}
		if len(x) == 0 {
			return "/log"
		}
		return "/log?" + x.Encode()
	}
	if page > 1 {
		v.Prev = link(page - 1)
	}
	if page < pages {
		v.Next = link(page + 1)
	}
	for p := max(1, page-3); p <= min(pages, page+3); p++ {
		v.Numbers = append(v.Numbers, Page{N: p, Label: strconv.Itoa(p), URL: link(p), Current: p == page})
	}
	return v, nil
}

// noisy kinds get a calm chip; the ones the TUI colours amber get a warning chip.
func chipOf(kind string) string {
	switch kind {
	case "rate_limited", "rate_limited_all", "modal_stuck", "revive_gave_up", "orch_down", "supervisor_down", "unread_stuck", "stuck_gave_up", "stalled",
		"ask_user", "spend_cap", "merge_conflict", "merge_error", "wake_error", "merge_refused", "lane_denied", "web_denied", "pane_decision":
		return "chip-warn"
	case "merged", "memory_edited":
		return "chip-ok"
	}
	if strings.HasPrefix(kind, "guard:") {
		return "chip-warn"
	}
	if strings.HasPrefix(kind, "hook:") || strings.HasPrefix(kind, "usage:") {
		return "chip-idle"
	}
	return "chip-info"
}

// whatKeys are the data fields that best say what an event was, in order.
var whatKeys = []string{"question", "summary", "title", "comments", "body", "message", "text", "reason", "cause", "error", "tool", "command", "scope", "action", "type", "status"}

func rowOf(e store.Event) Row {
	var d map[string]any
	_ = json.Unmarshal([]byte(e.Data), &d)
	agent := e.Agent
	if agent == "" {
		agent = supervisorName
	}
	r := Row{Time: time.Unix(e.At, 0).Local().Format("15:04:05"), Agent: agent, Kind: e.Kind, Chip: chipOf(e.Kind)}
	r.Task = str(d["task_id"])
	if r.Task == "" {
		r.Task = str(d["task"])
	}
	var parts []string
	if to := str(d["to"]); to != "" {
		parts = append(parts, "→ "+to)
	}
	for _, k := range whatKeys {
		if s := str(d[k]); s != "" {
			parts = append(parts, s)
			break
		}
	}
	if len(parts) == 0 && len(d) > 0 {
		b, _ := json.Marshal(d)
		parts = append(parts, string(b))
	}
	r.What = trunc(strings.Join(parts, " "), 200)
	return r
}

func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return fmt.Sprint(x)
	}
	return ""
}

func trunc(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func (m *mod) page(w http.ResponseWriter, r *http.Request) {
	v, err := m.view(r)
	if err != nil {
		m.d.Fail(w, r, err)
		return
	}
	tpl, err := m.d.Template("log")
	if err != nil {
		m.d.Fail(w, r, err)
		return
	}
	m.d.Render(w, r, tpl, templates.Page{Title: "Log", Active: "log", Data: v})
}

func (m *mod) fragment(w http.ResponseWriter, r *http.Request) {
	v, err := m.view(r)
	if err != nil {
		m.d.Fail(w, r, err)
		return
	}
	tpl, err := m.d.Template("log")
	if err != nil {
		m.d.Fail(w, r, err)
		return
	}
	m.d.Fragment(w, r, tpl, "log-live", v)
}
