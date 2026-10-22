package sse_test

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/web/sse"
	"github.com/oorbt/slai/internal/web/webtest"
)

// open starts a GET /events against a real listener, as the logged-in browser, with the
// Host the server expects.
func open(t *testing.T, h *webtest.Harness, ts *httptest.Server, hdr map[string]string) (*http.Response, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/events", nil)
	req.Host = webtest.Addr
	req.AddCookie(h.Cookie)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	return res, cancel
}

func harness(t *testing.T) (*webtest.Harness, *httptest.Server) {
	t.Helper()
	h := webtest.New(t, webtest.WithModules(sse.Module(nil)))
	h.Login()
	ts := httptest.NewServer(h.Srv.Handler())
	t.Cleanup(ts.Close)
	return h, ts
}

// P12 AC2: a store change on a seeded event reaches the browser within 1 s.
func TestStreamDeliversNoticeWithin1s(t *testing.T) {
	h, ts := harness(t)
	res, cancel := open(t, h, ts, nil)
	defer cancel()
	defer res.Body.Close()
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d type %q", res.StatusCode, res.Header.Get("Content-Type"))
	}
	if res.Header.Get("Content-Encoding") != "" {
		t.Fatal("the stream must not be compressed")
	}
	h.CheckHeaders(&webtest.Resp{Code: res.StatusCode, Header: res.Header})
	rd := bufio.NewReader(res.Body)
	if l, _ := rd.ReadString('\n'); !strings.HasPrefix(l, "retry:") {
		t.Fatalf("first line %q", l)
	}
	time.Sleep(50 * time.Millisecond) // the stream is open; now change the store
	start := time.Now()
	if err := h.Store.AddEvent("eng-1", "update_task", `{"task_id":"T-0001"}`); err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 1)
	go func() {
		for {
			l, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			if strings.HasPrefix(l, "data: ") && strings.Contains(l, `"topic":"tasks"`) {
				got <- l
				return
			}
		}
	}()
	select {
	case l := <-got:
		if d := time.Since(start); d > time.Second {
			t.Fatalf("notice took %v", d)
		}
		if strings.Contains(l, "<") {
			t.Fatalf("notice carries markup: %s", l)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no notice")
	}
}

// Addendum AC6: the 9th stream of a session is refused.
func TestNinthStreamRefused(t *testing.T) {
	h, ts := harness(t)
	for i := 0; i < sse.DefaultMaxPerSession; i++ {
		res, cancel := open(t, h, ts, nil)
		defer cancel()
		defer res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("stream %d: status %d", i+1, res.StatusCode)
		}
	}
	res, cancel := open(t, h, ts, nil)
	defer cancel()
	defer res.Body.Close()
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("9th stream: status %d, want 429", res.StatusCode)
	}
}

// A closed stream frees its slot.
func TestClosedStreamFreesSlot(t *testing.T) {
	h, ts := harness(t)
	var cancels []context.CancelFunc
	for i := 0; i < sse.DefaultMaxPerSession; i++ {
		res, cancel := open(t, h, ts, nil)
		defer res.Body.Close()
		cancels = append(cancels, cancel)
	}
	cancels[0]()
	time.Sleep(200 * time.Millisecond)
	res, cancel := open(t, h, ts, nil)
	defer cancel()
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status %d after a stream closed", res.StatusCode)
	}
	for _, c := range cancels {
		c()
	}
}

// The SSE route sits behind the same Host/Origin/session checks as every route.
func TestStreamChecksHostOriginAndSession(t *testing.T) {
	h := webtest.New(t, webtest.WithModules(sse.Module(nil)))
	h.Login()
	for name, mod := range map[string]func(*http.Request){
		"foreign host":   func(r *http.Request) { r.Host = "evil.example:8123" },
		"foreign origin": func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") },
	} {
		req := h.Raw("GET", "/events", nil)
		req.AddCookie(h.Cookie)
		mod(req)
		if r := h.Do(req); r.Code != 403 {
			t.Errorf("%s: status %d, want 403", name, r.Code)
		}
	}
	// a token left in the query string is never honoured as a credential
	req := h.Raw("GET", "/events?token=test-token", nil)
	if r := h.Do(req); r.Code == 200 {
		t.Error("token in the query string opened a stream with no session")
	}
}

// Addendum AC6 over HTTP: a Last-Event-ID more than 500 events behind gets a resync.
func TestLastEventIDTooFarBehindGetsResync(t *testing.T) {
	h, ts := harness(t)
	for i := 0; i < 600; i++ {
		_ = h.Store.AddEvent("orch", "update_task", `{"task_id":"T-1"}`)
	}
	res, cancel := open(t, h, ts, map[string]string{"Last-Event-ID": "1"})
	defer cancel()
	defer res.Body.Close()
	rd := bufio.NewReader(res.Body)
	got := make(chan string, 1)
	go func() {
		for {
			l, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			if strings.HasPrefix(l, "event: ") {
				got <- strings.TrimSpace(l)
				return
			}
		}
	}()
	select {
	case ev := <-got:
		if ev != "event: resync" {
			t.Fatalf("first event %q, want resync", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no frame")
	}
}

// T-0106: ?watch= names the panes a page is looking at; only well-formed pane topics count, at
// most MaxWatch, and they end with the stream.
func TestWatchQueryRegistersOnlyWellFormedPaneTopics(t *testing.T) {
	var hub *sse.Hub
	h := webtest.New(t, webtest.WithModules(sse.Module(func(x *sse.Hub) { hub = x })))
	h.Login()
	ts := httptest.NewServer(h.Srv.Handler())
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/events?watch="+
		"pane:eng-1,pane:orch,task:T-1,pane:..%2Fx,pane:,pane:a%20b,pane:eng-1,"+strings.Repeat("pane:x,", 20), nil)
	req.Host = webtest.Addr
	req.AddCookie(h.Cookie)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	deadline := time.Now().Add(2 * time.Second)
	var got []string
	for time.Now().Before(deadline) {
		if got = hub.Watched(""); len(got) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	want := map[string]bool{"pane:eng-1": true, "pane:orch": true, "pane:x": true}
	if len(got) != len(want) {
		t.Fatalf("watched = %v, want eng-1, orch, x panes only", got)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("watched %q", g)
		}
	}
	cancel()
	for time.Now().Before(deadline.Add(2 * time.Second)) {
		if len(hub.Watched("")) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("watch outlived its stream: %v", hub.Watched(""))
}
