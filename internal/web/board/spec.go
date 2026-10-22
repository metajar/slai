package board

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const maxSpecBytes = 256 << 10

// SpecView is a spec file rendered as structure (never as markup): the template escapes every
// string, so a spec can say anything and render only as text.
type SpecView struct {
	Path    string
	Message string // why there is nothing to show (outside .slai/specs/, missing, too big)
	Blocks  []Block
}

// Block is one markdown block.
type Block struct {
	Kind  string // h1 h2 h3 p ul ol code
	Spans []Span // h1 h2 h3 p
	Items [][]Span
	Code  string
}

// Span is a run of inline text: plain, `code` or **bold**.
type Span struct {
	Text       string
	Code, Bold bool
}

// ReadSpec reads rel (a task's spec_path) only when it is a file inside <dir>/.slai/specs/ once
// symlinks are resolved. Anything else gets a plain message and nothing is read.
func ReadSpec(dir, rel string) SpecView {
	v := SpecView{Path: rel}
	if strings.TrimSpace(rel) == "" {
		v.Message = "This task has no spec."
		return v
	}
	p := rel
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	p = filepath.Clean(p)
	inside := func(root, path string) bool {
		r, err := filepath.Rel(root, path)
		return err == nil && r != "." && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) && !filepath.IsAbs(r)
	}
	// first by name (a path that names something outside is refused whether or not it exists),
	if !inside(filepath.Join(dir, ".slai", "specs"), p) {
		v.Message = "The spec path is outside .slai/specs/, so it is not shown."
		return v
	}
	root, err := filepath.EvalSymlinks(filepath.Join(dir, ".slai", "specs"))
	if err != nil {
		v.Message = "There is no .slai/specs/ directory in this project."
		return v
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		v.Message = "The spec file was not found."
		return v
	}
	// then by where it really is (a symlink out of the directory is refused)
	if !inside(root, real) {
		v.Message = "The spec path is outside .slai/specs/, so it is not shown."
		return v
	}
	fi, err := os.Stat(real)
	if err != nil || !fi.Mode().IsRegular() {
		v.Message = "The spec file was not found."
		return v
	}
	if fi.Size() > maxSpecBytes {
		v.Message = fmt.Sprintf("The spec is %d KB, too large to show here; open it in the repository.", fi.Size()>>10)
		return v
	}
	b, err := os.ReadFile(real)
	if err != nil {
		v.Message = "The spec file could not be read."
		return v
	}
	v.Blocks = ParseMarkdown(string(b))
	return v
}

// ParseMarkdown handles the subset specs use: # headings, paragraphs, - / * and 1. lists, fenced
// code, and inline `code` and **bold**. Everything else is text.
func ParseMarkdown(src string) []Block {
	var out []Block
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	var para []string
	flush := func() {
		if len(para) > 0 {
			out = append(out, Block{Kind: "p", Spans: inline(strings.Join(para, " "))})
			para = nil
		}
	}
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		t := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(t, "```"):
			flush()
			var code []string
			for i++; i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```"); i++ {
				code = append(code, lines[i])
			}
			out = append(out, Block{Kind: "code", Code: strings.Join(code, "\n")})
		case t == "":
			flush()
		case strings.HasPrefix(t, "#"):
			flush()
			n := len(t) - len(strings.TrimLeft(t, "#"))
			if n > 3 {
				n = 3
			}
			out = append(out, Block{Kind: fmt.Sprintf("h%d", n), Spans: inline(strings.TrimSpace(strings.TrimLeft(t, "#")))})
		case isBullet(t) || isNumbered(t):
			flush()
			kind := "ul"
			if isNumbered(t) {
				kind = "ol"
			}
			b := Block{Kind: kind}
			for ; i < len(lines); i++ {
				s := strings.TrimSpace(lines[i])
				if s == "" || (kind == "ul" && !isBullet(s)) || (kind == "ol" && !isNumbered(s)) {
					break
				}
				b.Items = append(b.Items, inline(stripMarker(s)))
			}
			i--
			out = append(out, b)
		default:
			para = append(para, t)
		}
	}
	flush()
	return out
}

func isBullet(t string) bool { return strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ") }

func isNumbered(t string) bool {
	i := 0
	for i < len(t) && t[i] >= '0' && t[i] <= '9' {
		i++
	}
	return i > 0 && i+1 < len(t) && t[i] == '.' && t[i+1] == ' '
}

func stripMarker(t string) string {
	if isBullet(t) {
		return strings.TrimSpace(t[2:])
	}
	i := strings.Index(t, ". ")
	return strings.TrimSpace(t[i+2:])
}

// inline splits s into plain, `code` and **bold** spans.
func inline(s string) []Span {
	var out []Span
	for len(s) > 0 {
		c, b := strings.Index(s, "`"), strings.Index(s, "**")
		switch {
		case c >= 0 && (b < 0 || c < b):
			if e := strings.Index(s[c+1:], "`"); e >= 0 {
				out = add(out, Span{Text: s[:c]})
				out = add(out, Span{Text: s[c+1 : c+1+e], Code: true})
				s = s[c+1+e+1:]
				continue
			}
		case b >= 0:
			if e := strings.Index(s[b+2:], "**"); e >= 0 {
				out = add(out, Span{Text: s[:b]})
				out = add(out, Span{Text: s[b+2 : b+2+e], Bold: true})
				s = s[b+2+e+2:]
				continue
			}
		}
		out = add(out, Span{Text: s})
		break
	}
	return out
}

func add(out []Span, sp Span) []Span {
	if sp.Text == "" {
		return out
	}
	return append(out, sp)
}
