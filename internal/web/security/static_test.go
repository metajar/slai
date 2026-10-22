package security_test

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/web/assets"
)

// W8: nothing outside the embedded asset set can be reached through /static/, whatever the
// path looks like, and what is served is exactly what was embedded.
func TestStaticPathTraversal(t *testing.T) {
	s := newSeeded(t)
	s.Login()
	bad := []string{
		"/static/../go.mod", "/static/../../go.mod", "/static/..%2fgo.mod", "/static/%2e%2e/go.mod", "/static/%2e%2e%2fgo.mod",
		"/static/..%5cgo.mod", "/static/%2e%2e%5cgo.mod", "/static/....//go.mod", "/static/slai.css/../tabler.min.css",
		"/static/slai.css/", "/static//etc/passwd", "/static/%2fetc%2fpasswd", "/static/..;/go.mod", "/static/%252e%252e%252fgo.mod",
		"/static/VENDOR.md", "/static/assets.go", "/static/assets_test.go", "/static/", "/static", "/static/.", "/static/%00", "/static/slai.css%00.png",
		"/static/..", "/static/.git/config", "/static/templates/layout/layout.html", "/static/%c0%ae%c0%ae/go.mod",
	}
	for _, p := range bad {
		resp := s.Get(p)
		s.CheckHeaders(resp)
		if resp.Code == 200 {
			t.Errorf("GET %s = 200 (%d bytes): %.80q", p, len(resp.Body), resp.Body)
		}
		for _, leak := range []string{"module github.com", "root:x:", "package assets", "[core]"} {
			if strings.Contains(resp.Body, leak) {
				t.Errorf("GET %s leaked %q", p, leak)
			}
		}
	}
	// other methods on /static/ are refused
	for _, m := range []string{"POST", "PUT", "DELETE"} {
		if resp := s.Do(s.Browser(m, "/static/slai.css", nil)); resp.Code == 200 {
			t.Errorf("%s /static/slai.css = 200", m)
		}
	}
}

// W8 / security AC9: exactly four JavaScript files are embedded (htmx, SortableJS, our
// slai.js and theme.js), each pinned in VENDOR.md, each served byte for byte; and every script any page
// loads is one of them.
func TestExactlyTheFourPinnedScriptsAreServed(t *testing.T) {
	s := newSeeded(t)
	s.Login()
	var js []string
	_ = fs.WalkDir(assets.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".js") {
			js = append(js, p)
		}
		return nil
	})
	if len(js) != 4 {
		t.Fatalf("want exactly 4 embedded JS files, got %v", js)
	}
	vendor, _ := assets.FS.ReadFile("VENDOR.md")
	for _, name := range js {
		embedded, _ := assets.FS.ReadFile(name)
		resp := s.Get("/static/" + name)
		if resp.Code != 200 || resp.Body != string(embedded) {
			t.Errorf("/static/%s: %d, %d bytes served vs %d embedded", name, resp.Code, len(resp.Body), len(embedded))
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
			t.Errorf("/static/%s content type %q", name, ct)
		}
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("/static/%s lacks nosniff", name)
		}
		sum := sha256.Sum256(embedded)
		pinned := strings.Contains(string(vendor), hex.EncodeToString(sum[:]))
		ours := name == "slai.js" || name == "theme.js"
		if !pinned && !ours {
			t.Errorf("%s is not pinned in VENDOR.md", name)
		}
	}
	if !strings.Contains(string(vendor), "slai.js") {
		t.Error("VENDOR.md does not mention slai.js (ours)")
	}
	// every <script src> of every page is one of the four
	allowed := map[string]bool{}
	for _, n := range js {
		allowed["/static/"+n] = true
	}
	src := regexp.MustCompile(`(?i)<script[^>]*\ssrc="([^"]*)"`)
	pages := 0
	for _, p := range []string{"/", "/agents", "/agents/eng-1", "/agents/new", "/board", "/tasks/" + s.Tasks[1], "/capabilities"} {
		resp := s.Get(p)
		if resp.Code != 200 {
			continue
		}
		pages++
		for _, m := range src.FindAllStringSubmatch(resp.Body, -1) {
			if !allowed[m[1]] {
				t.Errorf("GET %s loads %s", p, m[1])
			}
		}
	}
	if pages < 4 {
		t.Errorf("only %d pages checked", pages)
	}
}
