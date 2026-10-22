package pane

import (
	"html/template"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var frag = template.Must(template.New("p").Parse(
	`{{range .Lines}}{{range .Spans}}{{if .Class}}<span class="{{.Class}}">{{.Text}}</span>{{else}}{{.Text}}{{end}}{{end}}
{{end}}`))

func html(t *testing.T, r Result) string {
	t.Helper()
	var b strings.Builder
	if err := frag.Execute(&b, r); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// Spec P3 AC3 (golden): colours and bold become whitelisted classes; OSC 52, cursor moves
// and every other escape disappear; text is escaped.
func TestRenderGolden(t *testing.T) {
	raw := "\x1b[1;31mred bold\x1b[0m plain \x1b[32mgreen\x1b[39m\n" +
		"\x1b]52;c;ZXZpbA==\x07clipboard gone\n" +
		"\x1b[2J\x1b[H\x1b[10;5Hmoved\x1b[K\n" +
		"<script>alert(1)</script> & \"q\"\n" +
		"\x1b]0;title\x1b\\after-title \x1bP1$r1\x1b\\dcs-gone\n" +
		"\x1b[38;5;9mbright\x1b[38;2;1;2;3mtruecolor-ignored\x1b[0m \x1b[44mbg\x1b[0m\n"
	got := html(t, Render(raw, false))
	want := `<span class="p-b p-fg1">red bold</span> plain <span class="p-fg2">green</span>
clipboard gone
moved
&lt;script&gt;alert(1)&lt;/script&gt; &amp; &#34;q&#34;
after-title dcs-gone
<span class="p-fg9">brighttruecolor-ignored</span> <span class="p-bg4">bg</span>
`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	for _, bad := range []string{"\x1b", "52;c", "ZXZpbA", "<script", "0;title", "\x07"} {
		if strings.Contains(got, bad) {
			t.Errorf("output contains %q", bad)
		}
	}
}

// An unterminated OSC must not swallow the rest of the capture.
func TestUnterminatedOSCDoesNotEatTheCapture(t *testing.T) {
	got := html(t, Render("a\x1b]0;never ends\nnext line\n", false))
	if !strings.Contains(got, "next line") {
		t.Fatalf("%q", got)
	}
}

// Spec addendum AC 7: 500 lines and 200 KB come back as at most 200 lines and 64 KB, with
// only the last lines kept.
func TestCaps(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 500; i++ {
		b.WriteString("\x1b[31m")
		b.WriteString(strings.Repeat("x", 400))
		b.WriteString("\x1b[0m line ")
		b.WriteString(string(rune('a' + i%26)))
		b.WriteString("\n")
	}
	if b.Len() < 200<<10 {
		t.Fatalf("fixture too small: %d", b.Len())
	}
	r := Render(b.String(), false)
	out := html(t, r)
	if len(r.Lines) > MaxLines || len(out) > MaxBytes {
		t.Fatalf("%d lines, %d bytes", len(r.Lines), len(out))
	}
	if !r.Truncated {
		t.Error("Truncated not set")
	}
	if !strings.Contains(out, "line "+string(rune('a'+499%26))) {
		t.Error("the newest line was dropped")
	}
	if strings.Contains(out, "\x1b") {
		t.Error("escape survived")
	}
	// a short capture is untouched
	if r := Render("one\ntwo\n", false); len(r.Lines) != 2 || r.Truncated {
		t.Errorf("%+v", r)
	}
}

// XSS corpus: whatever a pane prints, it never becomes markup, attribute or script.
func TestXSSCorpus(t *testing.T) {
	corpus := []string{
		`<script>alert(1)</script>`, `<img src=x onerror=alert(1)>`, `"><svg onload=alert(1)>`,
		`</span><script>x</script>`, `javascript:alert(1)`, `<iframe src="//evil">`, `'><a href=//evil>`,
		"\x1b[31m<b>bold</b>\x1b[0m", "<\x1b[0mscript>alert(1)</script>", "&lt;script&gt;", `{{.}}`, "<!--", "]]>",
		"\x1b]8;;http://evil/\x07link\x1b]8;;\x07", "\x1b[31m\x1b]0;<script>\x07x",
	}
	for _, c := range corpus {
		out := html(t, Render(c+"\n", true))
		stripped := regexp.MustCompile(`<span class="p-[a-z0-9 \-]+">|</span>`).ReplaceAllString(out, "")
		if strings.ContainsAny(stripped, "<>") && !strings.Contains(stripped, "&lt;") && !strings.Contains(stripped, "&gt;") {
			t.Errorf("%q leaked markup: %q", c, out)
		}
		if regexp.MustCompile(`<(?:script|img|svg|iframe|a|b)\b`).MatchString(stripped) {
			t.Errorf("%q produced a tag: %q", c, out)
		}
		if strings.Contains(out, "\x1b") || strings.Contains(out, "evil/") {
			t.Errorf("%q: %q", c, out)
		}
	}
}

// Spec addendum AC 19: the real captured panes (T-0044 fixtures) plus a planted script run
// through the renderer and come out as spans and escaped text only.
func TestRealPaneFixtures(t *testing.T) {
	files, _ := filepath.Glob("../../hooks/testdata/panes/*/*.txt")
	if len(files) < 10 {
		t.Fatalf("expected the T-0044 fixtures, found %d", len(files))
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		raw := string(b) + "\n<script>alert('planted')</script>\n"
		r := Render(raw, true)
		out := html(t, r)
		if strings.Contains(out, "<script") || strings.Contains(out, "\x1b") {
			t.Errorf("%s: unsafe output", f)
		}
		if !strings.Contains(out, "&lt;script&gt;alert(&#39;planted&#39;)") && !r.Truncated {
			t.Errorf("%s: planted payload not shown escaped", f)
		}
		if len(r.Lines) > MaxLines || len(out) > MaxBytes {
			t.Errorf("%s: over the caps (%d lines, %d bytes)", f, len(r.Lines), len(out))
		}
	}
}

// Spec addendum AC 19: secret-looking tokens are redacted before escaping (best-effort).
func TestRedaction(t *testing.T) {
	cases := map[string]string{
		"key sk-ant-api03-AbCdEfGhIjKlMnOpQrStUv here":                           "sk-ant-",
		"ANTHROPIC_API_KEY=sk-1234567890abcdefghijkl":                            "1234567890",
		"token: ghp_abcdefghijklmnopqrstuvwxyz0123456789":                        "ghp_abc",
		"export AWS=AKIAABCDEFGHIJKLMNOP":                                        "AKIAABCD",
		"Authorization: Bearer abcdefghijklmnop1234567890":                       "abcdefghijklmnop",
		"password = hunter2hunter2":                                              "hunter2hunter2",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.abcdefghij":                    "eyJhbGci",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIEow\n-----END RSA PRIVATE KEY-----": "MIIEow",
	}
	for in, leak := range cases {
		r := Render(in+"\n", true)
		out := html(t, r)
		if strings.Contains(out, leak) || r.Redacted == 0 || !strings.Contains(out, "[redacted]") {
			t.Errorf("%q -> %q (redacted %d)", in, out, r.Redacted)
		}
	}
	plain := "go test ./... ok 0.3s token count: 5"
	if out, n := Redact(plain); out != plain || n != 0 {
		t.Errorf("redacted ordinary text: %q %d", out, n)
	}
	if r := Render("sk-ant-api03-AbCdEfGhIjKlMnOp\n", false); r.Redacted != 0 {
		t.Error("redact=false should not redact")
	}
}

// Spec addendum AC 7: the push fires only when the pane hash changes.
func TestWatcherPushesOnlyOnChange(t *testing.T) {
	var w Watcher
	if !w.Changed("eng-1", "a") {
		t.Fatal("first capture is a change")
	}
	if w.Changed("eng-1", "a") {
		t.Fatal("same capture reported as a change")
	}
	if !w.Changed("eng-1", "b") || !w.Changed("eng-2", "b") {
		t.Fatal("a different capture, or another agent, is a change")
	}
}
