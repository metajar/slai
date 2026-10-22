package cmd

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/pflag"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/web"
)

// Security AC2: no flag turns authentication off. The flag set is enumerated: only
// --listen, its alias, --read-only and --insecure-http exist (plus cobra's help), and none names auth, a
// token or the like.
func TestServeHasNoFlagThatDisablesAuth(t *testing.T) {
	var names []string
	serveCmd().Flags().VisitAll(func(f *pflag.Flag) { names = append(names, f.Name) })
	// --insecure-http only acknowledges plain HTTP off-loopback; TestInsecureHTTPDoesNotDisableAuth
	// (internal/web/server) proves it leaves sessions, the token and every check on.
	allowed := map[string]bool{"listen": true, "addr": true, "read-only": true, "insecure-http": true, "allow-host": true,
		// the hidden demo flags serve a seeded fake project in its own temp directory; they
		// change nothing about authentication (the demo server has sessions like any other)
		"demo": true, "demo-dir": true, "demo-first-run": true}
	for _, n := range names {
		if !allowed[n] {
			t.Errorf("unexpected serve flag --%s: a new flag must not weaken authentication; extend this test deliberately", n)
		}
		if n != "insecure-http" && n != "allow-host" && regexp.MustCompile(`(?i)auth|token|insecure|no-?login|anonymous|open|trust`).MatchString(n) {
			t.Errorf("--%s looks like an authentication switch", n)
		}
	}
	if len(names) != len(allowed) {
		t.Errorf("flags = %v", names)
	}
}

// AC: `slai serve` starts on a random loopback port, prints the URL, and a visit there
// works end to end (session, page, a write with and without the CSRF header).
func TestServeStartsAndAnswers(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	team := config.Preset("solo", "p", "d", "main")
	var out safeBuf
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- web.Serve(ctx, web.Options{Dir: dir, Store: st, Team: func() *config.Team { return team }, Out: &out})
	}()
	var url string
	for i := 0; i < 100 && url == ""; i++ {
		if m := regexp.MustCompile(`http://127\.0\.0\.1:\d+/`).FindString(out.String()); m != "" {
			url = m
		}
		time.Sleep(20 * time.Millisecond)
	}
	if url == "" {
		t.Fatalf("no URL printed: %q", out.String())
	}
	if strings.Contains(out.String(), "token") {
		t.Errorf("a loopback URL needs no token: %q", out.String())
	}
	jar := &simpleJar{}
	cl := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(method, u string, hdr map[string]string) (int, string) {
		req, _ := http.NewRequest(method, u, strings.NewReader("body=hi"))
		for _, c := range jar.cookies {
			req.AddCookie(c)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		jar.cookies = append(jar.cookies, resp.Cookies()...)
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("GET", url, nil); code != 200 || !strings.Contains(body, "Dashboard") {
		t.Fatalf("GET / = %d %.200s", code, body)
	}
	if code, _ := get("POST", url+"agents/orch/send", nil); code != 403 {
		t.Errorf("write without the CSRF header = %d, want 403", code)
	}
	if code, _ := get("POST", url+"agents/orch/send", map[string]string{"X-Slai-Request": "1", "Origin": strings.TrimSuffix(url, "/")}); code == 403 {
		t.Errorf("write with the header was refused")
	}
	if code, _ := get("GET", strings.Replace(url, "127.0.0.1", "localhost", 1)+"static/htmx.min.js", nil); code != 200 {
		t.Errorf("localhost host name = %d", code)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop")
	}
}

type simpleJar struct{ cookies []*http.Cookie }

// safeBuf is written by the server goroutine and read by the test.
type safeBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *safeBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *safeBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }
