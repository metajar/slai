package security_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// W8: the SSE limits over the real hub, through the whole handler chain (Host, Origin,
// session) on a real listener: spec P12 AC2 and the principal's addendum AC6. The hub's own
// unit tests (internal/web/sse) cover the 256-slot queue with a fake clock; a slow reader
// cannot be simulated honestly over a loopback socket, so that limit is not repeated here.

type sseFrame struct {
	id, event, data, comment string
}

// stream is one open /events connection.
type stream struct {
	resp   *http.Response
	cancel context.CancelFunc
	frames chan sseFrame
}

type sseEnv struct {
	*seeded
	srv *httptest.Server
}

func newSSE(t *testing.T) *sseEnv {
	t.Helper()
	s := newSeeded(t)
	s.Login()
	srv := httptest.NewServer(s.Srv.Handler())
	t.Cleanup(srv.Close)
	return &sseEnv{s, srv}
}

// open connects to /events as the logged-in browser; lastID, if set, is the Last-Event-ID.
func (e *sseEnv) open(t *testing.T, lastID string, hdr map[string]string) *stream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/events", nil)
	req.Host = e.Host // the Host the server was built for, not the listener's random port
	req.AddCookie(e.Cookie)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	tr := &http.Transport{DisableCompression: true}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	st := &stream{resp: resp, cancel: cancel, frames: make(chan sseFrame, 4096)}
	t.Cleanup(st.close)
	if resp.StatusCode != 200 {
		close(st.frames)
		return st
	}
	go func() {
		defer close(st.frames)
		sc := bufio.NewScanner(resp.Body)
		var f sseFrame
		for sc.Scan() {
			l := sc.Text()
			switch {
			case l == "":
				if f != (sseFrame{}) {
					st.frames <- f
				}
				f = sseFrame{}
			case strings.HasPrefix(l, ":"):
				f.comment = strings.TrimSpace(l[1:])
			case strings.HasPrefix(l, "id:"):
				f.id = strings.TrimSpace(l[3:])
			case strings.HasPrefix(l, "event:"):
				f.event = strings.TrimSpace(l[6:])
			case strings.HasPrefix(l, "data:"):
				f.data = strings.TrimSpace(l[5:])
			}
		}
	}()
	return st
}

func (st *stream) close() {
	st.cancel()
	if st.resp != nil && st.resp.Body != nil {
		st.resp.Body.Close()
	}
}

// drain returns the frames that arrive until the stream has been quiet for quiet.
func (st *stream) drain(quiet time.Duration) []sseFrame {
	var out []sseFrame
	for {
		select {
		case f, ok := <-st.frames:
			if !ok {
				return out
			}
			out = append(out, f)
		case <-time.After(quiet):
			return out
		}
	}
}

func notices(fs []sseFrame) []sseFrame {
	var out []sseFrame
	for _, f := range fs {
		if f.event != "" {
			out = append(out, f)
		}
	}
	return out
}

// Host and Origin are checked on the stream like on every route, and a session is needed.
func TestSSERefusesForeignHostOriginAndNoSession(t *testing.T) {
	s := newSeeded(t)
	s.Login()
	for _, host := range []string{"evil.example", "evil.example:8123", "127.0.0.1:9999", "localhost"} {
		req := s.Raw("GET", "/events", nil)
		req.Host = host
		req.AddCookie(s.Cookie)
		if r := s.Do(req); r.Code != 403 {
			t.Errorf("/events with Host %q = %d, want 403", host, r.Code)
		}
	}
	for _, origin := range []string{"http://evil.example", "null", "http://127.0.0.1:9999"} {
		req := s.Raw("GET", "/events", nil)
		req.AddCookie(s.Cookie)
		req.Header.Set("Origin", origin)
		if r := s.Do(req); r.Code != 403 {
			t.Errorf("/events with Origin %q = %d, want 403", origin, r.Code)
		}
	}
	nl := newSeeded(t, withNonLoopback()...) // no session on a non-loopback bind
	if r := nl.Do(nl.Raw("GET", "/events", nil)); r.Code == 200 {
		t.Error("/events answered 200 without a session on a non-loopback bind")
	}
}

// The stream's headers: event-stream, not cached, never compressed (a gzip stream would be
// buffered by the client), and the security headers like every response.
func TestSSEHeadersAndNoCompression(t *testing.T) {
	e := newSSE(t)
	st := e.open(t, "", map[string]string{"Accept-Encoding": "gzip, br"})
	if st.resp.StatusCode != 200 {
		t.Fatalf("status %d", st.resp.StatusCode)
	}
	h := st.resp.Header
	if !strings.HasPrefix(h.Get("Content-Type"), "text/event-stream") {
		t.Errorf("content type %q", h.Get("Content-Type"))
	}
	if h.Get("Content-Encoding") != "" {
		t.Errorf("stream is compressed: %q", h.Get("Content-Encoding"))
	}
	if cc := h.Get("Cache-Control"); !strings.Contains(cc, "no-cache") && !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control %q", cc)
	}
	if !strings.Contains(h.Get("Content-Security-Policy"), "default-src 'self'") || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("security headers missing: %v", h)
	}
}

// Addendum AC6: the ninth stream of a session is refused; closing one frees the slot.
func TestSSEAtMostEightStreamsPerSession(t *testing.T) {
	e := newSSE(t)
	var open []*stream
	for i := 0; i < 8; i++ {
		st := e.open(t, "", nil)
		if st.resp.StatusCode != 200 {
			t.Fatalf("stream %d = %d", i+1, st.resp.StatusCode)
		}
		open = append(open, st)
	}
	if st := e.open(t, "", nil); st.resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("9th stream = %d, want 429", st.resp.StatusCode)
	}
	open[0].close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		st := e.open(t, "", nil)
		if st.resp.StatusCode == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no slot freed after a stream closed (last status %d)", st.resp.StatusCode)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (e *sseEnv) addEvents(n int, kind string) int64 {
	var last int64
	for i := 0; i < n; i++ {
		_ = e.Store.AddEvent("eng-1", kind, fmt.Sprintf(`{"n":%d}`, i))
	}
	last = e.Store.LastEventID()
	return last
}

// Addendum AC6: Last-Event-ID further behind than 500 events gets one resync, not a replay.
func TestSSEResyncWhenMoreThan500EventsBehind(t *testing.T) {
	e := newSSE(t)
	last := e.addEvents(600, "stuck_nudge")
	st := e.open(t, fmt.Sprint(last-600), nil)
	fs := notices(st.drain(700 * time.Millisecond))
	if len(fs) == 0 || fs[0].event != "resync" {
		t.Fatalf("first notice = %+v, want resync", fs)
	}
	if len(fs) > 3 {
		t.Errorf("a resync should not be followed by a replay: %d notices", len(fs))
	}
}

// Addendum AC6: a reconnect a few events behind replays them (as notices), at most 500.
func TestSSEReplayAfterLastEventIDIsBounded(t *testing.T) {
	e := newSSE(t)
	last := e.addEvents(30, "stuck_nudge")
	st := e.open(t, fmt.Sprint(last-30), nil)
	fs := notices(st.drain(700 * time.Millisecond))
	if len(fs) == 0 {
		t.Fatal("no replay of the 30 missed events")
	}
	if fs[0].event == "resync" {
		t.Errorf("30 events behind should replay, not resync: %+v", fs[0])
	}
	if len(fs) > 500 {
		t.Errorf("replayed %d notices, want at most 500", len(fs))
	}
}

// Addendum AC6: a burst of 50 events inside the coalescing window is a handful of notices,
// not 50; and what the stream carries is topic names and ids as JSON, never markup.
func TestSSEBurstIsCoalescedAndCarriesNoMarkup(t *testing.T) {
	e := newSSE(t)
	st := e.open(t, "", nil)
	_ = st.drain(300 * time.Millisecond) // the initial comment
	for i := 0; i < 50; i++ {
		_ = e.Store.AddEvent("eng-1", "stuck_nudge", fmt.Sprintf(`{"cause":%q}`, xss(fEventCause)))
	}
	fs := notices(st.drain(1200 * time.Millisecond))
	if len(fs) == 0 {
		t.Fatal("the burst produced no notice within a second")
	}
	perTopic := map[string]int{}
	for _, f := range fs {
		var m struct {
			Topic string `json:"topic"`
		}
		if json.Unmarshal([]byte(f.data), &m) != nil {
			t.Errorf("notice data is not JSON: %q", f.data)
		}
		perTopic[m.Topic]++
		if strings.ContainsAny(f.data+f.event, "<>") {
			t.Errorf("notice carries markup: %q", f.data)
		}
	}
	for topic, n := range perTopic {
		if n > 3 {
			t.Errorf("topic %q got %d notices for one burst of 50 events: not coalesced", topic, n)
		}
	}
	if len(fs) > 20 {
		t.Errorf("%d notices for a burst of 50 events", len(fs))
	}
}
