package assets

import (
	"regexp"
	"strings"
	"testing"
)

// The conditions for allowing a first-party script (principal, T-0093): it is short, builds no
// HTML, evaluates no text, makes no request of its own to anywhere but a relative path, and
// has no notion of a token. They hold for every first-party script (slai.js and theme.js, T-0002);
// the per-file table below holds each file's size cap and the few things it may do.
var firstPartyScripts = []struct {
	name     string
	maxLines int
	// allow lists banned-pattern keys this file is excused from (it has its own check below)
	allow map[string]bool
}{
	{name: "slai.js", maxLines: 150, allow: map[string]bool{`\bEventSource\b`: true}},
	{name: "theme.js", maxLines: 60},
}

// code is src without comments (a comment is not executed, and these scripts describe in prose
// what they must never do). Only block comments and whole-line // comments are removed, so a
// "//" inside a string (a protocol-relative URL) is still seen.
var (
	blockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	lineComment  = regexp.MustCompile(`(?m)^\s*//.*$`)
)

func code(src string) string {
	return lineComment.ReplaceAllString(blockComment.ReplaceAllString(src, ""), "")
}

func TestFirstPartyScriptsStaySmallAndHarmless(t *testing.T) {
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
		`(?i)document\.cookie`:                         "cookie access (theme.js needs none: the server renders an explicit theme)",
		`\bimport\s*\(`:                                "dynamic import",
		`(?m)^\s*import\b`:                             "an import",
		`\brequire\s*\(`:                               "require",
		`\bXMLHttpRequest\b`:                           "a hand-made request",
		`\bWebSocket\b`:                                "a socket",
		`\bEventSource\b`:                              "an event stream",
		`\bfetch\s*\(`:                                 "a request of its own (use htmx so the CSRF header and the swap rules apply)",
		`\.src\s*=|createElement\s*\(\s*["']script`:    "script injection",
	}
	for _, f := range firstPartyScripts {
		b, err := FS.ReadFile(f.name)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		if n := strings.Count(src, "\n"); n >= f.maxLines {
			t.Errorf("%s is %d lines, the limit is %d", f.name, n, f.maxLines)
		}
		for re, what := range bad {
			if f.allow[re] {
				continue
			}
			if regexp.MustCompile(re).MatchString(code(src)) {
				t.Errorf("%s contains %s", f.name, what)
			}
		}
	}
}

// slai.js alone: the one stream it may open is the same-origin notice stream (W2), and the paths
// it names are relative.
func TestSlaiJSStreamAndPaths(t *testing.T) {
	b, err := FS.ReadFile("slai.js")
	if err != nil {
		t.Fatal(err)
	}
	src := code(string(b))
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
		if !strings.HasPrefix(m[1], "/tasks/") && m[1] != "/events" && m[1] != "/move" { // "/move" is the tail of "/tasks/" + id + "/move"
			t.Errorf("slai.js names the path %q: it may only post under /tasks/ and read /events", m[1])
		}
	}
}
