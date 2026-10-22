package server

import (
	"net/http"
	"sort"

	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/web/templates"
)

// builtinRoutes are the shell's own routes. All are Fallback: a page module that registers
// the same pattern replaces them (the dashboard module replaces "/", the agents module
// replaces the send route).
func builtinRoutes(d *Deps) []Route {
	return []Route{
		{Method: http.MethodGet, Pattern: "/{$}", Calls: "reads agents and open tasks from the store", Handler: d.dashboard},
		{Method: http.MethodGet, Pattern: "/theme", Calls: "sets the light/dark/system cookie; changes no team state", Handler: d.theme},
		{Method: http.MethodPost, Pattern: "/agents/{id}/send", Action: "send",
			Calls: "the supervisor's SendUser, the same call as `slai send`, after the 4000-character check", Handler: d.send},
	}
}

// capabilitiesRoute is not a fallback: nothing may replace the page that tells the truth
// about the route table.
func capabilitiesRoute(d *Deps) Route {
	return Route{Method: http.MethodGet, Pattern: "/capabilities",
		Calls: "reads this server's route table (this page)", Handler: d.capabilities}
}

type capRow struct {
	Method, Pattern, Action, Calls string
	Writes                         bool
}

// CapabilityRows is the route table as /capabilities shows it: one row per route, writes
// first split from reads. The page and its test both use it, so the printed claim and the
// tested claim are the same list.
func (s *Server) CapabilityRows() (writes, reads []capRow) {
	for _, r := range s.routes {
		row := capRow{Method: r.Method, Pattern: r.Pattern, Action: r.Action, Calls: r.Calls, Writes: r.Write()}
		if row.Calls == "" {
			row.Calls = "(not described)"
		}
		if row.Writes {
			writes = append(writes, row)
		} else {
			reads = append(reads, row)
		}
	}
	return writes, reads
}

func (d *Deps) capabilities(w http.ResponseWriter, r *http.Request) {
	writes, reads := d.s.CapabilityRows()
	tpl, err := d.Template("capabilities")
	if err != nil {
		d.Fail(w, r, err)
		return
	}
	d.Render(w, r, tpl, templates.Page{Title: "What this page can do", Data: struct{ Writes, Reads []capRow }{writes, reads}})
}

type dashData struct {
	Agents       []store.Agent
	Tasks        []store.Task
	Orchestrator string
}

// dashboard is the shell's placeholder home page: enough to prove the layout, the session
// and a write end to end. The dashboard module replaces it.
func (d *Deps) dashboard(w http.ResponseWriter, r *http.Request) {
	agents, err := d.Store.GetAgents()
	if err != nil {
		d.Fail(w, r, err)
		return
	}
	tasks, err := d.Store.ListTasks("")
	if err != nil {
		d.Fail(w, r, err)
		return
	}
	var open []store.Task
	for _, t := range tasks {
		switch t.Status {
		case store.StatusMerged, store.StatusCancelled, store.StatusDone, store.StatusReverted:
		default:
			open = append(open, t)
		}
	}
	sort.SliceStable(agents, func(i, j int) bool { return agents[i].ID < agents[j].ID })
	data := dashData{Agents: agents, Tasks: open}
	if d.Team != nil {
		for _, a := range d.Team().Agents {
			if a.Role == "orchestrator" {
				data.Orchestrator = a.ID
				break
			}
		}
	}
	tpl, err := d.Template("home")
	if err != nil {
		d.Fail(w, r, err)
		return
	}
	d.Render(w, r, tpl, templates.Page{Title: "Dashboard", Active: "dashboard", Data: data})
}

// theme stores the light/dark/system preference (cosmetic; the only state a GET changes).
// "system", or anything unrecognised, clears the cookie so the page follows the OS.
func (d *Deps) theme(w http.ResponseWriter, r *http.Request) {
	c := &http.Cookie{Name: "slai_theme", Value: "system", Path: "/", MaxAge: -1, SameSite: http.SameSiteStrictMode, Secure: secureRequest(r)}
	if v := r.URL.Query().Get("set"); v == "dark" || v == "light" {
		c.Value, c.MaxAge = v, 365*24*3600
	}
	http.SetCookie(w, c)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// send queues a user message for an agent, the same call as `slai send`. The text is held
// to the service's 4000-char limit and any error is shown verbatim.
func (d *Deps) send(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		d.Fail(w, r, err)
		return
	}
	id, body := r.PathValue("id"), r.PostForm.Get("body")
	if body == "" {
		d.Fail(w, r, errEmptyMessage)
		return
	}
	if err := service.CheckText("message", body); err != nil {
		d.Fail(w, r, err)
		return
	}
	if d.Actions == nil {
		d.Fail(w, r, errNoActions)
		return
	}
	n, err := d.Actions.SendUser(id, body)
	if err != nil {
		d.Fail(w, r, err)
		return
	}
	d.OK(w, r, "Queued message "+itoa(n)+" for "+id+".", "/")
}
