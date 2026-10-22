package server_test

import (
	"context"
	"crypto/tls"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/web/server"
	"github.com/oorbt/slai/internal/web/webtest"
)

// stub is a test module: an SSE-like read route (the real /events belongs to the SSE hub)
// and two more writes, so the route-table tests have more than the shell's own routes.
func stub(d *server.Deps) []server.Route {
	return []server.Route{
		{Method: "GET", Pattern: "/events", Handler: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, ": hi\n\n")
		}},
		{Method: "GET", Pattern: "/boom/{id}", Handler: func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "x") }},
		{Method: "POST", Pattern: "/tasks/{id}/cancel", Action: "cancel", Calls: "service CancelTask <stub>", Handler: func(w http.ResponseWriter, r *http.Request) {
			d.OK(w, r, "cancelled "+r.PathValue("id"), "/")
		}},
		{Method: "DELETE", Pattern: "/things/{id}", Action: "delete-thing", Calls: "nothing (test stub)", Handler: func(w http.ResponseWriter, r *http.Request) {
			d.Fail(w, r, fmt.Errorf("refused <b>%s</b>", r.PathValue("id")))
		}},
	}
}

func newH(t *testing.T, opts ...webtest.Option) *webtest.Harness {
	return webtest.New(t, append([]webtest.Option{webtest.WithModules(stub)}, opts...)...)
}

// P1 AC2 + security AC1: a foreign Host or Origin is refused on the normal and the SSE
// routes, for reads and writes, before any session or route logic.
func TestForeignHostAndOriginRefused(t *testing.T) {
	h := newH(t)
	h.Login()
	for _, route := range []string{"/", "/events"} {
		for _, host := range []string{"evil.example", "evil.example:8123", "127.0.0.1:9999", "localhost", "127.0.0.1.evil.example:8123"} {
			req := h.Raw("GET", route, nil)
			req.Host = host
			req.AddCookie(h.Cookie)
			if r := h.Do(req); r.Code != 403 {
				t.Errorf("GET %s with Host %q = %d, want 403", route, host, r.Code)
			}
		}
		for _, origin := range []string{"http://evil.example", "http://127.0.0.1:9999", "null", "http://127.0.0.1:8123.evil.example", "http://127.0.0.1:8123/path"} {
			req := h.Raw("GET", route, nil)
			req.AddCookie(h.Cookie)
			req.Header.Set("Origin", origin)
			if r := h.Do(req); r.Code != 403 {
				t.Errorf("GET %s with Origin %q = %d, want 403", route, origin, r.Code)
			}
		}
		// the legitimate names work
		for _, host := range []string{"127.0.0.1:8123", "localhost:8123"} {
			req := h.Raw("GET", route, nil)
			req.Host = host
			req.AddCookie(h.Cookie)
			req.Header.Set("Origin", "http://"+host)
			if r := h.Do(req); r.Code != 200 {
				t.Errorf("GET %s with Host %q = %d, want 200", route, host, r.Code)
			}
		}
	}
	req := h.Browser("POST", "/agents/orch/send", url.Values{"body": {"hi"}})
	req.Header.Set("Origin", "http://evil.example")
	if r := h.Do(req); r.Code != 403 || len(h.Sent()) != 0 || len(h.WebActions()) != 0 {
		t.Fatalf("cross-origin POST = %d, sent %v, actions %v", r.Code, h.Sent(), h.WebActions())
	}
	req = h.Browser("POST", "/agents/orch/send", url.Values{"body": {"hi"}})
	req.Host = "evil.example:8123"
	if r := h.Do(req); r.Code != 403 || len(h.Sent()) != 0 {
		t.Fatalf("foreign-host POST = %d", r.Code)
	}
}

// Security AC1: a write without X-Slai-Request, or without a session, changes nothing.
func TestWriteNeedsSessionAndRequestHeader(t *testing.T) {
	h := newH(t)
	form := url.Values{"body": {"hello"}}
	// no session: POST does not create one
	if r := h.Do(h.Raw("POST", "/agents/orch/send", form)); r.Code != 403 || h.Cookie != nil {
		t.Fatalf("no session: %d cookie=%v", r.Code, h.Cookie)
	}
	h.Login()
	for name, mod := range map[string]func(*http.Request){
		"no header":     func(r *http.Request) { r.Header.Del("X-Slai-Request") },
		"header is 0":   func(r *http.Request) { r.Header.Set("X-Slai-Request", "0") },
		"header is yes": func(r *http.Request) { r.Header.Set("X-Slai-Request", "yes") },
	} {
		req := h.Browser("POST", "/agents/orch/send", form)
		mod(req)
		if r := h.Do(req); r.Code != 403 {
			t.Errorf("%s: %d, want 403", name, r.Code)
		}
	}
	if len(h.Sent()) != 0 || len(h.WebActions()) != 0 {
		t.Fatalf("a refused write changed something: sent %v, actions %v", h.Sent(), h.WebActions())
	}
	// and with the header it goes through
	if r := h.Post("/agents/orch/send", form); r.Code != 303 && r.Code != 200 {
		t.Fatalf("valid write = %d: %s", r.Code, r.Body)
	}
	if got := h.Sent(); len(got) != 1 || got[0].To != "orch" || got[0].Body != "hello" {
		t.Fatalf("sent %v", got)
	}
}

// P1 AC3: every write route in the table refuses a POST without a valid session/header,
// and changes nothing. The table is read from the server, so new routes are covered.
func TestEveryWriteRouteRefusesUnauthenticatedWrites(t *testing.T) {
	h := newH(t)
	n := 0
	for _, rt := range h.Srv.Routes() {
		if !rt.Write() {
			continue
		}
		n++
		path := webtest.ConcretePath(rt.Pattern)
		if r := h.Do(h.Raw(rt.Method, path, url.Values{"body": {"x"}})); r.Code != 403 {
			t.Errorf("%s %s without a session = %d", rt.Method, path, r.Code)
		}
	}
	h.Login()
	for _, rt := range h.Srv.Routes() {
		if !rt.Write() {
			continue
		}
		req := h.Browser(rt.Method, webtest.ConcretePath(rt.Pattern), url.Values{"body": {"x"}})
		req.Header.Del("X-Slai-Request")
		if r := h.Do(req); r.Code != 403 {
			t.Errorf("%s %s without X-Slai-Request = %d", rt.Method, rt.Pattern, r.Code)
		}
	}
	if n < 3 {
		t.Fatalf("expected the shell and stub writes in the table, found %d", n)
	}
	if len(h.WebActions()) != 0 || len(h.Sent()) != 0 {
		t.Fatalf("refused writes had effects: %v %v", h.WebActions(), h.Sent())
	}
}

// Security AC3: each mutating route writes exactly one web_action event, actor user.
func TestEveryWriteRouteRecordsOneWebAction(t *testing.T) {
	h := newH(t)
	h.Login()
	count := 0
	for _, rt := range h.Srv.Routes() {
		if !rt.Write() {
			continue
		}
		count++
		before := len(h.WebActions())
		h.Do(h.Browser(rt.Method, webtest.ConcretePath(rt.Pattern), url.Values{"body": {"x"}}))
		acts := h.WebActions()
		if len(acts) != before+1 {
			t.Fatalf("%s %s wrote %d web_action events, want exactly 1", rt.Method, rt.Pattern, len(acts)-before)
		}
		last := acts[len(acts)-1]
		if last["actor"] != "user" || last["action"] != rt.Action {
			t.Errorf("%s %s: event %v, want actor user, action %q", rt.Method, rt.Pattern, last, rt.Action)
		}
	}
	if count == 0 {
		t.Fatal("no write routes")
	}
	// a read writes none
	before := len(h.WebActions())
	h.Get("/")
	h.Get("/events")
	if len(h.WebActions()) != before {
		t.Fatal("a GET recorded a web_action")
	}
}

// Security AC2: --read-only refuses every non-GET route, and a refused write records
// nothing. Reads still work and the page hides the write form.
func TestReadOnlyRefusesEveryWrite(t *testing.T) {
	h := newH(t, webtest.ReadOnly())
	h.Login()
	for _, rt := range h.Srv.Routes() {
		if rt.Write() {
			if r := h.Do(h.Browser(rt.Method, webtest.ConcretePath(rt.Pattern), url.Values{"body": {"x"}})); r.Code != 403 {
				t.Errorf("%s %s read-only = %d, want 403", rt.Method, rt.Pattern, r.Code)
			}
		}
	}
	if len(h.WebActions()) != 0 || len(h.Sent()) != 0 {
		t.Fatalf("read-only server wrote: %v %v", h.WebActions(), h.Sent())
	}
	page := h.Get("/")
	h.CheckPage(page)
	if strings.Contains(page.Body, "hx-post") || !strings.Contains(page.Body, "read-only") {
		t.Fatal("read-only page should hide the write form and say it is read-only")
	}
}

// Security AC1 (token): only the one-time exchange on / starts a non-loopback session; a
// wrong token, a token on another path, or a token after the exchange is never honoured.
func TestTokenExchange(t *testing.T) {
	h := newH(t, webtest.Listen(webtest.NonLoopback))
	tok := h.Srv.Token()

	for _, tc := range []struct{ name, path string }{
		{"no token", "/"}, {"wrong token", "/?token=nope"}, {"empty token", "/?token="},
		{"token on another path", "/agents/orch/x?token=" + tok}, {"token on SSE", "/events?token=" + tok},
	} {
		if r := h.Do(h.Raw("GET", tc.path, nil)); r.Code != 401 || h.Cookie != nil {
			t.Errorf("%s: %d cookie=%v, want 401 and no session", tc.name, r.Code, h.Cookie)
		}
	}
	// a write cannot start a session with the token either
	if r := h.Do(h.Browser("POST", "/agents/orch/send?token="+tok, url.Values{"body": {"x"}})); r.Code != 403 || h.Cookie != nil {
		t.Errorf("POST with token = %d", r.Code)
	}

	r := h.Do(h.Raw("GET", "/?token="+tok, nil))
	if r.Code != 303 || r.Header.Get("Location") != "/" {
		t.Fatalf("exchange = %d to %q, want a redirect to / without the token", r.Code, r.Header.Get("Location"))
	}
	if h.Cookie == nil {
		t.Fatal("exchange set no cookie")
	}
	c := r.Header.Get("Set-Cookie")
	for _, want := range []string{"HttpOnly", "SameSite=Strict"} {
		if !strings.Contains(c, want) {
			t.Errorf("session cookie %q lacks %s", c, want)
		}
	}
	// Plain HTTP on a non-loopback host: a Secure cookie would be dropped by the browser and
	// the visitor would loop on the sign-in page, so the attribute must be absent.
	if strings.Contains(c, "Secure") {
		t.Errorf("plain-HTTP session cookie must not be Secure: %q", c)
	}
	if h.Cookie.Value == tok {
		t.Fatal("the session id must not be the token")
	}
	// logged in: the page loads, and a token left in a later query is stripped, not read
	h.CheckPage(h.Get("/"))
	r = h.Get("/?token=" + tok + "&x=1")
	if r.Code != 303 || strings.Contains(r.Header.Get("Location"), "token") || r.Header.Get("Location") != "/?x=1" {
		t.Fatalf("token after exchange: %d -> %q", r.Code, r.Header.Get("Location"))
	}
	// the cookie is the only credential: without it the same token on a deep path fails
	h.Cookie = nil
	if r := h.Do(h.Raw("GET", "/events?token="+tok, nil)); r.Code != 401 {
		t.Fatalf("SSE with token only = %d", r.Code)
	}
	// non-loopback Host: the bound IP or a literal IP with the port; a name is not allowed
	for host, want := range map[string]int{"10.0.0.5:8123": 401, "192.168.1.9:8123": 401, "evil.example:8123": 403, "10.0.0.5:9": 403} {
		req := h.Raw("GET", "/", nil)
		req.Host = host
		if r := h.Do(req); r.Code != want {
			t.Errorf("non-loopback Host %q = %d, want %d", host, r.Code, want)
		}
	}
}

// Loopback needs no token: the first visit creates the session. The cookie is HttpOnly and
// SameSite=Strict, and not Secure (plain HTTP on loopback).
func TestLoopbackAutoSession(t *testing.T) {
	h := newH(t)
	r := h.Do(h.Raw("GET", "/", nil))
	if r.Code != 200 {
		t.Fatalf("first visit = %d", r.Code)
	}
	c := r.Header.Get("Set-Cookie")
	if !strings.Contains(c, "HttpOnly") || !strings.Contains(c, "SameSite=Strict") || strings.Contains(c, "Secure") {
		t.Errorf("cookie flags: %q", c)
	}
	// a token in the URL is never needed, and never left there
	r = h.Do(h.Raw("GET", "/?token=anything", nil))
	if r.Code != 303 || strings.Contains(r.Header.Get("Location"), "token") {
		t.Errorf("loopback token param: %d -> %q", r.Code, r.Header.Get("Location"))
	}
	// a forged or stale cookie is not a session; the visit simply starts a new one
	req := h.Raw("POST", "/agents/orch/send", url.Values{"body": {"x"}})
	req.AddCookie(&http.Cookie{Name: "slai_session", Value: "forged"})
	req.Header.Set("X-Slai-Request", "1")
	if r := h.Do(req); r.Code != 403 {
		t.Errorf("forged cookie write = %d", r.Code)
	}
}

// P1 AC / security AC4: a body over 64 KB is 413 (declared or streamed) and changes
// nothing; a message over 4000 chars is refused with the service's error text.
func TestBodyAndMessageLimits(t *testing.T) {
	h := newH(t)
	h.Login()
	big := url.Values{"body": {strings.Repeat("a", 70<<10)}}
	if r := h.Post("/agents/orch/send", big); r.Code != 413 {
		t.Fatalf("70 KB body = %d, want 413", r.Code)
	}
	req := h.Browser("POST", "/agents/orch/send", big)
	req.ContentLength = -1 // chunked: no declared length
	if r := h.Do(req); r.Code != 413 {
		t.Fatalf("streamed 70 KB body = %d, want 413", r.Code)
	}
	if len(h.Sent()) != 0 || len(h.WebActions()) != 0 {
		t.Fatalf("an oversized request had effects: %v %v", h.Sent(), h.WebActions())
	}

	long := url.Values{"body": {strings.Repeat("é", 4001)}}
	r := h.Post("/agents/orch/send", long)
	if r.Code != 422 || !strings.Contains(r.Body, "message is too long (4001 &gt; 4000 chars)") {
		t.Fatalf("4001-char message = %d %.300s", r.Code, r.Body)
	}
	if len(h.Sent()) != 0 {
		t.Fatal("a too-long message was sent")
	}
	if r := h.Post("/agents/orch/send", url.Values{"body": {strings.Repeat("é", 4000)}}); r.Code != 303 {
		t.Fatalf("4000-char message = %d", r.Code)
	}
	if r := h.Post("/agents/orch/send", url.Values{"body": {""}}); r.Code != 422 || !strings.Contains(r.Body, "message is empty") {
		t.Fatalf("empty message = %d %s", r.Code, r.Body)
	}
}

// Failed actions show the error text verbatim but escaped; htmx requests get a fragment.
func TestFailShowsEscapedErrorText(t *testing.T) {
	h := newH(t)
	h.Login()
	req := h.Browser("DELETE", "/things/%3Cscript%3E", nil)
	req.Header.Set("HX-Request", "true")
	r := h.Do(req)
	if r.Code != 422 || !strings.Contains(r.Body, "refused &lt;b&gt;") || strings.Contains(r.Body, "<b>") {
		t.Fatalf("htmx failure = %d %s", r.Code, r.Body)
	}
	r = h.Do(h.Browser("DELETE", "/things/x", nil))
	if r.Code != 422 || !strings.Contains(r.Body, "refused &lt;b&gt;x&lt;/b&gt;") {
		t.Fatalf("plain failure = %d %s", r.Code, r.Body)
	}
	h.CheckHeaders(r)
}

// Every page and error carries the security headers; the home page passes the page checks.
func TestPageChecksAndHeaders(t *testing.T) {
	h := newH(t)
	h.Login()
	r := h.Get("/")
	h.CheckPage(r)
	for _, want := range []string{"Dashboard", "/static/tabler.min.css", "/static/htmx.min.js", `hx-headers`, "No agents yet", "slai"} {
		if !strings.Contains(r.Body, want) {
			t.Errorf("home page lacks %q", want)
		}
	}
	for _, p := range []string{"/nope", "/static/missing.css", "/boom/x"} {
		h.CheckHeaders(h.Get(p))
	}
	if r := h.Get("/nope"); r.Code != 404 {
		t.Errorf("unknown page = %d", r.Code)
	}
}

// The nav only links pages some module serves.
func TestNavEnablesOnlyServedPages(t *testing.T) {
	h := newH(t)
	h.Login()
	body := h.Get("/").Body
	if !strings.Contains(body, `href="/"`) || strings.Contains(body, `href="/board"`) {
		t.Fatalf("nav should link the dashboard and not the board:\n%s", body)
	}
	h2 := newH(t, webtest.WithModules(func(d *server.Deps) []server.Route {
		return []server.Route{{Method: "GET", Pattern: "/board", Handler: func(w http.ResponseWriter, r *http.Request) {}}}
	}))
	h2.Login()
	if !strings.Contains(h2.Get("/").Body, `href="/board"`) {
		t.Fatal("a module that serves /board should enable its nav entry")
	}
}

// A module's route replaces a shell fallback; two modules on one pattern is an error.
func TestRouteRegistration(t *testing.T) {
	own := func(d *server.Deps) []server.Route {
		return []server.Route{{Method: "GET", Pattern: "/{$}", Handler: func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "mine") }}}
	}
	h := webtest.New(t, webtest.WithModules(own))
	h.Login()
	if h.Get("/").Body != "mine" {
		t.Fatal("module route did not replace the shell placeholder")
	}
	opts := server.Options{Listen: "127.0.0.1:0", Modules: []server.Module{own, own}}
	if _, err := server.New(opts); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("duplicate route: %v", err)
	}
	bad := func(*server.Deps) []server.Route {
		return []server.Route{{Method: "POST", Pattern: "/x", Handler: func(http.ResponseWriter, *http.Request) {}}}
	}
	if _, err := server.New(server.Options{Listen: "127.0.0.1:0", Modules: []server.Module{bad}}); err == nil || !strings.Contains(err.Error(), "Action") {
		t.Fatalf("a write without an Action: %v", err)
	}
	for _, m := range []string{"PUT", "PATCH", "OPTIONS"} {
		mod := func(*server.Deps) []server.Route {
			return []server.Route{{Method: m, Pattern: "/x", Action: "a", Handler: func(http.ResponseWriter, *http.Request) {}}}
		}
		if _, err := server.New(server.Options{Listen: "127.0.0.1:0", Modules: []server.Module{mod}}); err == nil {
			t.Errorf("method %s should be rejected", m)
		}
	}
	if _, err := server.New(server.Options{Listen: "not-an-address"}); err == nil {
		t.Fatal("bad --listen accepted")
	}
}

// P1 AC4: static assets load locally with their types; the pinned notes are not served;
// no traversal.
func TestStaticAssets(t *testing.T) {
	h := newH(t)
	for path, ct := range map[string]string{"/static/tabler.min.css": "text/css", "/static/htmx.min.js": "text/javascript", "/static/sortable.min.js": "text/javascript", "/static/slai.css": "text/css"} {
		r := h.Do(h.Raw("GET", path, nil)) // no session needed
		if r.Code != 200 || !strings.Contains(r.Header.Get("Content-Type"), ct) || r.Header.Get("ETag") == "" {
			t.Errorf("%s = %d %q etag=%q", path, r.Code, r.Header.Get("Content-Type"), r.Header.Get("ETag"))
		}
		h.CheckHeaders(r)
		req := h.Raw("GET", path, nil)
		req.Header.Set("If-None-Match", r.Header.Get("ETag"))
		if r2 := h.Do(req); r2.Code != 304 {
			t.Errorf("%s with matching ETag = %d, want 304", path, r2.Code)
		}
	}
	for _, p := range []string{"/static/VENDOR.md", "/static/", "/static/../x", "/static/a/b", "/static/%2e%2e/main.go"} {
		if r := h.Do(h.Raw("GET", p, nil)); r.Code == 200 {
			t.Errorf("%s served", p)
		}
	}
	if r := h.Do(h.Raw("POST", "/static/htmx.min.js", nil)); r.Code == 200 {
		t.Error("POST to static")
	}
	// assets are not a way around the Host check
	req := h.Raw("GET", "/static/htmx.min.js", nil)
	req.Host = "evil.example:8123"
	if r := h.Do(req); r.Code != 403 {
		t.Errorf("static with foreign Host = %d", r.Code)
	}
}

func TestThemeCookie(t *testing.T) {
	h := newH(t)
	h.Login()
	r := h.Get("/theme?set=dark")
	if r.Code != 303 || !strings.Contains(r.Header.Get("Set-Cookie"), "slai_theme=dark") {
		t.Fatalf("theme = %d %q", r.Code, r.Header.Get("Set-Cookie"))
	}
}

func TestSupervisorBanner(t *testing.T) {
	running := false
	h := newH(t, webtest.SupervisorRunning(func() bool { return running }))
	h.Login()
	if !strings.Contains(h.Get("/").Body, "supervisor is not running") {
		t.Fatal("no banner while the supervisor is down")
	}
	running = true
	if strings.Contains(h.Get("/").Body, "supervisor is not running") {
		t.Fatal("banner shown while the supervisor is up")
	}
}

func TestStreamsPerSessionLimit(t *testing.T) {
	var got []bool
	var sess *server.Session
	h := webtest.New(t, webtest.WithModules(func(d *server.Deps) []server.Route {
		return []server.Route{{Method: "GET", Pattern: "/s", Handler: func(w http.ResponseWriter, r *http.Request) { sess = d.Session(r) }}}
	}))
	h.Login()
	h.Get("/s")
	var rel []func()
	for i := 0; i < 9; i++ {
		f, ok := sess.AcquireStream(8)
		got = append(got, ok)
		if ok {
			rel = append(rel, f)
		}
	}
	if got[7] != true || got[8] != false {
		t.Fatalf("8 streams allowed, 9th refused: %v", got)
	}
	rel[0]()
	rel[0]() // release is idempotent
	if _, ok := sess.AcquireStream(8); !ok {
		t.Fatal("a released slot should be reusable")
	}
}

// The Secure attribute follows the request: present when the browser used HTTPS (directly or
// behind a TLS-terminating proxy that sets X-Forwarded-Proto), absent over plain HTTP.
func TestSessionCookieSecureFollowsTheRequest(t *testing.T) {
	for name, tc := range map[string]struct {
		mod  func(*http.Request)
		want bool
	}{
		"plain http":             {func(*http.Request) {}, false},
		"x-forwarded-proto http": {func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "http") }, false},
		"x-forwarded-proto":      {func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "https") }, true},
		"x-forwarded-proto case": {func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "HTTPS") }, true},
		"tls":                    {func(r *http.Request) { r.TLS = &tls.ConnectionState{} }, true},
	} {
		for _, bind := range []string{webtest.Addr, webtest.NonLoopback} {
			h := newH(t, webtest.Listen(bind))
			// the token exchange: a proxied request (X-Forwarded-Proto) needs it on a loopback bind too
			req := h.Raw("GET", "/?token="+h.Srv.Token(), nil)
			tc.mod(req)
			r := h.Do(req)
			c := r.Header.Get("Set-Cookie")
			if got := strings.Contains(c, "Secure"); got != tc.want || !strings.Contains(c, "HttpOnly") || !strings.Contains(c, "SameSite=Strict") {
				t.Errorf("%s on %s: Set-Cookie %q, Secure want %v", name, bind, c, tc.want)
			}
		}
	}
	// the theme cookie follows the same rule
	h := newH(t)
	h.Login()
	req := h.Raw("GET", "/theme?set=dark", nil)
	req.AddCookie(h.Cookie)
	req.Header.Set("X-Forwarded-Proto", "https")
	if c := h.Do(req).Header.Get("Set-Cookie"); !strings.Contains(c, "Secure") {
		t.Errorf("theme cookie behind https: %q", c)
	}
}

// ---- W1b (T-0101): focus-group addendum ACs 1-4 ----

// AC1: a non-loopback bind over plain HTTP needs --insecure-http, with exactly this error.
func TestNonLoopbackNeedsInsecureHTTP(t *testing.T) {
	const want = "bind 127.0.0.1 and put a TLS proxy in front, or pass --insecure-http on a trusted LAN"
	for _, listen := range []string{"0.0.0.0:8080", ":8080", "10.0.0.5:8080", "[::]:8080"} {
		_, err := server.New(server.Options{Listen: listen})
		if err == nil || err.Error() != want {
			t.Errorf("%s without the flag: %v, want exactly %q", listen, err, want)
		}
		if _, err := server.New(server.Options{Listen: listen, InsecureHTTP: true}); err != nil {
			t.Errorf("%s with the flag: %v", listen, err)
		}
	}
	for _, listen := range []string{"127.0.0.1:0", "localhost:8080", "[::1]:8080"} {
		if _, err := server.New(server.Options{Listen: listen}); err != nil {
			t.Errorf("loopback %s must not need the flag: %v", listen, err)
		}
	}
}

// AC1: --insecure-http is an acknowledgement only. Auth, the token and every check stay on,
// and the flag never shows in the URL line.
func TestInsecureHTTPDoesNotDisableAuth(t *testing.T) {
	h := newH(t, webtest.Listen(webtest.NonLoopback)) // the harness passes InsecureHTTP
	if r := h.Do(h.Raw("GET", "/", nil)); r.Code != 401 {
		t.Fatalf("no token = %d, want 401", r.Code)
	}
	if r := h.Do(h.Raw("POST", "/agents/orch/send", url.Values{"body": {"x"}})); r.Code != 403 {
		t.Fatalf("write without a session = %d", r.Code)
	}
	h.Login()
	if r := h.Do(h.Browser("POST", "/agents/orch/send", url.Values{"body": {"x"}})); r.Code == 403 {
		t.Fatalf("a signed-in write should work: %d", r.Code)
	}
	req := h.Browser("POST", "/agents/orch/send", url.Values{"body": {"x"}})
	req.Header.Del("X-Slai-Request")
	if r := h.Do(req); r.Code != 403 {
		t.Fatalf("CSRF header still required: %d", r.Code)
	}
}

// startLine runs Serve on a non-loopback address long enough to read the line it prints.
func startLine(t *testing.T) string {
	t.Helper()
	var out lockedBuf
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, server.Options{Listen: "0.0.0.0:0", InsecureHTTP: true, Out: &out}) }()
	for i := 0; i < 200 && out.String() == ""; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	got := out.String()
	cancel()
	<-done
	if got == "" {
		t.Fatal("serve printed nothing")
	}
	return got
}

type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// AC2: every start generates a new token; the URL line never mentions the flag; the sign-in
// page says where the token is safe and how to rotate it.
func TestTokenRotatesAndSignInPageExplains(t *testing.T) {
	tok := regexp.MustCompile(`token=([0-9a-f]{64})`)
	a, b := tok.FindStringSubmatch(startLine(t)), tok.FindStringSubmatch(startLine(t))
	if a == nil || b == nil || a[1] == b[1] {
		t.Fatalf("two starts must print two different tokens: %v %v", a, b)
	}
	if line := startLine(t); strings.Contains(line, "insecure") {
		t.Errorf("the flag appears in the URL line: %q", line)
	}
	h := newH(t, webtest.Listen(webtest.NonLoopback))
	r := h.Do(h.Raw("GET", "/", nil))
	for _, want := range []string{"trusted LAN", "plain HTTP", "new token is generated every time", "restarting it rotates the token"} {
		if r.Code != 401 || !strings.Contains(r.Body, want) {
			t.Errorf("sign-in page (%d) lacks %q", r.Code, want)
		}
	}
}

// AC3: refusals on a request with a valid session become web_denied events and count in the
// footer; refusals without a session are only counted in memory, never stored.
func TestWebDeniedEventsAndFooter(t *testing.T) {
	h := newH(t)
	h.Login()
	// valid session: missing header, bad Origin
	req := h.Browser("POST", "/agents/orch/send", url.Values{"body": {"x"}})
	req.Header.Del("X-Slai-Request")
	h.Do(req)
	req = h.Browser("POST", "/agents/orch/send", url.Values{"body": {"x"}})
	req.Header.Set("Origin", "http://evil.example")
	h.Do(req)
	evs := h.EventsOfKind("web_denied")
	if len(evs) != 2 {
		t.Fatalf("want 2 web_denied events, got %v", evs)
	}
	got := []string{evs[0]["reason"].(string), evs[1]["reason"].(string)}
	if got[0] != "missing_csrf_header" || got[1] != "bad_origin" || evs[0]["route"] != "POST /agents/{id}/send" || evs[0]["actor"] != "user" {
		t.Fatalf("events %v", evs)
	}
	if !strings.Contains(h.Get("/").Body, "2 writes refused this session") {
		t.Fatalf("footer lacks the count:\n%s", h.Get("/").Body)
	}
	if len(h.WebActions()) != 0 {
		t.Fatal("a refused write must not record a web_action")
	}

	// read-only
	ro := newH(t, webtest.ReadOnly())
	ro.Login()
	ro.Do(ro.Browser("POST", "/agents/orch/send", url.Values{"body": {"x"}}))
	if evs := ro.EventsOfKind("web_denied"); len(evs) != 1 || evs[0]["reason"] != "read_only" {
		t.Fatalf("read-only denial: %v", evs)
	}
	if !strings.Contains(ro.Get("/").Body, "1 write refused this session") {
		t.Fatal("singular footer text")
	}

	// no session: 1000 POSTs, foreign Hosts and bad tokens leave the events table unchanged
	before := len(h.AllEvents())
	h2 := newH(t, webtest.Listen(webtest.NonLoopback))
	for i := 0; i < 1000; i++ {
		h2.Do(h2.Raw("POST", "/agents/orch/send", url.Values{"body": {"x"}}))
	}
	req = h2.Raw("GET", "/", nil)
	req.Host = "evil.example:8123"
	h2.Do(req)
	h2.Do(h2.Raw("GET", "/?token=wrong", nil))
	if n := len(h2.AllEvents()); n != 0 {
		t.Fatalf("unauthenticated refusals wrote %d events", n)
	}
	_ = before
	h2.Login()
	if body := h2.Get("/").Body; !strings.Contains(body, "1002 unauthenticated requests refused since start") {
		t.Fatalf("in-memory counter missing from the footer: %s", body)
	}
}

// A signed-in client cannot fill the events table: the counter goes on, the events stop.
func TestWebDeniedEventsAreCapped(t *testing.T) {
	h := newH(t, webtest.ReadOnly())
	h.Login()
	for i := 0; i < 250; i++ {
		h.Do(h.Browser("POST", "/agents/orch/send", url.Values{"body": {"x"}}))
	}
	if n := len(h.EventsOfKind("web_denied")); n != 100 {
		t.Fatalf("stored %d events, want the cap of 100", n)
	}
	if !strings.Contains(h.Get("/").Body, "250 writes refused this session") {
		t.Fatal("the footer should still count every refusal")
	}
}

// AC4: /capabilities is generated from the route table: one row per route, no more, lists
// itself as a read-only GET, and the footer links to it.
func TestCapabilitiesPageEqualsRouteTable(t *testing.T) {
	h := newH(t)
	h.Login()
	r := h.Get("/capabilities")
	h.CheckPage(r)
	rows := regexp.MustCompile(`<tr><td><code>([A-Z]+)</code></td><td><code>([^<]*)</code></td><td>([^<]*)</td>`).FindAllStringSubmatch(r.Body, -1)
	got := map[string]string{}
	for _, m := range rows {
		got[m[1]+" "+html.UnescapeString(m[2])] = m[3]
	}
	want := map[string]string{}
	for _, rt := range h.Srv.Routes() {
		if rt.Write() {
			want[rt.Method+" "+rt.Pattern] = "yes"
		} else {
			want[rt.Method+" "+rt.Pattern] = "no (read-only)"
		}
	}
	if len(got) != len(want) || len(rows) != len(want) {
		t.Fatalf("page has %d rows (%v), route table has %d (%v)", len(rows), got, len(want), want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: page says %q, table says %q", k, got[k], v)
		}
	}
	if got["GET /capabilities"] != "no (read-only)" {
		t.Error("/capabilities must list itself as a read-only GET")
	}
	for _, rt := range h.Srv.Routes() {
		if rt.Write() && (rt.Calls == "" || !strings.Contains(r.Body, html.EscapeString(rt.Calls))) {
			t.Errorf("write route %s %s: what it calls is missing", rt.Method, rt.Pattern)
		}
	}
	if !strings.Contains(h.Get("/").Body, `href="/capabilities"`) || !strings.Contains(h.Get("/").Body, "never approves a review, merges, commits") {
		t.Error("footer claim and link missing")
	}
	// a module's routes appear in it too
	h2 := newH(t, webtest.ReadOnly())
	h2.Login()
	if !strings.Contains(h2.Get("/capabilities").Body, "every write below is refused") {
		t.Error("read-only mode should be stated on the page")
	}
}
