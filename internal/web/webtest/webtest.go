// Package webtest is the shared httptest harness for `slai serve` page tests. It builds a
// real server.Server over a temporary store and a small team, and gives tests a browser-like
// client (cookie jar, CSRF header, Origin) plus the checks every page should pass.
//
//	h := webtest.New(t, webtest.WithModules(board.Module))
//	h.Login()
//	r := h.Get("/board")
//	h.CheckPage(r)                 // 200 HTML, CSP, no external URLs, no inline script
//	r = h.Post("/tasks", url.Values{"title": {"x"}})
//	h.WebActions()                 // the web_action events written so far
package webtest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/git"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/web/server"
)

// Addr is the loopback address the harness pretends to listen on.
const (
	Addr        = "127.0.0.1:8123"
	NonLoopback = "0.0.0.0:8123"
)

// Sent records one SendUser call made through the harness's fake Actions.
type Sent struct {
	To, Body string
}

// Harness is a server under test.
type Harness struct {
	T      *testing.T
	Srv    *server.Server
	Store  *store.Store
	Team   *config.Team
	Host   string // Host header the client sends
	Cookie *http.Cookie
	Dir    string // the project directory the server was given (a temp dir)

	// Repo is the git repository behind the server when the WithGit option is used.
	Repo *git.Repo

	// What the fake Actions return for the commit and revert previews.
	CommitText string
	CommitErr  error
	RevertText map[string]string
	RevertErr  map[string]error

	mu   sync.Mutex
	sent []Sent
	now  time.Time
}

type config_ struct {
	git       bool
	opts      server.Options
	host      string
	setup     func(*store.Store, *config.Team)
	noTeam    bool
	actionsFn func(*Harness) server.Actions
}

// Option customises New.
type Option func(*config_)

// WithModules adds page modules.
func WithModules(m ...server.Module) Option {
	return func(c *config_) { c.opts.Modules = append(c.opts.Modules, m...) }
}

// AllowHosts adds Host header values accepted besides the bound address (--allow-host).
func AllowHosts(h ...string) Option {
	return func(c *config_) { c.opts.AllowHosts = append(c.opts.AllowHosts, h...) }
}

// ReadOnly starts the server with --read-only.
func ReadOnly() Option { return func(c *config_) { c.opts.ReadOnly = true } }

// Listen sets the listen address (default Addr); a non-loopback one needs the token.
func Listen(addr string) Option {
	return func(c *config_) {
		c.opts.Listen = addr
		c.opts.InsecureHTTP = true
		c.host = strings.Replace(addr, "0.0.0.0", "10.0.0.5", 1)
	}
}

// WithGit gives the server a real git repository in its project directory: one commit on
// main and the integration branch slai/integration at the same commit. Use Harness.Repo and
// Harness.CommitFile to build task branches.
func WithGit() Option { return func(c *config_) { c.git = true } }

// Setup runs against the store and team before the server starts (seed agents, tasks). Several
// Setup options run in order.
func Setup(f func(*store.Store, *config.Team)) Option {
	return func(c *config_) {
		prev := c.setup
		c.setup = func(st *store.Store, t *config.Team) {
			if prev != nil {
				prev(st, t)
			}
			f(st, t)
		}
	}
}

// WithActions replaces the fake Actions (default: records SendUser calls). Use it to give a
// page module the extra capabilities its tests need (a fake or the real service).
func WithActions(a server.Actions) Option { return func(c *config_) { c.opts.Actions = a } }

// WithActionsFunc is WithActions for an Actions built from the harness (it can use h.Store,
// h.Team and the harness's Dir once they exist). The function runs after setup.
func WithActionsFunc(f func(h *Harness) server.Actions) Option {
	return func(c *config_) { c.actionsFn = f }
}

// SupervisorRunning sets the supervisor-running probe.
func SupervisorRunning(f func() bool) Option {
	return func(c *config_) { c.opts.SupervisorRunning = f }
}

// New builds a harness: a temp store, a team of orch + eng-1, fake actions, a fixed token
// ("test-token") and a fixed clock.
func New(t *testing.T, opts ...Option) *Harness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	team := config.Preset("solo", "p", "d", "main")
	team.Agents = []config.Agent{{ID: "orch", Role: config.RoleOrchestrator, Model: "sonnet"}, {ID: "eng-1", Role: config.RoleEngineer, Model: "sonnet"}}
	h := &Harness{T: t, Store: st, Team: team, now: time.Now().Truncate(time.Second)}
	st.SetClock(h.clock) // events and messages are stamped with the harness clock, which Advance moves
	c := &config_{host: Addr}
	c.opts.Listen = Addr
	for _, o := range opts {
		o(c)
	}
	if c.setup != nil {
		c.setup(st, team)
	}
	c.opts.Dir = t.TempDir()
	h.Dir = c.opts.Dir
	h.RevertText, h.RevertErr = map[string]string{}, map[string]error{}
	if c.git {
		h.Repo = initRepo(t, c.opts.Dir)
		c.opts.Git = h.Repo
	}
	c.opts.Store = st
	c.opts.Team = func() *config.Team { return team }
	if c.opts.Actions == nil && c.actionsFn != nil {
		c.opts.Actions = c.actionsFn(h)
	}
	if c.opts.Actions == nil {
		c.opts.Actions = actions{h}
	}
	c.opts.Now = h.clock
	c.opts.Token = "test-token"
	srv, err := server.New(c.opts)
	if err != nil {
		t.Fatal(err)
	}
	srv.SetBound(c.opts.Listen)
	h.Srv, h.Host = srv, c.host
	return h
}

type actions struct{ h *Harness }

// CommitPreview and RevertPreview return the texts tests set on the harness
// (Harness.CommitText, Harness.RevertText[task]); the real ones come from cmd.
func (a actions) CommitPreview(context.Context) (string, error) {
	a.h.mu.Lock()
	defer a.h.mu.Unlock()
	if a.h.CommitErr != nil {
		return "", a.h.CommitErr
	}
	return a.h.CommitText, nil
}

func (a actions) RevertPreview(_ context.Context, task string) (string, error) {
	a.h.mu.Lock()
	defer a.h.mu.Unlock()
	if err, ok := a.h.RevertErr[task]; ok {
		return "", err
	}
	if t, ok := a.h.RevertText[task]; ok {
		return t, nil
	}
	return "", fmt.Errorf("task %s is not merged", task)
}

func (a actions) SendUser(to, body string) (int64, error) {
	a.h.mu.Lock()
	defer a.h.mu.Unlock()
	a.h.sent = append(a.h.sent, Sent{to, body})
	return int64(len(a.h.sent)), nil
}

func (h *Harness) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

// AllTasks lists every task in the store.
func (h *Harness) AllTasks() []store.Task {
	h.T.Helper()
	ts, err := h.Store.ListTasks("")
	if err != nil {
		h.T.Fatal(err)
	}
	return ts
}

// Advance moves the harness clock (the one the server's Now reports) forward.
func (h *Harness) Advance(d time.Duration) {
	h.mu.Lock()
	h.now = h.now.Add(d)
	h.mu.Unlock()
}

// Sent returns the messages queued through the fake Actions.
func (h *Harness) Sent() []Sent {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Sent(nil), h.sent...)
}

// Resp is a recorded response.
type Resp struct {
	Code   int
	Header http.Header
	Body   string
}

// Do sends req (Host defaulted) through the full handler chain with no extra headers.
func (h *Harness) Do(req *http.Request) *Resp {
	h.T.Helper()
	if req.Host == "" || req.Host == "example.com" {
		req.Host = h.Host
	}
	rec := httptest.NewRecorder()
	h.Srv.Handler().ServeHTTP(rec, req)
	res := rec.Result()
	b, _ := io.ReadAll(res.Body)
	// keep the session cookie like a browser would
	for _, c := range res.Cookies() {
		if c.Name == "slai_session" && c.Value != "" {
			h.Cookie = c
		}
	}
	return &Resp{res.StatusCode, res.Header, string(b)}
}

// Raw builds a request with only what the caller gives it: no cookie, no CSRF header, no
// Origin. Use it for the negative cases.
func (h *Harness) Raw(method, path string, body url.Values) *http.Request {
	var rd io.Reader
	if body != nil {
		rd = strings.NewReader(body.Encode())
	}
	req := httptest.NewRequest(method, path, rd)
	req.Host = h.Host
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return req
}

// Login makes the harness a signed-in browser: it loads "/" (loopback) or exchanges the
// token (non-loopback).
func (h *Harness) Login() {
	h.T.Helper()
	path := "/"
	if !h.Srv.Loopback() {
		path = "/?token=" + url.QueryEscape(h.Srv.Token())
	}
	h.Cookie = nil
	h.Do(h.Raw("GET", path, nil))
	if h.Cookie == nil {
		h.T.Fatal("login did not set a session cookie")
	}
}

// Get is a browser GET with the session cookie.
func (h *Harness) Get(path string) *Resp {
	h.T.Helper()
	req := h.Raw("GET", path, nil)
	if h.Cookie != nil {
		req.AddCookie(h.Cookie)
	}
	return h.Do(req)
}

// Post is a browser POST: session cookie, X-Slai-Request, same-origin Origin.
func (h *Harness) Post(path string, form url.Values) *Resp {
	h.T.Helper()
	return h.Do(h.Browser("POST", path, form))
}

// Browser builds a request the way the page's own htmx would: cookie, CSRF header, Origin.
func (h *Harness) Browser(method, path string, form url.Values) *http.Request {
	req := h.Raw(method, path, form)
	if h.Cookie != nil {
		req.AddCookie(h.Cookie)
	}
	req.Header.Set("X-Slai-Request", "1")
	req.Header.Set("Origin", "http://"+h.Host)
	return req
}

// EventsOfKind returns the data of every event of the given kind, oldest first.
func (h *Harness) EventsOfKind(kind string) []map[string]any {
	h.T.Helper()
	evs, err := h.Store.RecentEvents(100000)
	if err != nil {
		h.T.Fatal(err)
	}
	var out []map[string]any
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Kind != kind {
			continue
		}
		var m map[string]any
		_ = json.Unmarshal([]byte(evs[i].Data), &m)
		out = append(out, m)
	}
	return out
}

// AllEvents returns every event in the store, newest first.
func (h *Harness) AllEvents() []store.Event {
	h.T.Helper()
	evs, err := h.Store.RecentEvents(100000)
	if err != nil {
		h.T.Fatal(err)
	}
	return evs
}

// WebActions returns the web_action events written so far, oldest first.
func (h *Harness) WebActions() []map[string]any {
	h.T.Helper()
	evs, err := h.Store.RecentEvents(10000)
	if err != nil {
		h.T.Fatal(err)
	}
	var out []map[string]any
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Kind != "web_action" {
			continue
		}
		var m map[string]any
		_ = json.Unmarshal([]byte(evs[i].Data), &m)
		out = append(out, m)
	}
	return out
}

var (
	external = regexp.MustCompile(`(?i)(?:href|src|action|content)\s*=\s*["']\s*(?:https?:)?//`)
	inlineJS = regexp.MustCompile(`(?is)<script(?:\s[^>]*)?>[^<]`)
	inlineOn = regexp.MustCompile(`(?i)\son[a-z]+\s*=`)
	styleAtt = regexp.MustCompile(`(?i)\sstyle\s*=`)
)

// CheckPage asserts what every page must satisfy: 200, HTML, the security headers, no
// external URL, no inline script, no inline event handler and no style attribute (the CSP
// forbids them).
func (h *Harness) CheckPage(r *Resp) {
	h.T.Helper()
	if r.Code != 200 {
		h.T.Fatalf("status %d: %.200s", r.Code, r.Body)
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		h.T.Errorf("content type %q", ct)
	}
	h.CheckHeaders(r)
	if m := external.FindString(r.Body); m != "" {
		h.T.Errorf("page references another origin: %s", m)
	}
	if m := inlineJS.FindString(r.Body); m != "" {
		h.T.Errorf("inline script: %s", m)
	}
	if m := inlineOn.FindString(r.Body); m != "" {
		h.T.Errorf("inline event handler: %s", m)
	}
	if m := styleAtt.FindString(r.Body); m != "" {
		h.T.Errorf("style attribute: %s", m)
	}
}

// CheckHeaders asserts the security headers on any response.
func (h *Harness) CheckHeaders(r *Resp) {
	h.T.Helper()
	csp := r.Header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "script-src 'self'", "frame-ancestors 'none'", "object-src 'none'"} {
		if !strings.Contains(csp, want) {
			h.T.Errorf("CSP %q lacks %q", csp, want)
		}
	}
	if strings.Contains(csp, "unsafe-") {
		h.T.Errorf("CSP allows unsafe sources: %q", csp)
	}
	for k, v := range map[string]string{"X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer", "X-Content-Type-Options": "nosniff"} {
		if r.Header.Get(k) != v {
			h.T.Errorf("%s = %q, want %q", k, r.Header.Get(k), v)
		}
	}
}

// ConcretePath turns a route pattern into a requestable path ("/tasks/{id}/cancel" ->
// "/tasks/x/cancel").
func ConcretePath(pattern string) string {
	p := regexp.MustCompile(`\{[^}]*\}`).ReplaceAllStringFunc(pattern, func(s string) string {
		if s == "{$}" {
			return ""
		}
		return "x"
	})
	if p == "" {
		return "/"
	}
	return p
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e.com",
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func initRepo(t *testing.T, dir string) *git.Repo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	runGit(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(dir+"/a.txt", []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-q", "-m", "init")
	runGit(t, dir, "branch", "slai/integration")
	return &git.Repo{Dir: dir}
}

// CommitFile creates (or extends) branch from the integration branch with one commit that
// writes content to file, then returns to main. It uses the harness's own repository.
func (h *Harness) CommitFile(branch, file, content string) {
	h.T.Helper()
	if h.Repo == nil {
		h.T.Fatal("CommitFile needs the WithGit option")
	}
	if exec.Command("git", "-C", h.Dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch).Run() != nil {
		runGit(h.T, h.Dir, "branch", branch, "slai/integration")
	}
	runGit(h.T, h.Dir, "checkout", "-q", branch)
	if err := os.WriteFile(h.Dir+"/"+file, []byte(content), 0o644); err != nil {
		h.T.Fatal(err)
	}
	runGit(h.T, h.Dir, "add", "-A")
	runGit(h.T, h.Dir, "commit", "-q", "-m", "work on "+file)
	runGit(h.T, h.Dir, "checkout", "-q", "main")
}
