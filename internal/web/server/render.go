package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/git"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/web/templates"
)

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// Deps is what a page package gets: the same store, team and actions the CLI uses, plus
// rendering and error helpers. It is shared by every module; do not retain request state.
type Deps struct {
	Dir      string
	Store    *store.Store
	Git      *git.Repo
	Team     func() *config.Team
	Actions  Actions
	ReadOnly bool
	Now      func() time.Time
	// Orphans is Options.Orphans (nil when the server was not given one).
	Orphans func(ctx context.Context) ([]string, error)
	s       *Server
}

// Session is the caller's session (never nil on a routed request).
func (d *Deps) Session(r *http.Request) *Session { return d.s.sessions.fromRequest(r) }

// Template returns the layout plus every file of the page directory (e.g. "board").
func (d *Deps) Template(dir string) (*template.Template, error) { return templates.PageTemplate(dir) }

// Render executes a page template into the layout. Title and Active are the page's; the
// nav, theme, read-only flag and banners are filled in here.
func (d *Deps) Render(w http.ResponseWriter, r *http.Request, tpl *template.Template, p templates.Page) {
	p.Nav = d.s.nav()
	for i := range p.Nav {
		if f := d.s.badge(p.Nav[i].Key); f != nil {
			p.Nav[i].Badge, p.Nav[i].Hint = f()
			if p.Nav[i].Key == "inbox" {
				p.Needs = p.Nav[i].Badge
			}
		}
	}
	p.ReadOnly = d.ReadOnly
	p.Theme = themeOf(r)
	if sess := d.s.sessions.fromRequest(r); sess != nil {
		p.Refused = sess.refused.Load()
	}
	p.RefusedUnauth = d.s.refusedUnauth.Load()
	if p.Notice == "" && d.s.opts.SupervisorRunning != nil && !d.s.opts.SupervisorRunning() {
		p.Notice = "The supervisor is not running, so agents will not be woken. Run `slai up`. You can read the team and queue messages meanwhile."
	}
	var b bytes.Buffer
	if err := tpl.ExecuteTemplate(&b, "layout", p); err != nil {
		d.s.refuse(w, r, http.StatusInternalServerError, "Template error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b.Bytes())
}

// Fail shows a service error verbatim (escaped) with status 422: htmx swaps it into the
// page's target, a plain form post gets a small error page.
func (d *Deps) Fail(w http.ResponseWriter, r *http.Request, err error) {
	if err == nil {
		err = errors.New("failed")
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnprocessableEntity)
		template.Must(template.New("e").Parse(`<div class="alert alert-danger" role="alert">{{.}}</div>`)).Execute(w, err.Error())
		return
	}
	d.s.refuse(w, r, http.StatusUnprocessableEntity, "That did not work", err.Error())
}

// OK answers a successful write: an htmx request gets a one-line confirmation fragment, a
// plain form post is redirected to back (a path on this server).
func (d *Deps) OK(w http.ResponseWriter, r *http.Request, msg, back string) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		template.Must(template.New("o").Parse(`<div class="alert alert-success" role="status">{{.}}</div>`)).Execute(w, msg)
		return
	}
	if back == "" || back[0] != '/' || (len(back) > 1 && back[1] == '/') {
		back = "/"
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

var navItems = []templates.NavItem{
	{Label: "Dashboard", Href: "/", Key: "dashboard"}, {Label: "Agents", Href: "/agents", Key: "agents"},
	{Label: "Board", Href: "/board", Key: "board"}, {Label: "Reviews", Href: "/reviews", Key: "reviews"},
	{Label: "Inbox", Href: "/inbox", Key: "inbox"}, {Label: "Stats", Href: "/stats", Key: "stats"},
	{Label: "Memory", Href: "/memory", Key: "memory"}, {Label: "Doctor", Href: "/doctor", Key: "doctor"},
	{Label: "Log", Href: "/log", Key: "log"},
}

// nav is the sidebar; an entry is enabled once some module serves its path.
func (s *Server) nav() []templates.NavItem {
	out := make([]templates.NavItem, len(navItems))
	for i, n := range navItems {
		n.Enabled = s.hasPage(n.Href)
		out[i] = n
	}
	return out
}

// themeOf is the stored preference: "light", "dark", or "system" (no cookie, the default), which
// leaves the choice to theme.js and the OS.
func themeOf(r *http.Request) string {
	if c, err := r.Cookie("slai_theme"); err == nil && (c.Value == "dark" || c.Value == "light") {
		return c.Value
	}
	return "system"
}

var (
	errEmptyMessage = errors.New("message is empty")
	errNoActions    = errors.New("this server has no supervisor connection, so it cannot queue messages")
)

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// SupervisorUp reports whether a supervisor holds the project lock. known is false when
// the server was started without a probe (then a page should neither claim nor deny it).
func (d *Deps) SupervisorUp() (up, known bool) {
	if d.s.opts.SupervisorRunning == nil {
		return false, false
	}
	return d.s.opts.SupervisorRunning(), true
}

// SetBadge makes the nav entry with this key show a count (and a tooltip) on every page:
// f is called per render. The inbox module sets "inbox" to the number of items that need
// the user; that number also prefixes the browser tab title.
func (d *Deps) SetBadge(key string, f func() (n int, hint string)) {
	d.s.badgeMu.Lock()
	defer d.s.badgeMu.Unlock()
	if d.s.badges == nil {
		d.s.badges = map[string]func() (int, string){}
	}
	d.s.badges[key] = f
}

func (s *Server) badge(key string) func() (int, string) {
	s.badgeMu.Lock()
	defer s.badgeMu.Unlock()
	return s.badges[key]
}

// Integration is the integration branch name from the current team definition.
func (d *Deps) Integration() string {
	if d.Team != nil {
		if t := d.Team(); t != nil && t.Project.IntegrationBranch != "" {
			return t.Project.IntegrationBranch
		}
	}
	return "slai/integration"
}
