package assets

import (
	"regexp"
	"strings"
	"testing"
)

// The conditions for allowing a first-party script (principal, T-0093): it is short, builds no
// HTML, evaluates no text, makes no request of its own to anywhere but a relative path, and
// has no notion of a token.
func TestSlaiJSStaysSmallAndHarmless(t *testing.T) {
	b, err := FS.ReadFile("slai.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if n := strings.Count(src, "\n"); n >= 150 {
		t.Errorf("slai.js is %d lines, the limit is 150", n)
	}
	bad := map[string]string{
		`\beval\s*\(`:        "eval",
		`new\s+Function`:     "new Function",
		`\bFunction\s*\(`:    "Function()",
		`\.innerHTML`:        "innerHTML",
		`\.outerHTML`:        "outerHTML",
		`insertAdjacentHTML`: "insertAdjacentHTML",
		`document\.write`:    "document.write",
		`set(Timeout|Interval)\s*\(\s*["'` + "`" + `]`: "setTimeout/setInterval with a string",
		`\bon[a-z]+\s*=\s*["']`:                        "an on*= attribute",
		`(?i)https?:`:                                  "an absolute URL",
		`["'` + "`" + `]\s*//`:                         "a protocol-relative URL",
		`(?i)\bwww\.`:                                  "a host name",
		`(?i)token`:                                    "token handling",
		`(?i)document\.cookie`:                         "cookie access",
		`\bimport\s*\(`:                                "dynamic import",
		`\bXMLHttpRequest\b`:                           "a hand-made request",
		`\bWebSocket\b`:                                "a socket",
		`\bfetch\s*\(`:                                 "a request of its own (use htmx so the CSRF header and the swap rules apply)",
		`\.src\s*=|createElement\s*\(\s*["']script`:    "script injection",
	}
	for re, what := range bad {
		if regexp.MustCompile(re).MatchString(src) {
			t.Errorf("slai.js contains %s", what)
		}
	}
	// the one stream it may open is the same-origin notice stream (W2); nothing else
	for _, m := range regexp.MustCompile(`EventSource\s*\(([^)]*)\)`).FindAllStringSubmatch(src, -1) {
		// "/events", or "/events" plus the ?watch= query watching() builds from the page's own
		// data-live attributes (pane topics only; the server validates them again)
		if a := strings.TrimSpace(m[1]); a != `"/events"` && a != `"/events" + watching(` {
			t.Errorf("slai.js opens an EventSource on %s: only \"/events\" (plus watching()) is allowed", m[1])
		}
	}
	// the paths it names are relative: /tasks/ for the board, /events for the stream
	for _, m := range regexp.MustCompile(`["'](/[^"']*)["']`).FindAllStringSubmatch(src, -1) {
		if !strings.HasPrefix(m[1], "/tasks/") && m[1] != "/events" {
			t.Errorf("slai.js names the path %q: it may only post under /tasks/ and read /events", m[1])
		}
	}
}
