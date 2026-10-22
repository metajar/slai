package security_test

import (
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/web/webtest"
)

// writeRoutes are the non-GET routes of the real table with a concrete path each.
type wroute struct{ method, pattern, path string }

func (s *seeded) writeRoutes() []wroute {
	var out []wroute
	for _, rt := range s.Srv.Routes() {
		if !rt.Write() {
			continue
		}
		id := s.Tasks[1]
		if strings.HasPrefix(rt.Pattern, "/agents") {
			id = "eng-1"
		}
		out = append(out, wroute{rt.Method, rt.Pattern, strings.NewReplacer("{id}", id).Replace(rt.Pattern)})
	}
	return out
}

func form() url.Values {
	return url.Values{"body": {"x"}, "title": {"x"}, "assignee": {"eng-1"}, "id": {"eng-9"}, "role": {"engineer"}, "model": {"sonnet"}, "harness": {"claude"}}
}

// snapNoDenied is snap with web_denied events left out (a refusal may record those).
func (s *seeded) stable() snapshot {
	evs, _ := s.Store.RecentEvents(1_000_000)
	n := 0
	for _, e := range evs {
		if e.Kind != "web_denied" {
			n++
		}
	}
	sn := s.snap()
	sn.events = n
	return sn
}

// W8 / P1 ACs 2-3 + security AC1, over every write route of the real table: no session, no
// X-Slai-Request, a foreign Origin or a foreign Host is refused with 403, and nothing in the
// store or team.yaml changes.
func TestEveryWriteRouteRefusesTheCSRFMatrix(t *testing.T) {
	s := newSeeded(t)
	s.Login()
	routes := s.writeRoutes()
	if len(routes) < 5 {
		t.Fatalf("only %d write routes in the table", len(routes))
	}
	before := s.stable()
	for _, r := range routes {
		cases := map[string]func() *webtest.Resp{
			"no session": func() *webtest.Resp { return s.Do(s.Raw(r.method, r.path, form())) },
			"no X-Slai-Request": func() *webtest.Resp {
				req := s.Browser(r.method, r.path, form())
				req.Header.Del("X-Slai-Request")
				return s.Do(req)
			},
			"X-Slai-Request: 0": func() *webtest.Resp {
				req := s.Browser(r.method, r.path, form())
				req.Header.Set("X-Slai-Request", "0")
				return s.Do(req)
			},
		}
		for _, origin := range []string{"http://evil.example", "null", "http://127.0.0.1:9999", "http://127.0.0.1:8123.evil.example", "http://127.0.0.1:8123/x"} {
			origin := origin
			cases["Origin "+origin] = func() *webtest.Resp {
				req := s.Browser(r.method, r.path, form())
				req.Header.Set("Origin", origin)
				return s.Do(req)
			}
		}
		for _, host := range []string{"evil.example", "evil.example:8123", "127.0.0.1", "127.0.0.1:9999", "localhost.evil.example:8123", "127.0.0.1:8123.evil.example", "evil.example:80"} {
			host := host
			cases["Host "+host] = func() *webtest.Resp {
				req := s.Browser(r.method, r.path, form())
				req.Host = host
				req.Header.Set("Origin", "http://"+host) // consistent with the Host, so only the Host check can refuse
				return s.Do(req)
			}
		}
		for name, do := range cases {
			if resp := do(); resp.Code != 403 {
				t.Errorf("%s %s [%s] = %d, want 403: %.120s", r.method, r.pattern, name, resp.Code, resp.Body)
			} else {
				s.CheckHeaders(resp)
			}
		}
	}
	if after := s.stable(); after != before {
		t.Fatalf("a refused write changed state: %s", before.diff(after))
	}
	if len(s.Act.sent) != 0 || len(s.Act.pauses) != 0 {
		t.Fatalf("a refused write reached the supervisor: %v %v", s.Act.sent, s.Act.pauses)
	}
}

// Security AC (addendum 2): 1000 POSTs without a session leave the events table exactly as it
// was; a refusal on a valid session is recorded as web_denied, but only a bounded number of times.
func TestUnauthenticatedWritesNeverTouchTheEventsTable(t *testing.T) {
	s := newSeeded(t)
	routes := s.writeRoutes()
	before := s.snap()
	for i := 0; i < 1000; i++ {
		r := routes[i%len(routes)]
		req := s.Raw(r.method, r.path, form())
		switch i % 3 { // vary what else is wrong: nothing, a header, a hostile Origin
		case 1:
			req.Header.Set("X-Slai-Request", "1")
		case 2:
			req.Header.Set("X-Slai-Request", "1")
			req.Header.Set("Origin", "http://"+s.Host)
		}
		if resp := s.Do(req); resp.Code != 403 {
			t.Fatalf("POST %d to %s = %d", i, r.path, resp.Code)
		}
	}
	if after := s.snap(); after != before {
		t.Fatalf("unauthenticated writes changed the store: %s", before.diff(after))
	}

	s.Login()
	for i := 0; i < 300; i++ {
		r := routes[i%len(routes)]
		req := s.Browser(r.method, r.path, form())
		req.Header.Del("X-Slai-Request")
		s.Do(req)
	}
	evs, _ := s.Store.RecentEvents(1_000_000)
	n := 0
	for _, e := range evs {
		if e.Kind == "web_denied" {
			n++
			if !strings.Contains(e.Data, `"actor":"user"`) || !strings.Contains(e.Data, "missing_csrf_header") {
				t.Errorf("odd web_denied event: %s", e.Data)
			}
		}
	}
	if n == 0 || n > 100 {
		t.Errorf("%d web_denied events after 300 refusals on one session: want some, but bounded", n)
	}
}

// Security AC2 / addendum 2: --read-only refuses every non-GET route of the real table and the
// pages carry no write control.
func TestReadOnlyOverTheRealRouteTable(t *testing.T) {
	s := newSeeded(t, webtest.ReadOnly())
	s.Login()
	before := s.stable()
	for _, r := range s.writeRoutes() {
		if resp := s.Do(s.Browser(r.method, r.path, form())); resp.Code != 403 {
			t.Errorf("%s %s read-only = %d, want 403", r.method, r.pattern, resp.Code)
		}
	}
	if after := s.stable(); after != before || len(s.Act.sent) != 0 {
		t.Fatalf("read-only server wrote: %s", before.diff(after))
	}
	for _, rt := range s.Srv.Routes() {
		if rt.Write() || rt.Pattern == "/events" || strings.Contains(rt.Pattern, "{") && !strings.HasPrefix(rt.Pattern, "/agents/") && !strings.HasPrefix(rt.Pattern, "/tasks/") {
			continue
		}
		path := strings.NewReplacer("{id}", "eng-1", "{$}", "").Replace(rt.Pattern)
		if strings.HasPrefix(rt.Pattern, "/tasks/") {
			path = strings.Replace(rt.Pattern, "{id}", s.Tasks[1], 1)
		}
		resp := s.Get(path)
		if resp.Code == 200 && strings.Contains(resp.Header.Get("Content-Type"), "html") && strings.Contains(resp.Body, "hx-post=") {
			t.Errorf("GET %s offers a write control in read-only mode", path)
		}
	}
}

// Every write route rejects a body over 64 KB before the handler runs.
func TestOversizedBodyIsRefusedOnEveryWriteRoute(t *testing.T) {
	s := newSeeded(t)
	s.Login()
	before := s.stable()
	for _, r := range s.writeRoutes() {
		big := url.Values{"body": {strings.Repeat("a", 70<<10)}}
		if resp := s.Do(s.Browser(r.method, r.path, big)); resp.Code != 413 {
			t.Errorf("%s %s with a 70 KB body = %d, want 413", r.method, r.pattern, resp.Code)
		}
	}
	if s.stable() != before {
		t.Error("an oversized request changed state")
	}
}

// An agent-controlled string in a form field comes back escaped in the answer to every write
// (the service error or confirmation is shown verbatim, as text).
func TestWriteAnswersEscapeTheirInput(t *testing.T) {
	s := newSeeded(t)
	s.Login()
	for _, r := range s.writeRoutes() {
		v := url.Values{}
		for _, k := range []string{"body", "title", "description", "assignee", "id", "role", "model", "harness", "focus", "persona", "perm", "reason", "note", "status", "priority", "spec", "files", "depends_on", "to", "column"} {
			v.Set(k, xss(98))
		}
		req := s.Browser(r.method, r.path, v)
		req.Header.Set("HX-Request", "true")
		resp := s.Do(req)
		s.CheckHeaders(resp)
		if resp.Code >= 500 {
			t.Errorf("%s %s with hostile input = %d: %.200s", r.method, r.pattern, resp.Code, resp.Body)
		}
		if strings.Contains(resp.Body, "<img src=x") {
			t.Errorf("%s %s echoed raw markup: %.300s", r.method, r.pattern, resp.Body)
		}
	}
	_ = fmt.Sprint
}
