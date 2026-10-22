// Package pane turns a `tmux capture-pane -e` capture into data the agent page renders
// safely. Nothing here produces markup: Render returns lines of spans (text plus a class
// name), and the template escapes the text, so a pane can never inject HTML.
//
// Rules (spec addendum, principal's pane rules):
//   - Only SGR (ESC [ ... m) is honoured, and of it only bold and the 16 basic foreground
//     and background colours. Every other CSI sequence (cursor movement, erase, mode
//     changes), every OSC (titles, clipboard: OSC 52), DCS, APC, PM and SOS string, every
//     other escape and every C0/C1 control character is dropped.
//   - The result is the last 200 lines and at most 64 KB of rendered output.
//   - Secret-looking tokens are redacted before anything else, best-effort. A pane may
//     contain secrets that no pattern recognises; the page says so.
package pane

import (
	"crypto/sha256"
	"encoding/hex"
	"html/template"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// Caps on what a pane view returns.
const (
	MaxLines = 200
	MaxBytes = 64 << 10
	// slack reserves room for the fragment's own wrapper markup inside MaxBytes.
	slack = 1 << 10
)

// Span is a run of text with one style. Class is "" or space-separated class names from
// the fixed set below; it is never derived from the capture's text.
type Span struct {
	Text  string
	Class string
}

// Line is one pane line.
type Line struct{ Spans []Span }

// Result is a rendered pane.
type Result struct {
	Lines     []Line
	Truncated bool // older lines were dropped to fit the caps
	Redacted  int  // secret-looking tokens replaced
}

// Size is the byte length of the rendered fragment body (escaped text plus span markup).
func (r Result) Size() int {
	n := 0
	for _, l := range r.Lines {
		n += lineSize(l)
	}
	return n
}

func lineSize(l Line) int {
	n := 1 // newline
	for _, s := range l.Spans {
		n += len(template.HTMLEscapeString(s.Text))
		if s.Class != "" {
			n += len(`<span class="">`) + len(s.Class) + len(`</span>`)
		}
	}
	return n
}

type style struct {
	bold   bool
	fg, bg int // -1 default, 0-15
}

func (s style) class() string {
	var c []string
	if s.bold {
		c = append(c, "p-b")
	}
	if s.fg >= 0 {
		c = append(c, "p-fg"+strconv.Itoa(s.fg))
	}
	if s.bg >= 0 {
		c = append(c, "p-bg"+strconv.Itoa(s.bg))
	}
	return strings.Join(c, " ")
}

// Render converts raw to safe lines. redact turns secret redaction on (the page always
// does; tests of the converter alone may not).
func Render(raw string, redact bool) Result {
	var res Result
	if redact {
		raw, res.Redacted = Redact(raw)
	}
	cur := style{fg: -1, bg: -1}
	var lines []Line
	var spans []Span
	var text strings.Builder
	flush := func() {
		if text.Len() == 0 {
			return
		}
		cls := cur.class()
		if n := len(spans); n > 0 && spans[n-1].Class == cls {
			spans[n-1].Text += text.String()
		} else {
			spans = append(spans, Span{text.String(), cls})
		}
		text.Reset()
	}
	endLine := func() {
		flush()
		lines = append(lines, Line{spans})
		spans = nil
	}
	i := 0
	for i < len(raw) {
		c := raw[i]
		switch {
		case c == 0x1b:
			i = skipEscape(raw, i, &cur, flush)
		case c == '\n':
			endLine()
			i++
		case c == '\t':
			text.WriteString("    ")
			i++
		case c < 0x20 || c == 0x7f:
			i++ // other C0 controls, \r and BEL included
		default:
			r, n := utf8.DecodeRuneInString(raw[i:])
			i += n
			if r == utf8.RuneError && n == 1 {
				continue
			}
			if r >= 0x80 && r <= 0x9f {
				continue // C1 controls
			}
			text.WriteRune(r)
		}
	}
	if text.Len() > 0 || len(spans) > 0 {
		endLine()
	}
	if len(lines) > MaxLines {
		lines, res.Truncated = lines[len(lines)-MaxLines:], true
	}
	size := 0
	for _, l := range lines {
		size += lineSize(l)
	}
	for size > MaxBytes-slack && len(lines) > 0 {
		size -= lineSize(lines[0])
		lines, res.Truncated = lines[1:], true
	}
	res.Lines = lines
	return res
}

// skipEscape handles the escape sequence starting at raw[i] and returns the index after
// it. SGR updates *cur (after flushing the pending text in the old style).
func skipEscape(raw string, i int, cur *style, flush func()) int {
	if i+1 >= len(raw) {
		return i + 1
	}
	switch raw[i+1] {
	case '[': // CSI: parameters 0x30-0x3f, intermediates 0x20-0x2f, final 0x40-0x7e
		j := i + 2
		for j < len(raw) && raw[j] >= 0x30 && raw[j] <= 0x3f {
			j++
		}
		params := raw[i+2 : j]
		for j < len(raw) && raw[j] >= 0x20 && raw[j] <= 0x2f {
			j++
		}
		if j >= len(raw) {
			return len(raw)
		}
		if raw[j] == 'm' && !strings.ContainsAny(params, "<=>?") {
			flush()
			applySGR(params, cur)
		}
		if raw[j] < 0x40 || raw[j] > 0x7e {
			return j // malformed: stop at the offending byte, which is then handled normally
		}
		return j + 1
	case ']', 'P', 'X', '^', '_': // OSC, DCS, SOS, PM, APC: a string ended by BEL or ST
		j := i + 2
		for j < len(raw) {
			if raw[j] == 0x07 {
				return j + 1
			}
			if raw[j] == 0x1b && j+1 < len(raw) && raw[j+1] == '\\' {
				return j + 2
			}
			// a string never contains newlines in a terminal; treat one as its end so a
			// stray unterminated OSC cannot swallow the rest of the capture
			if raw[j] == '\n' {
				return j
			}
			j++
		}
		return len(raw)
	default: // ESC + intermediates + final (charset selection, ESC 7, ESC =, ...)
		j := i + 1
		for j < len(raw) && raw[j] >= 0x20 && raw[j] <= 0x2f {
			j++
		}
		if j < len(raw) && raw[j] >= 0x30 && raw[j] <= 0x7e {
			j++
		}
		return j
	}
}

// applySGR applies the supported attributes of one SGR sequence. Everything else (dim,
// italic, underline, blink, reverse, truecolor, 256-colour above 15) is ignored; its
// parameters are consumed so they are not read as something else.
func applySGR(params string, cur *style) {
	if params == "" {
		*cur = style{fg: -1, bg: -1}
		return
	}
	ps := strings.Split(params, ";")
	num := func(k int) int {
		if k >= len(ps) || ps[k] == "" {
			return 0
		}
		n, err := strconv.Atoi(ps[k])
		if err != nil || n < 0 || n > 1000 {
			return -1
		}
		return n
	}
	for k := 0; k < len(ps); k++ {
		n := num(k)
		switch {
		case n == 0:
			*cur = style{fg: -1, bg: -1}
		case n == 1:
			cur.bold = true
		case n == 22:
			cur.bold = false
		case n >= 30 && n <= 37:
			cur.fg = n - 30
		case n >= 90 && n <= 97:
			cur.fg = n - 90 + 8
		case n == 39:
			cur.fg = -1
		case n >= 40 && n <= 47:
			cur.bg = n - 40
		case n >= 100 && n <= 107:
			cur.bg = n - 100 + 8
		case n == 49:
			cur.bg = -1
		case n == 38 || n == 48:
			mode := num(k + 1)
			set := func(v int) {
				if v < 0 || v > 15 {
					return // only the 16 basic colours are drawn
				}
				if n == 38 {
					cur.fg = v
				} else {
					cur.bg = v
				}
			}
			switch mode {
			case 5:
				set(num(k + 2))
				k += 2
			case 2:
				k += 4
			default:
				k++
			}
		}
	}
}

// Hash identifies a capture; the page re-renders (and the hub pushes) only when it changes.
func Hash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// Watcher remembers the last capture hash per agent.
type Watcher struct {
	mu   sync.Mutex
	seen map[string]string
}

// Changed records raw as agent's latest capture and reports whether it differs from the
// previous one. The first capture of an agent counts as a change.
func (w *Watcher) Changed(agent, raw string) bool {
	h := Hash(raw)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.seen == nil {
		w.seen = map[string]string{}
	}
	if w.seen[agent] == h {
		return false
	}
	w.seen[agent] = h
	return true
}

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)`),
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{8,}`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{20,}`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9\-]{10,}`),
	regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{30,}`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`),
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/\-]{16,}=*`),
}

// assignRe: NAME=value / name: value where the name says it is a credential. The name is
// kept so the line still reads; only the value goes.
var assignRe = regexp.MustCompile(`(?i)\b([A-Za-z0-9_\-]*(?:api[_-]?key|secret|token|passw(?:or)?d|credential)[A-Za-z0-9_\-]*)(\s*[=:]\s*)("?)([^\s"']{8,})("?)`)

// Redact replaces secret-looking tokens with [redacted] and returns how many it replaced.
// It is a best-effort tripwire: it cannot know every secret.
func Redact(s string) (string, int) {
	n := 0
	for _, re := range secretPatterns {
		s = re.ReplaceAllStringFunc(s, func(string) string { n++; return "[redacted]" })
	}
	s = assignRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := assignRe.FindStringSubmatch(m)
		if strings.Contains(sub[4], "[redacted]") {
			return m
		}
		n++
		return sub[1] + sub[2] + sub[3] + "[redacted]" + sub[5]
	})
	return s, n
}
