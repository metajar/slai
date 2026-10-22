package security_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/web/webtest"
)

func setCookies(r *webtest.Resp) []*http.Cookie {
	return (&http.Response{Header: r.Header}).Cookies()
}

// W8 addendum: on a non-loopback bind a name is refused unless --allow-host lists it, an IP
// literal with the bound port is accepted (it cannot be rebound), and the token exchange works
// under an allowed name. Reads and writes, normal and SSE-style routes alike.
func TestAllowHostOnANonLoopbackBind(t *testing.T) {
	deny := newSeeded(t, webtest.Listen(webtest.NonLoopback))
	allow := newSeeded(t, webtest.Listen(webtest.NonLoopback), webtest.AllowHosts("slai.lan:8123"))
	get := func(h *seeded, host, path string) *webtest.Resp {
		req := h.Raw("GET", path, nil)
		req.Host = host
		return h.Do(req)
	}
	for _, path := range []string{"/", "/agents", "/board", "/static/slai.css", "/events"} {
		if r := get(deny, "slai.lan:8123", path+"?token=test-token"); r.Code != 403 {
			t.Errorf("GET %s via an unlisted name = %d, want 403", path, r.Code)
		}
	}
	if r := get(allow, "slai.lan:8123", "/?token=test-token"); r.Code != 303 && r.Code != 200 {
		t.Errorf("token exchange under an allowed name = %d", r.Code)
	}
	for _, host := range []string{"other.lan:8123", "slai.lan:9999", "slai.lan", "slai.lan:8123.evil.example", "evil.example:8123"} {
		if r := get(allow, host, "/"); r.Code != 403 {
			t.Errorf("Host %q = %d with only slai.lan:8123 allowed", host, r.Code)
		}
	}
	// the allowed name does not relax the write checks
	allow.Login()
	for _, r := range allow.writeRoutes() {
		req := allow.Browser(r.method, r.path, form())
		req.Host = "slai.lan:8123"
		req.Header.Set("Origin", "http://evil.example")
		if resp := allow.Do(req); resp.Code != 403 {
			t.Errorf("%s %s with a foreign Origin under an allowed host = %d", r.method, r.pattern, resp.Code)
		}
	}
}

// W8 addendum: the Secure flag of the cookies follows the request (TLS or X-Forwarded-Proto),
// never the bind address; both cookies are HttpOnly (session) and SameSite=Strict.
func TestCookieFlagsFollowTheRequest(t *testing.T) {
	h := newSeeded(t, webtest.Listen(webtest.NonLoopback))
	for _, c := range []struct {
		name   string
		header string
		secure bool
	}{{"plain http", "", false}, {"behind a TLS proxy", "https", true}, {"proxy says http", "http", false}} {
		req := h.Raw("GET", "/?token=test-token", nil)
		if c.header != "" {
			req.Header.Set("X-Forwarded-Proto", c.header)
		}
		resp := h.Do(req)
		var sess *http.Cookie
		for _, ck := range setCookies(resp) {
			if ck.Name == "slai_session" {
				sess = ck
			}
		}
		if sess == nil {
			t.Fatalf("%s: no session cookie (status %d)", c.name, resp.Code)
		}
		if sess.Secure != c.secure || !sess.HttpOnly || sess.SameSite != http.SameSiteStrictMode {
			t.Errorf("%s: session cookie Secure=%v HttpOnly=%v SameSite=%v", c.name, sess.Secure, sess.HttpOnly, sess.SameSite)
		}
		if strings.Contains(resp.Header.Get("Location"), "token") {
			t.Errorf("%s: the redirect keeps the token: %s", c.name, resp.Header.Get("Location"))
		}
	}
}

// W8 addendum: --insecure-http does not turn authentication off: without a session a
// non-loopback server serves no page, with the flag or without.
func TestNoSessionNoPageEvenWithInsecureHTTP(t *testing.T) {
	h := newSeeded(t, webtest.Listen(webtest.NonLoopback))
	for _, rt := range h.Srv.Routes() {
		if rt.Write() || rt.Pattern == "/theme" {
			continue
		}
		for _, p := range paths(rt.Pattern, []string{"eng-1"}) {
			resp := h.Do(h.Raw("GET", p, nil))
			if resp.Code == 200 && !strings.Contains(resp.Body, "token") {
				t.Errorf("GET %s answered 200 without a session on a non-loopback bind", p)
			}
			for _, leak := range []string{"eng-1", "Build the"} {
				if resp.Code == 200 && strings.Contains(resp.Body, leak+"</") {
					t.Errorf("GET %s leaked team data without a session", p)
				}
			}
		}
	}
	if r := h.Do(h.Raw("GET", "/?token=wrong", nil)); r.Code == 200 && len(setCookies(r)) > 0 {
		t.Error("a wrong token started a session")
	}
}

// T-0111: behind a reverse proxy on the same host, the loopback bind must not sign the first
// visitor in. The automatic session is for a request whose own Host is a loopback name and
// that carries no forwarding header; anything else does the token exchange, on every GET
// route of the real route table.
func TestLoopbackBindDoesNotAutoSignInProxiedRequests(t *testing.T) {
	h := newSeeded(t, webtest.AllowHosts("slai.example.org:8123"))
	proxied := []struct {
		name string
		mod  func(*http.Request)
	}{
		{"an --allow-host name", func(r *http.Request) { r.Host = "slai.example.org:8123" }},
		{"X-Forwarded-For on a loopback Host", func(r *http.Request) { r.Header.Set("X-Forwarded-For", "203.0.113.7") }},
		{"Forwarded", func(r *http.Request) { r.Header.Set("Forwarded", "for=203.0.113.7;proto=https") }},
		{"X-Forwarded-Host", func(r *http.Request) { r.Header.Set("X-Forwarded-Host", "slai.example.org") }},
		{"X-Forwarded-Proto", func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "https") }},
		{"X-Real-IP", func(r *http.Request) { r.Header.Set("X-Real-IP", "203.0.113.7") }},
	}
	n := 0
	for _, rt := range h.Srv.Routes() {
		if rt.Write() || rt.Pattern == "/theme" {
			continue
		}
		for _, p := range paths(rt.Pattern, []string{"eng-1"}) {
			n++
			for _, c := range proxied {
				req := h.Raw("GET", p, nil)
				c.mod(req)
				r := h.Do(req)
				if r.Code != 401 || len(setCookies(r)) != 0 || !strings.Contains(r.Body, "Sign in") {
					t.Errorf("GET %s via %s = %d with %d cookie(s), want the sign-in page and no session", p, c.name, r.Code, len(setCookies(r)))
				}
			}
		}
	}
	if n < 10 {
		t.Fatalf("only %d GET paths exercised", n)
	}
	// a write through the proxy without a session is refused and starts no session either
	req := h.Browser("POST", "/agents/orch/send", form())
	req.Host = "slai.example.org:8123"
	h.Cookie = nil
	req.Header.Del("Cookie")
	if r := h.Do(req); r.Code != 403 || len(setCookies(r)) != 0 {
		t.Errorf("proxied write without a session = %d", r.Code)
	}
}

// T-0111: with the token a proxied request gets a session (exchange on /, then the token is
// stripped); a plain loopback visit is still signed in automatically.
func TestProxiedExchangeWorksAndPlainLoopbackStillAutoSignsIn(t *testing.T) {
	h := newSeeded(t, webtest.AllowHosts("slai.example.org:8123"))
	exchange := func(mod func(*http.Request)) *webtest.Resp {
		req := h.Raw("GET", "/?token=test-token", nil)
		mod(req)
		return h.Do(req)
	}
	for name, mod := range map[string]func(*http.Request){
		"allowed name":       func(r *http.Request) { r.Host = "slai.example.org:8123" },
		"forwarded loopback": func(r *http.Request) { r.Header.Set("X-Forwarded-For", "203.0.113.7") },
	} {
		r := exchange(mod)
		if r.Code != 303 || r.Header.Get("Location") != "/" || len(setCookies(r)) == 0 {
			t.Errorf("%s: token exchange = %d (Location %q, %d cookies)", name, r.Code, r.Header.Get("Location"), len(setCookies(r)))
		}
		if bad := exchangeAt(h, mod, "/agents?token=test-token"); bad.Code != 401 {
			t.Errorf("%s: a token on another path = %d, want 401", name, bad.Code)
		}
		if wrong := exchangeAt(h, mod, "/?token=nope"); wrong.Code != 401 || len(setCookies(wrong)) != 0 {
			t.Errorf("%s: a wrong token = %d", name, wrong.Code)
		}
	}
	for _, host := range []string{"127.0.0.1:8123", "localhost:8123", "[::1]:8123"} {
		req := h.Raw("GET", "/", nil)
		req.Host = host
		r := h.Do(req)
		if r.Code != 200 || len(setCookies(r)) == 0 {
			t.Errorf("plain loopback Host %s = %d with %d cookies, want an automatic session", host, r.Code, len(setCookies(r)))
		}
	}
}

func exchangeAt(h *seeded, mod func(*http.Request), path string) *webtest.Resp {
	req := h.Raw("GET", path, nil)
	mod(req)
	return h.Do(req)
}
