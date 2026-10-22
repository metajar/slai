package agents_test

import (
	"bufio"
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/web/agents"
	"github.com/oorbt/slai/internal/web/sse"
	"github.com/oorbt/slai/internal/web/templates"
	"github.com/oorbt/slai/internal/web/webtest"
)

type capSrc struct {
	mu   sync.Mutex
	text map[string]string
	n    map[string]int
}

func (c *capSrc) CapturePane(_ context.Context, id string, _ bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == nil {
		c.n = map[string]int{}
	}
	c.n[id]++
	return c.text[id], nil
}

func (c *capSrc) set(id, s string) { c.mu.Lock(); c.text[id] = s; c.mu.Unlock() }
func (c *capSrc) count(id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[id]
}

type tclock struct{ t time.Time }

func (c *tclock) now() time.Time { return c.t }

// pushEnv: a real hub over a real store, a fake clock the test advances, and a Push whose
// captures the test counts.
func pushEnv(t *testing.T) (*agents.Push, *sse.Hub, *capSrc, *tclock) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	clk := &tclock{time.Unix(1_800_000_000, 0)}
	hub := sse.New(sse.Config{Source: st, Now: clk.now, PollEvery: time.Hour}) // the test drives Poll
	src := &capSrc{text: map[string]string{"eng-1": "one", "orch": "x"}}
	p := &agents.Push{}
	p.Attach(hub)
	agents.SetPushSource(p, src)
	return p, hub, src, clk
}

// flush advances past the coalescing window, polls, and returns the pane notices queued on s.
func flush(hub *sse.Hub, clk *tclock, s *sse.Sub) []string {
	clk.t = clk.t.Add(300 * time.Millisecond)
	hub.Poll()
	var out []string
	for {
		select {
		case f := <-s.C:
			if f.Comment == "" && strings.Contains(f.Data, `"topic":"pane:`) {
				out = append(out, f.Data)
			}
		default:
			return out
		}
	}
}

// T-0106 AC1: an unchanged pane sends no notices; a change sends exactly one.
func TestPushSendsOneNoticePerChangeAndNoneWhenUnchanged(t *testing.T) {
	p, hub, src, clk := pushEnv(t)
	s := hub.Subscribe(0)
	defer s.Close()
	s.SetWatch([]string{"pane:eng-1"})
	// the page rendered "one" (the handler records what it shows)
	p.W.Changed("eng-1", "one")

	if n := p.Tick(context.Background()); n != 1 {
		t.Fatalf("Tick watched %d agents, want 1", n)
	}
	if got := flush(hub, clk, s); len(got) != 0 {
		t.Fatalf("an unchanged pane sent %v", got)
	}
	src.set("eng-1", "two")
	p.Tick(context.Background())
	got := flush(hub, clk, s)
	if len(got) != 1 || !strings.Contains(got[0], `"topic":"pane:eng-1"`) {
		t.Fatalf("a change should send exactly one notice, got %v", got)
	}
	p.Tick(context.Background())
	p.Tick(context.Background())
	if got := flush(hub, clk, s); len(got) != 0 {
		t.Fatalf("the same content again sent %v", got)
	}
	// two changes inside one window are one notice
	src.set("eng-1", "three")
	p.Tick(context.Background())
	src.set("eng-1", "four")
	p.Tick(context.Background())
	if got := flush(hub, clk, s); len(got) != 1 {
		t.Fatalf("two changes in one window = %d notices, want 1 (coalesced)", len(got))
	}
}

// T-0106 AC2: nothing is captured for an agent nobody watches, and the loop ends when the
// last stream does.
func TestPushWatchesOnlyWhatIsViewed(t *testing.T) {
	p, hub, src, clk := pushEnv(t)
	if n := p.Tick(context.Background()); n != 0 || src.count("eng-1")+src.count("orch") != 0 {
		t.Fatalf("with no stream, Tick captured (%d watched, eng-1 %d, orch %d)", n, src.count("eng-1"), src.count("orch"))
	}
	s := hub.Subscribe(0) // a stream that watches no pane
	s.SetWatch(nil)
	p.Tick(context.Background())
	if src.count("eng-1")+src.count("orch") != 0 {
		t.Fatal("a stream with no watch caused a capture")
	}
	s.SetWatch([]string{"pane:orch"})
	p.Tick(context.Background())
	if src.count("orch") != 1 || src.count("eng-1") != 0 {
		t.Fatalf("captures orch=%d eng-1=%d: only the watched agent may be captured", src.count("orch"), src.count("eng-1"))
	}
	s.Close()
	clk.t = clk.t.Add(time.Second)
	before := src.count("orch")
	if n := p.Tick(context.Background()); n != 0 || src.count("orch") != before {
		t.Fatalf("captured after the stream closed (%d watched)", n)
	}
}

// The capture loop starts when a stream starts watching and ends when none does.
func TestPushLoopStartsAndStops(t *testing.T) {
	p, hub, _, _ := pushEnv(t)
	p.Interval = 10 * time.Millisecond
	if p.Running() {
		t.Fatal("the loop runs with nobody watching")
	}
	s := hub.Subscribe(0)
	s.SetWatch([]string{"pane:eng-1"})
	wait := func(want bool) {
		t.Helper()
		for i := 0; i < 200; i++ {
			if p.Running() == want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("Running() never became %v", want)
	}
	wait(true)
	s.Close()
	wait(false)
}

// T-0106 AC3: no agents template polls any more; the pane region is a live region on its
// pane topic, which is what the stream pushes.
func TestAgentTemplatesHaveNoPollingTrigger(t *testing.T) {
	n := 0
	err := fs.WalkDir(templates.FS, "agents", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := templates.FS.ReadFile(p)
		n++
		if strings.Contains(string(b), "hx-trigger") {
			t.Errorf("%s still has an hx-trigger", p)
		}
		return nil
	})
	if err != nil || n < 4 {
		t.Fatalf("walked %d agents templates (%v)", n, err)
	}
	b, _ := templates.FS.ReadFile("agents/pane.html")
	if !strings.Contains(string(b), `data-live="pane:{{.AgentID}}"`) || !strings.Contains(string(b), `data-live-src="/agents/{{.AgentID}}/pane"`) {
		t.Error("the pane fragment is not a live region on its pane topic")
	}
}

// capActions is server.Actions plus a pane source whose content the test changes.
type capActions struct{ *capSrc }

func (capActions) SendUser(string, string) (int64, error)        { return 1, nil }
func (capActions) CommitPreview(context.Context) (string, error) { return "", nil }
func (capActions) RevertPreview(context.Context, string) (string, error) {
	return "", nil
}

// The whole path over a real listener: a page's stream names its pane, the pane changes, the
// stream carries a notice for it; a change to an agent nobody watches does not.
func TestPanePushEndToEnd(t *testing.T) {
	push := &agents.Push{Interval: 40 * time.Millisecond}
	src := &capSrc{text: map[string]string{"eng-1": "first", "orch": "other"}}
	h := webtest.New(t,
		webtest.WithModules(sse.Module(func(x *sse.Hub) { push.Attach(x) }), agents.ModuleWithPush(push)),
		webtest.WithActions(capActions{src}),
		webtest.Setup(func(st *store.Store, team *config.Team) {
			for _, id := range []string{"eng-1", "orch"} {
				_ = st.UpsertAgent(store.Agent{ID: id, Role: "engineer", TmuxSession: "s-" + id})
				_ = st.SetAgentStatus(id, "working")
			}
		}))
	h.Login()
	// the page: renders the pane (recording what it shows) and is a live region
	page := h.Get("/agents/eng-1")
	if !strings.Contains(page.Body, `data-live="pane:eng-1"`) {
		t.Fatalf("no live pane region:\n%.400s", page.Body)
	}
	ts := httptest.NewServer(h.Srv.Handler())
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/events?watch=pane:eng-1", nil)
	req.Host = h.Host
	req.AddCookie(h.Cookie)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "data: ") {
				lines <- sc.Text()
			}
		}
		close(lines)
	}()
	waitFor := func(what string, d time.Duration) (string, bool) {
		timeout := time.After(d)
		for {
			select {
			case l := <-lines:
				if strings.Contains(l, what) {
					return l, true
				}
			case <-timeout:
				return "", false
			}
		}
	}
	// an unchanged pane, and a change to an agent nobody watches: no pane notice
	src.set("orch", "changed, but nobody is looking")
	if l, ok := waitFor(`"topic":"pane:`, 700*time.Millisecond); ok {
		t.Fatalf("a notice without a change to a watched pane: %s", l)
	}
	if src.count("orch") != 0 {
		t.Errorf("the unwatched agent was captured %d times", src.count("orch"))
	}
	// the watched pane changes: one notice
	src.set("eng-1", "second")
	l, ok := waitFor(`"topic":"pane:eng-1"`, 3*time.Second)
	if !ok {
		t.Fatal("no pane:eng-1 notice after the pane changed")
	}
	if strings.Contains(l, "<") || strings.Contains(l, "second") {
		t.Errorf("the notice carries content: %s", l)
	}
	if l, ok := waitFor(`"topic":"pane:eng-1"`, 600*time.Millisecond); ok {
		t.Fatalf("a second notice for one change: %s", l)
	}
	// the stream closes: the capture loop ends
	cancel()
	res.Body.Close()
	deadline := time.Now().Add(3 * time.Second)
	for push.Running() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if push.Running() {
		t.Error("the capture loop is still running with no stream")
	}
}
