package pages_test

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/web/pages"
	"github.com/oorbt/slai/internal/web/sse"
	"github.com/oorbt/slai/internal/web/templates"
	"github.com/oorbt/slai/internal/web/webtest"
)

var liveRe = regexp.MustCompile(`<div id="([a-z-]+)" data-live="([^"]*)" data-live-src="([^"]*)"`)

// The Stats, Log and Doctor pages are live regions on the SSE topics W2 named: the page carries
// data-live (topics) and data-live-src (its fragment, with its filters), the fragment is the
// same element so a swap keeps it live, and an event that changes what the page shows makes the
// hub publish a notice for one of those topics.
func TestStatsLogAndDoctorRefreshOnTheirTopics(t *testing.T) {
	cases := []struct {
		name, path, id, topics, src string
		kind, agent, data           string // an event that must reach this page
	}{
		{"stats", "/stats?since=7d", "stats-body", "stats spend", "/f/stats?since=7d", "approve", "principal", `{"task_id":"T-0001"}`},
		{"log", "/log?agent=eng-1", "log-body", "events", "/f/log?agent=eng-1", "hook:stop", "eng-1", `{}`},
		{"doctor", "/doctor", "doctor-body", "alerts memory", "/f/doctor", "stuck_detected", "eng-1", `{}`},
	}
	h := webtest.New(t, webtest.WithModules(pages.All()...))
	h.Login()
	hub := sse.New(sse.Config{Source: h.Store, PollEvery: time.Hour, Coalesce: time.Millisecond})
	for _, c := range cases {
		page := h.Get(c.path)
		h.CheckPage(page)
		m := liveRe.FindStringSubmatch(page.Body)
		if m == nil || m[1] != c.id || m[2] != c.topics || m[3] != c.src {
			t.Fatalf("%s: live region = %q, want id %s topics %q src %q", c.name, m, c.id, c.topics, c.src)
		}
		// slai.js (the live-region code) is loaded exactly once, by the layout
		if n := strings.Count(page.Body, `src="/static/slai.js"`); n != 1 {
			t.Errorf("%s: slai.js is included %d times, want exactly once (the layout loads it)", c.name, n)
		}
		// no unconditional polling trigger on the page (the stream-down fallback is slai.js's)
		if strings.Contains(page.Body, "hx-trigger") || strings.Contains(page.Body, "every 30s") {
			t.Errorf("%s: the page still carries a polling trigger", c.name)
		}
		// the fragment is the same element, so the outerHTML swap keeps it live
		frag := h.Get(c.src)
		if frag.Code != 200 || strings.Contains(frag.Body, "<html") || !strings.Contains(frag.Body, `<div id="`+c.id+`" data-live="`+c.topics+`" data-live-src="`+c.src+`"`) {
			t.Errorf("%s: fragment %d does not carry the live region: %.200s", c.name, frag.Code, frag.Body)
		}
		// an event for this page becomes a notice on one of its topics
		sub := hub.Subscribe(h.Store.LastEventID())
		if err := h.Store.AddEvent(c.agent, c.kind, c.data); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
		hub.Poll()
		time.Sleep(5 * time.Millisecond)
		hub.Poll()
		got := map[string]bool{}
	drain:
		for {
			select {
			case f := <-sub.C:
				got[topicOf(f.Data)] = true
			default:
				break drain
			}
		}
		hit := false
		for _, topic := range strings.Fields(c.topics) {
			hit = hit || got[topic]
		}
		if !hit {
			t.Errorf("%s: event %s produced notices %v, none in %q", c.name, c.kind, got, c.topics)
		}
		sub.Close()
	}
}

func topicOf(data string) string {
	m := regexp.MustCompile(`"topic":"([^"]+)"`).FindStringSubmatch(data)
	if m == nil {
		return ""
	}
	return m[1]
}

// AC2 (T-0108): no template of the three pages has a polling trigger of its own.
func TestNoPollingTriggerInTheLivePageTemplates(t *testing.T) {
	for _, dir := range []string{"stats", "log", "doctor"} {
		err := fs.WalkDir(templates.FS, dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, _ := templates.FS.ReadFile(p)
			if m := regexp.MustCompile(`hx-trigger|every [0-9]+s`).Find(b); m != nil {
				t.Errorf("%s has a polling trigger (%q): live regions are refetched by slai.js on a notice, with the 30 s fallback only while the stream is down", p, m)
			}
			if !strings.Contains(string(b), "data-live=") {
				t.Errorf("%s has no live region", p)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
