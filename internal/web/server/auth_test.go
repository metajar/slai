package server_test

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/web/server"
	"github.com/oorbt/slai/internal/web/webtest"
)

func cookiesOf(r *webtest.Resp) []*http.Cookie { return (&http.Response{Header: r.Header}).Cookies() }

// T-0111 AC1+AC2+AC3: behind a reverse proxy a loopback bind does not sign the first visitor
// in; with the token the proxied request gets a session; a plain loopback visit still does.
func TestNoAutomaticSessionBehindAProxy(t *testing.T) {
	h := newH(t, webtest.AllowHosts("slai.example.org:8123"))
	proxied := map[string]func(*http.Request){
		"allow-host name": func(r *http.Request) { r.Host = "slai.example.org:8123" },
		"X-Forwarded-For": func(r *http.Request) { r.Header.Set("X-Forwarded-For", "198.51.100.4") },
		"both": func(r *http.Request) {
			r.Host = "slai.example.org:8123"
			r.Header.Set("X-Forwarded-For", "198.51.100.4")
		},
		"Forwarded": func(r *http.Request) { r.Header.Set("Forwarded", "for=198.51.100.4") },
	}
	for name, mod := range proxied {
		// AC1: no token, so the sign-in page and no session cookie
		req := h.Raw("GET", "/", nil)
		mod(req)
		r := h.Do(req)
		if r.Code != 401 || len(cookiesOf(r)) != 0 || !strings.Contains(r.Body, "Sign in") {
			t.Errorf("%s: GET / = %d with %d cookies, want the sign-in page and no session", name, r.Code, len(cookiesOf(r)))
		}
		// AC2: with the token, a session (and the token leaves the URL)
		req = h.Raw("GET", "/?token="+h.Srv.Token(), nil)
		mod(req)
		r = h.Do(req)
		if r.Code != 303 || r.Header.Get("Location") != "/" || len(cookiesOf(r)) == 0 {
			t.Errorf("%s: token exchange = %d, Location %q, %d cookies", name, r.Code, r.Header.Get("Location"), len(cookiesOf(r)))
		}
	}
	// AC3: a plain loopback request is still signed in
	h.Cookie = nil
	r := h.Do(h.Raw("GET", "/", nil))
	if r.Code != 200 || len(cookiesOf(r)) == 0 {
		t.Fatalf("plain loopback = %d with %d cookies, want an automatic session", r.Code, len(cookiesOf(r)))
	}
	// a session made automatically for a loopback visit keeps working for a later proxied request
	// only because the cookie is its own proof; without one the proxied request is refused
	req := h.Raw("GET", "/board", nil)
	req.Header.Set("X-Forwarded-For", "198.51.100.4")
	if r := h.Do(req); r.Code != 401 {
		t.Errorf("a proxied request without the cookie = %d, want 401", r.Code)
	}
}

// startMsg runs Serve on loopback and returns what it printed.
func startMsg(t *testing.T, allow ...string) string {
	t.Helper()
	var out lockedBuf
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, server.Options{Listen: "127.0.0.1:0", AllowHosts: allow, Out: &out}) }()
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

// T-0111 AC5: with --allow-host the startup output prints the token URL for each name; without
// it a loopback start prints no token (nothing needs one).
func TestStartupPrintsTheTokenURLWhenAllowHostIsSet(t *testing.T) {
	got := startMsg(t, "slai.example.org:8123", "other.example.org:8123")
	for _, h := range []string{"slai.example.org:8123", "other.example.org:8123"} {
		if !regexp.MustCompile(`https://` + regexp.QuoteMeta(h) + `/\?token=[0-9a-f]{64}`).MatchString(got) {
			t.Errorf("no token URL for %s in:\n%s", h, got)
		}
	}
	if !strings.Contains(got, "open http://127.0.0.1:") || strings.Contains(got, "insecure") {
		t.Errorf("local URL missing or the flag leaked:\n%s", got)
	}
	if plain := startMsg(t); strings.Contains(plain, "token") {
		t.Errorf("a plain loopback start needs no token:\n%s", plain)
	}
}
