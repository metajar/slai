// Package agents is the Agents page of `slai serve` (spec P3): the list, the detail page
// with its read-only pane view, the agent actions (send, redirect, pause, resume) and the
// New Agent form. Every action calls the function the TUI or CLI calls; nothing here
// types into a pane or touches git.
package agents

import (
	"context"
	"errors"
	"github.com/oorbt/slai/internal/ratelimit"
	"github.com/oorbt/slai/internal/web/dashboard"
	"net/http"
	"strings"
	"time"

	"github.com/oorbt/slai/internal/agentrows"
	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/harness"
	"github.com/oorbt/slai/internal/hooks"
	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/supervisor"
	"github.com/oorbt/slai/internal/web/pane"
	"github.com/oorbt/slai/internal/web/server"
	"github.com/oorbt/slai/internal/web/templates"
)

// PaneSource captures an agent's tmux pane with colours (capture-pane -e). The server's
// Actions implements it when a tmux connection exists; without it the pane view says so.
type PaneSource interface {
	CapturePane(ctx context.Context, agentID string, withANSI bool) (string, error)
}

// AgentPauser is implemented by *supervisor.Supervisor, which the CLI passes as the server's
// Actions.
type AgentPauser interface {
	PauseAgent(id string) error
	ResumeAgent(id string) error
}

// view names the page variants of the one "agents" template directory.
const (
	viewList   = "list"
	viewDetail = "detail"
	viewNew    = "new"
)

type module struct {
	d    *server.Deps
	push *Push // nil: the pane view is not live
}

func newModule(d *server.Deps, push *Push) *module { return &module{d: d, push: push} }

// Module is the Agents page without the live pane push. It replaces the shell's fallback
// send route.
func Module(d *server.Deps) []server.Route { return newModule(d, nil).routes() }

func (m *module) routes() []server.Route {
	return []server.Route{
		{Method: http.MethodGet, Pattern: "/agents", Handler: m.list},
		{Method: http.MethodGet, Pattern: "/agents/new", Handler: m.newForm},
		{Method: http.MethodPost, Pattern: "/agents", Action: "new_agent", Calls: "config.AddAgent + config.Write (the TUI New Agent form's path)", Target: m.newTarget, Handler: m.create},
		{Method: http.MethodGet, Pattern: "/agents/{id}", Handler: m.detail},
		{Method: http.MethodGet, Pattern: "/agents/{id}/pane", Handler: m.pane},
		{Method: http.MethodPost, Pattern: "/agents/{id}/send", Action: "send", Calls: "Supervisor.SendUser (slai send)", Handler: m.send},
		{Method: http.MethodPost, Pattern: "/agents/{id}/redirect", Action: "redirect", Calls: "supervisor.RequestRedirect (the TUI redirect key)", Handler: m.redirect},
		{Method: http.MethodPost, Pattern: "/agents/{id}/pause", Action: "pause", Calls: "Supervisor.PauseAgent (store.SetAgentPaused)", Handler: m.pause},
		{Method: http.MethodPost, Pattern: "/agents/{id}/resume", Action: "resume", Calls: "Supervisor.ResumeAgent (store.SetAgentPaused false, clears the loop-guard hold)", Handler: m.resume},
	}
}

func (m *module) now() time.Time { return m.d.Now() }

func (m *module) team() *config.Team {
	if m.d.Team != nil {
		return m.d.Team()
	}
	return nil
}

type listData struct {
	View   string
	Groups []rowGroup
	Empty  bool
	// Limits is the plan's rate limits as one line: they are shared by every agent on the plan,
	// so the list shows them once above the table instead of repeating them on each row.
	Limits string
}

type rowGroup struct {
	Title string
	Rows  []Row
}

func (m *module) render(w http.ResponseWriter, r *http.Request, title string, data any) {
	tpl, err := m.d.Template("agents")
	if err != nil {
		m.d.Fail(w, r, err)
		return
	}
	m.d.Render(w, r, tpl, templates.Page{Title: title, Active: "agents", Data: data})
}

func (m *module) list(w http.ResponseWriter, r *http.Request) {
	rows, err := BuildRows(m.d.Store, m.team(), m.now())
	if err != nil {
		m.d.Fail(w, r, err)
		return
	}
	data := listData{View: viewList, Empty: len(rows) == 0}
	var snap *ratelimit.Snapshot
	if sn, ok, _ := ratelimit.Load(m.d.Store); ok {
		snap = &sn
	}
	data.Limits = dashboard.GaugeLine(m.now(), snap)
	for _, row := range rows {
		if n := len(data.Groups); n == 0 || data.Groups[n-1].Title != GroupTitle(row.Group) {
			data.Groups = append(data.Groups, rowGroup{Title: GroupTitle(row.Group)})
		}
		g := &data.Groups[len(data.Groups)-1]
		g.Rows = append(g.Rows, row)
	}
	m.render(w, r, "Agents", data)
}

type detailData struct {
	View         string
	Row          Row
	Info         map[string]string
	Degraded     string
	HealthEvents []HealthEvent
	DenialCount  int
	Denials      []Denial
	Corrections  []Correction
	History      []Message
	CLIOnly      []CLIOnly
	PaneNote     string
	CanAct       bool
	Pane         paneData
}

func (m *module) agent(w http.ResponseWriter, r *http.Request) (store.Agent, bool) {
	a, err := m.d.Store.AgentByID(r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return a, false
	}
	if err != nil {
		m.d.Fail(w, r, err)
		return a, false
	}
	return a, true
}

func (m *module) detail(w http.ResponseWriter, r *http.Request) {
	a, ok := m.agent(w, r)
	if !ok {
		return
	}
	rows, err := BuildRows(m.d.Store, m.team(), m.now())
	if err != nil {
		m.d.Fail(w, r, err)
		return
	}
	var row Row
	for _, x := range rows {
		if x.ID == a.ID {
			row = x
		}
	}
	now := m.now()
	n, last := LaneDenials(m.d.Store, a.ID, now)
	data := detailData{View: viewDetail, Row: row, Degraded: degradedText(row.Harness),
		HealthEvents: HealthEvents(m.d.Store, a.ID, 5, now), DenialCount: n, Denials: last,
		Corrections: Corrections(m.d.Store, a.ID, 5, now), History: History(m.d.Store, a.ID, 15, now),
		CLIOnly: CLIOnlyActions, CanAct: !m.d.ReadOnly, Pane: m.paneFor(r.Context(), a)}
	m.render(w, r, a.ID, data)
}

type paneData struct {
	AgentID  string
	Result   pane.Result
	Note     string // why there is no pane to show
	Redacted int
}

// paneFor captures and renders the agent's pane. Failures become a note, never an error page.
func (m *module) paneFor(ctx context.Context, a store.Agent) paneData {
	p := paneData{AgentID: a.ID}
	src, ok := m.d.Actions.(PaneSource)
	switch {
	case !ok:
		p.Note = "This server has no terminal connection, so the pane cannot be shown."
		return p
	case a.Status == "dead" || a.Status == "":
		p.Note = a.ID + " is not running, so there is no pane."
		return p
	}
	raw, err := src.CapturePane(ctx, a.ID, true)
	if err != nil {
		p.Note = "Could not read the pane: " + err.Error()
		return p
	}
	if m.push != nil {
		m.push.W.Changed(a.ID, raw) // remember what the user is shown; the push compares with it
	}
	p.Result = pane.Render(raw, true)
	p.Redacted = p.Result.Redacted
	return p
}

// pane serves the read-only pane fragment (htmx re-fetches it).
func (m *module) pane(w http.ResponseWriter, r *http.Request) {
	a, ok := m.agent(w, r)
	if !ok {
		return
	}
	tpl, err := m.d.Template("agents")
	if err != nil {
		m.d.Fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = tpl.ExecuteTemplate(w, "pane", m.paneFor(r.Context(), a))
}

var (
	errEmpty     = errors.New("message is empty")
	errNoActions = errors.New("this server has no supervisor connection, so it cannot queue messages")
)

func (m *module) send(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		m.d.Fail(w, r, err)
		return
	}
	id, body := r.PathValue("id"), r.PostForm.Get("body")
	if strings.TrimSpace(body) == "" {
		m.d.Fail(w, r, errEmpty)
		return
	}
	if err := service.CheckText("message", body); err != nil {
		m.d.Fail(w, r, err)
		return
	}
	if m.d.Actions == nil {
		m.d.Fail(w, r, errNoActions)
		return
	}
	n, err := m.d.Actions.SendUser(id, body)
	if err != nil {
		m.d.Fail(w, r, err)
		return
	}
	m.d.OK(w, r, "Queued message "+itoa(n)+" for "+id+".", "/agents/"+id)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for ; n > 0; n /= 10 {
		i--
		b[i] = byte('0' + n%10)
	}
	return string(b[i:])
}

// redirectBlock says why a course correction cannot be sent to a now, or "" when it can.
// It is the TUI's redirectBlock with the web's wording; the supervisor re-checks the pane
// before it presses Escape, this only keeps an impossible request from being queued.
func (m *module) redirectBlock(ctx context.Context, a store.Agent) string {
	st := m.d.Store
	paused, _ := st.PausedAgents()
	teamPaused := false
	if v, ok, _ := st.GetKV(supervisor.KVPaused); ok && v == "1" {
		teamPaused = true
	}
	switch {
	case a.Status == "dead" || a.Status == "errored" || a.Status == "":
		return a.ID + " is not running; revive it with v in the TUI"
	case !m.supervisorUp():
		return "no supervisor is running to interrupt " + a.ID + " (start one with slai up)"
	case teamPaused:
		return "the team is paused; resume it first (p in the TUI, or slai resume)"
	case paused[a.ID]:
		return a.ID + " is paused; resume it first (the Resume button above)"
	case a.Status == "waiting_on_user":
		return a.ID + " is waiting for your answer; answer it instead of redirecting"
	case a.Status != "working":
		return a.ID + " is not busy (" + a.Status + "); send an ordinary message instead"
	}
	if src, ok := m.d.Actions.(PaneSource); ok {
		kind := agentrows.HarnessOf(m.team(), a.ID)
		if h, err := harness.For(harness.Kind(kind)); err == nil && h.Features().PermissionPrompts {
			if raw, err := src.CapturePane(ctx, a.ID, false); err == nil && hooks.ModalExcerptWith(h.PaneRules(), raw) != "" {
				return a.ID + " has a permission dialog open; answer it first (attach to its session)"
			}
		}
	}
	return ""
}

func (m *module) supervisorUp() bool {
	up, known := m.d.SupervisorUp()
	return up || !known
}

func (m *module) redirect(w http.ResponseWriter, r *http.Request) {
	a, ok := m.agent(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		m.d.Fail(w, r, err)
		return
	}
	text := strings.TrimSpace(r.PostForm.Get("body"))
	if text == "" {
		m.d.Fail(w, r, errEmpty)
		return
	}
	if err := service.CheckText("message", text); err != nil {
		m.d.Fail(w, r, err)
		return
	}
	if why := m.redirectBlock(r.Context(), a); why != "" {
		m.d.Fail(w, r, errors.New("can't redirect: "+why))
		return
	}
	if _, err := supervisor.RequestRedirect(m.d.Store, a, text); err != nil {
		m.d.Fail(w, r, err)
		return
	}
	m.d.OK(w, r, "Interrupting "+a.ID+" with your correction.", "/agents/"+a.ID)
}

func (m *module) setPaused(w http.ResponseWriter, r *http.Request, paused bool) {
	a, ok := m.agent(w, r)
	if !ok {
		return
	}
	// The supervisor's own PauseAgent / ResumeAgent (what the TUI calls) when this server has
	// one; ResumeAgent also clears a loop-guard hold. The store flag is the same either way.
	var err error
	if p, ok := m.d.Actions.(AgentPauser); ok {
		if paused {
			err = p.PauseAgent(a.ID)
		} else {
			err = p.ResumeAgent(a.ID)
		}
	} else {
		err = m.d.Store.SetAgentPaused(a.ID, paused)
	}
	if err != nil {
		m.d.Fail(w, r, err)
		return
	}
	w.Header().Set("HX-Refresh", "true") // the page's buttons and chips follow the new state
	msg := a.ID + " paused: it is not woken until you resume it."
	if !paused {
		msg = a.ID + " resumed."
	}
	m.d.OK(w, r, msg, "/agents/"+a.ID)
}

func (m *module) pause(w http.ResponseWriter, r *http.Request)  { m.setPaused(w, r, true) }
func (m *module) resume(w http.ResponseWriter, r *http.Request) { m.setPaused(w, r, false) }
