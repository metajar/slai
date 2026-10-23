// Package dashboard is the home page of `slai serve`: team health, what needs the user,
// spend against caps, rate-limit gauges, and the team hold/pause controls.
//
// The page reads the same store the TUI does and classifies agents with the same
// service.AgentHealthWith, so the two cannot disagree. The live region is one fragment,
// GET /f/dashboard, that slai.js refetches when the SSE hub reports a change and that
// htmx refetches every 60 s so relative times stay true.
package dashboard

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/oorbt/slai/internal/agentrows"
	"github.com/oorbt/slai/internal/buildinfo"
	"github.com/oorbt/slai/internal/ratelimit"
	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/web/server"
	"github.com/oorbt/slai/internal/web/templates"
)

//go:embed dashboard.css
var css []byte

// Controller is what the hold/pause/resume buttons call: the supervisor's own methods (the
// same ones the TUI's `H` and `p` and `slai hold`/`resume` use). The server's Actions value
// is checked for it; without it the buttons say so instead of doing nothing.
type Controller interface {
	Pause() error
	Resume() error // also ends a team hold
	HoldTeam(name string) error
}

// Config tunes the module; every field is optional.
type Config struct {
	// BuildID is this binary's build (default buildinfo.ID), compared with the supervisor's.
	BuildID string
	// Orphans lists the prefixes of orphaned slai teams; nil means "not looked for". It is
	// called at most every OrphanEvery and may be slow (it scans tmux and processes).
	Orphans     func(ctx context.Context) ([]string, error)
	OrphanEvery time.Duration
}

var errNoControl = errors.New("this server has no supervisor connection, so it cannot hold or pause the team")

// Module returns the dashboard page module.
func Module(cfg Config) server.Module {
	if cfg.BuildID == "" {
		cfg.BuildID = buildinfo.ID()
	}
	if cfg.OrphanEvery <= 0 {
		cfg.OrphanEvery = 45 * time.Second
	}
	return func(d *server.Deps) []server.Route {
		if cfg.Orphans == nil {
			cfg.Orphans = d.Orphans
		}
		m := &mod{cfg: cfg, d: d}
		return []server.Route{
			{Method: http.MethodGet, Pattern: "/{$}", Calls: "reads agents, tasks, escalations, spend and rate limits from the store (the TUI's dashboard data)", Handler: m.page},
			{Method: http.MethodGet, Pattern: "/f/dashboard", Calls: "the dashboard's live region (same data as the home page)", Handler: m.fragment},
			{Method: http.MethodGet, Pattern: "/dashboard.css", Calls: "serves the dashboard stylesheet", Handler: serveCSS},
			{Method: http.MethodPost, Pattern: "/team/hold", Action: "hold", Target: func(*http.Request) string { return "team" },
				Calls: "the supervisor's HoldTeam, the same call as `slai hold` and the TUI's H", Handler: m.hold},
			{Method: http.MethodPost, Pattern: "/team/pause", Action: "pause", Target: func(*http.Request) string { return "team" },
				Calls: "the supervisor's Pause, the same call as the TUI's p", Handler: m.pause},
			{Method: http.MethodPost, Pattern: "/team/resume", Action: "resume", Target: func(*http.Request) string { return "team" },
				Calls: "the supervisor's Resume, the same call as `slai resume` (also ends a hold)", Handler: m.resume},
		}
	}
}

type mod struct {
	cfg Config
	d   *server.Deps

	mu         sync.Mutex
	orphans    []string
	orphansAt  time.Time
	orphansBus bool
	scans      int
}

func serveCSS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(css)
}

// pageData is what the templates are executed with.
type pageData struct {
	View
	ReadOnly bool
	HasCtl   bool
}

func (m *mod) data(r *http.Request) (pageData, error) {
	in, err := m.input(r.Context())
	if err != nil {
		return pageData{}, err
	}
	_, ok := m.d.Actions.(Controller)
	return pageData{View: Build(in), ReadOnly: m.d.ReadOnly, HasCtl: ok}, nil
}

func (m *mod) input(ctx context.Context) (Input, error) {
	d := m.d
	now := d.Now()
	team := d.Team()
	agents, err := d.Store.GetAgents()
	if err != nil {
		return Input{}, err
	}
	tasks, err := d.Store.ListTasks("")
	if err != nil {
		return Input{}, err
	}
	gc := service.GateConfigOf(team)
	gates := service.NewGateSource(d.Store, gc, now)
	in := Input{Now: now, Team: team, Agents: agents, Tasks: tasks, ReadOnly: d.ReadOnly,
		Health: map[string]service.Health{}, Identity: map[string]agentrows.Identity{}, Context: map[string]agentrows.Context{}, Questions: map[string]string{}, SpendBy: map[string]float64{}}
	ids := make([]string, 0, len(agents))
	for _, a := range agents {
		ids = append(ids, a.ID)
		in.Identity[a.ID] = agentrows.IdentityOf(d.Store, a.ID)
		in.Context[a.ID] = agentrows.ContextOf(d.Store, a.ID, team.Defaults.ContextThreshold())
		alive := a.Status != "dead" && a.Status != ""
		in.Health[a.ID] = service.AgentHealthWith(d.Store, a.ID, alive, now, service.InputOpts{Cfg: gc, Gates: gates})
		if a.Status == "waiting_on_user" {
			if evs, _ := d.Store.EventsByKind(a.ID, "ask_user", 1); len(evs) > 0 {
				var q struct{ Question, Reason string }
				_ = json.Unmarshal([]byte(evs[0].Data), &q)
				if in.Questions[a.ID] = q.Question; q.Question == "" {
					in.Questions[a.ID] = q.Reason
				}
			}
		}
	}
	in.Who = agentrows.WhoAll(d.Store, team, agents, in.Health, now)
	in.Gate = gates.Wake(ids)
	in.Esc, _ = d.Store.OpenEscalations()
	in.Heartbeat, _, _ = d.Store.SupervisorHeartbeat()
	in.Build = service.ReadBuildState(d.Store, m.cfg.BuildID, agents)
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
	if by, err := d.Store.UsageByAgent(midnight); err == nil {
		in.SpendBy = by
		for _, v := range by {
			in.SpendDay += v
		}
	}
	if snap, ok, _ := ratelimit.Load(d.Store); ok {
		in.Snap = &snap
	}
	in.Orphans = m.orphanList(ctx)
	return in, nil
}

// orphanList returns the cached scan, refreshing it when due. A failed scan keeps the last
// answer, so a flaky tmux call does not flash the banner away; the first request does not
// wait for the scan.
func (m *mod) orphanList(ctx context.Context) []string {
	if m.cfg.Orphans == nil {
		return nil
	}
	var wait chan struct{}
	m.mu.Lock()
	if !m.orphansBus && (m.orphansAt.IsZero() || m.d.Now().Sub(m.orphansAt) >= m.cfg.OrphanEvery) {
		m.orphansBus, m.orphansAt = true, m.d.Now()
		first := m.scans == 0
		m.scans++
		done := make(chan struct{})
		go func() {
			defer close(done)
			c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			list, err := m.cfg.Orphans(c)
			m.mu.Lock()
			defer m.mu.Unlock()
			m.orphansBus = false
			if err == nil {
				m.orphans = list
			}
		}()
		if first { // the very first look waits a moment, so the first page already knows
			wait = done
		}
	}
	m.mu.Unlock()
	if wait != nil {
		select {
		case <-wait:
		case <-time.After(500 * time.Millisecond):
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.orphans...)
}

func (m *mod) page(w http.ResponseWriter, r *http.Request) {
	data, err := m.data(r)
	if err != nil {
		m.d.Fail(w, r, err)
		return
	}
	tpl, err := m.d.Template("dashboard")
	if err != nil {
		m.d.Fail(w, r, err)
		return
	}
	m.d.Render(w, r, tpl, templates.Page{Title: "Dashboard", Active: "dashboard", Data: data})
}

func (m *mod) fragment(w http.ResponseWriter, r *http.Request) {
	data, err := m.data(r)
	if err != nil {
		m.d.Fail(w, r, err)
		return
	}
	tpl, err := m.d.Template("dashboard")
	if err != nil {
		m.d.Fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := tpl.ExecuteTemplate(w, "dash-body", data); err != nil {
		m.d.Fail(w, r, err)
	}
}

func (m *mod) ctl(w http.ResponseWriter, r *http.Request) (Controller, bool) {
	c, ok := m.d.Actions.(Controller)
	if !ok {
		m.d.Fail(w, r, errNoControl)
	}
	return c, ok
}

func (m *mod) hold(w http.ResponseWriter, r *http.Request) {
	c, ok := m.ctl(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		m.d.Fail(w, r, err)
		return
	}
	name := r.PostForm.Get("name")
	if err := service.CheckText("name", name); err != nil {
		m.d.Fail(w, r, err)
		return
	}
	if len(name) > 80 {
		m.d.Fail(w, r, errors.New("a hold name is at most 80 characters"))
		return
	}
	if err := c.HoldTeam(name); err != nil {
		m.d.Fail(w, r, err)
		return
	}
	m.d.OK(w, r, "The team is on hold. Nothing will be woken until you resume.", "/")
}

func (m *mod) pause(w http.ResponseWriter, r *http.Request) {
	c, ok := m.ctl(w, r)
	if !ok {
		return
	}
	if err := c.Pause(); err != nil {
		m.d.Fail(w, r, err)
		return
	}
	m.d.OK(w, r, "Wake-ups are paused.", "/")
}

func (m *mod) resume(w http.ResponseWriter, r *http.Request) {
	c, ok := m.ctl(w, r)
	if !ok {
		return
	}
	if err := c.Resume(); err != nil {
		m.d.Fail(w, r, err)
		return
	}
	m.d.OK(w, r, "Resumed: wake-ups will be delivered again.", "/")
}
