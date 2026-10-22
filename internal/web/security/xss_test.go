package security_test

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// hostileQuery puts a payload in every query parameter a page might reflect.
func hostileQuery() string {
	v := url.Values{}
	for _, k := range []string{"q", "text", "assignee", "status", "filter", "needs", "agent", "kind", "task", "page", "from", "to", "id", "msg", "error", "next", "back", "set"} {
		v.Set(k, xss(99))
	}
	return "?" + v.Encode()
}

var (
	rawTag    = regexp.MustCompile(`(?i)<img\s|<svg\s|<iframe|<object|<embed|<style`)
	foreignJS = regexp.MustCompile(`(?i)<script(?:\s[^>]*)?>`)
	ownJS     = regexp.MustCompile(`(?i)^<script src="/static/[a-z.\-]+\.js"( defer)?>$`)
	markerRe  = regexp.MustCompile(`onerror=alert\((\d+)\)`)
)

// W8: the XSS corpus, run over every GET route of the real table, through every field that a
// user or an agent controls (task title/description/files/spec, messages, agent id/model/focus,
// event causes, lane-denial paths, pane text from the T-0044 real captures, fact text, the
// project name) and through every query parameter. Whatever a page renders, the payload is
// escaped text: no tag, no script, no inline handler, and the security headers are on every answer.
func TestXSSCorpusOverEveryGETRoute(t *testing.T) {
	s := newSeeded(t)
	s.Login()
	seen := map[int][]string{} // field marker -> pages that rendered it
	nPages := 0
	for _, rt := range s.Srv.Routes() {
		if rt.Write() || rt.Pattern == "/theme" || rt.Pattern == "/events" { // /events is a stream: sse_test.go covers it
			continue
		}
		for _, base := range paths(rt.Pattern, s.candidates()) {
			for _, q := range []string{"", hostileQuery()} {
				path := base + q
				resp := s.Get(path)
				s.CheckHeaders(resp)
				if resp.Code >= 500 {
					t.Errorf("GET %s = %d: %.200s", path, resp.Code, resp.Body)
					continue
				}
				if !strings.Contains(resp.Header.Get("Content-Type"), "html") {
					continue
				}
				body := resp.Body
				nPages++
				if resp.Code == 200 {
					checkMarkup(t, path, body)
				}
				if m := rawTag.FindString(body); m != "" {
					t.Errorf("GET %s renders markup from data: %q", path, m)
				}
				for _, tag := range foreignJS.FindAllString(body, -1) {
					if !ownJS.MatchString(tag) {
						t.Errorf("GET %s has a script tag that is not a /static/ file: %s", path, tag)
					}
				}
				for _, m := range markerRe.FindAllStringSubmatchIndex(body, -1) {
					var n int
					fmt.Sscan(body[m[2]:m[3]], &n)
					want := fmt.Sprintf("&lt;img src=x onerror=alert(%d)&gt;", n)
					// inert either way: entity-escaped, or still JSON-escaped (\u003c) inside raw event data shown as text
					jsonForm := fmt.Sprintf(`\u003cimg src=x onerror=alert(%d)\u003e`, n)
					near := body[max(0, m[0]-len("&lt;img src=x ")-6):min(len(body), m[1]+10)]
					if !strings.Contains(near, want) && !strings.Contains(near, jsonForm) {
						ctx := body[max(0, m[0]-40):min(len(body), m[1]+20)]
						t.Errorf("GET %s: payload %d is not entity-escaped: %q", path, n, ctx)
					}
					seen[n] = append(seen[n], rt.Pattern)
				}
			}
		}
	}
	if nPages < 20 {
		t.Fatalf("only %d pages crawled", nPages)
	}
	// the payload must actually reach the pages, or this test proves nothing
	var missing []string
	for n, name := range fieldNames {
		if len(seen[n]) == 0 {
			missing = append(missing, fmt.Sprintf("%s (%d)", name, n))
		}
	}
	sort.Strings(missing)
	t.Logf("crawled %d pages; fields never rendered by any page yet: %v", nPages, missing)
	for _, n := range []int{fTaskTitle, fTaskDesc, fMsgBody, fAgentModel, fEventCause, fEventPath, fPane, 99} {
		if len(seen[n]) == 0 {
			t.Errorf("no page rendered %s: the corpus did not reach it", fieldNames[n])
		}
	}
	t.Logf("pane payload reached: %v", dedupe(seen[fPane]))
}

func dedupe(l []string) []string {
	m := map[string]bool{}
	var out []string
	for _, s := range l {
		if !m[s] {
			m[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// The pane fragment of the real captures carries only whitelisted spans and no escape byte.
func TestPaneFragmentOverRealCapturesIsSpansAndTextOnly(t *testing.T) {
	s := newSeeded(t)
	s.Login()
	resp := s.Get("/agents/eng-1/pane")
	if resp.Code != 200 {
		t.Fatalf("%d %.200s", resp.Code, resp.Body)
	}
	s.CheckHeaders(resp)
	if strings.Contains(resp.Body, "\x1b") || strings.Contains(resp.Body, "<img") || strings.Contains(resp.Body, "ZXZpbA") {
		t.Errorf("unsafe pane output")
	}
	tags := regexp.MustCompile(`</?[a-zA-Z][^>]*>`).FindAllString(resp.Body, -1)
	for _, tag := range tags {
		if !regexp.MustCompile(`^(<span class="p-[a-z0-9 \-]+">|</span>|<div[^>]*>|</div>|<pre[^>]*>|</pre>)$`).MatchString(tag) {
			t.Errorf("unexpected tag in the pane fragment: %s", tag)
		}
	}
	if len(resp.Body) > 66<<10 {
		t.Errorf("pane fragment is %d bytes", len(resp.Body))
	}
}

var (
	tagRe  = regexp.MustCompile(`<([a-zA-Z][a-zA-Z0-9]*)((?:\s+[^\s=>"']+(?:\s*=\s*(?:"[^"]*"|'[^']*'|[^\s>]+))?)*)\s*/?>`)
	attrRe = regexp.MustCompile(`([^\s=>"']+)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+)))?`)
)

// checkMarkup parses the tags of a page (text content is not looked at) and fails on an inline
// event handler, a style attribute, or a link/script/form that points anywhere but this origin.
// CheckPage's regexes also match the visible, escaped text of a payload, so the corpus test
// needs this tag-level version.
func checkMarkup(t *testing.T, path, body string) {
	t.Helper()
	for _, p := range markupProblems(path, body) {
		t.Error(p)
	}
}

func markupProblems(path, body string) []string {
	var out []string
	errorf := func(f string, a ...any) { out = append(out, fmt.Sprintf(f, a...)) }
	for _, m := range tagRe.FindAllStringSubmatch(body, -1) {
		tag := strings.ToLower(m[1])
		for _, a := range attrRe.FindAllStringSubmatch(m[2], -1) {
			name := strings.ToLower(a[1])
			val := a[2] + a[3] + a[4]
			switch {
			case strings.HasPrefix(name, "on"):
				errorf("GET %s: <%s> has an inline handler %s", path, tag, name)
			case name == "style":
				errorf("GET %s: <%s> has a style attribute", path, tag)
			case name == "href" || name == "src" || name == "action" || name == "formaction" || name == "hx-get" || name == "hx-post":
				v := strings.ToLower(strings.TrimSpace(val))
				if strings.HasPrefix(v, "javascript:") || strings.HasPrefix(v, "data:text") || strings.HasPrefix(v, "//") || strings.HasPrefix(v, "http:") || strings.HasPrefix(v, "https:") {
					errorf("GET %s: <%s %s=%q> leaves this origin", path, tag, name, val)
				}
			}
		}
	}
	return out
}

// The markup checker itself: it flags real injections and leaves escaped text alone.
func TestMarkupCheckerBites(t *testing.T) {
	bad := []string{`<img src=x onerror=alert(1)>`, `<a href="javascript:alert(1)">x</a>`, `<div style="x:y">`, `<div onclick='x()'>`,
		`<script src="//evil/x.js"></script>`, `<form action="https://evil/">`, `<button hx-post="http://evil/x">`}
	for _, b := range bad {
		if len(markupProblems("/x", b)) == 0 {
			t.Errorf("missed %s", b)
		}
	}
	good := []string{`<p>&lt;img src=x onerror=alert(1)&gt;</p>`, `<a href="/agents/eng-1" class="x">a</a>`, `<script src="/static/htmx.min.js" defer></script>`,
		`<button hx-post="/agents/eng-1/send" hx-target="#r">go</button>`}
	for _, g := range good {
		if p := markupProblems("/x", g); len(p) != 0 {
			t.Errorf("flagged %s: %v", g, p)
		}
	}
}
