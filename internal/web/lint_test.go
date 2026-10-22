package web_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/web/templates"
)

// goFiles lists the non-test Go sources under internal/web.
func goFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && d.Name() == "webtest" {
			return fs.SkipDir // test support, never part of the served binary
		}
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
			out = append(out, p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Binding decision 12: agent text is data. Nothing in the web layer turns a string into
// trusted markup, script, style or URL (html/template then escapes every value).
func TestNoTrustedMarkupConversions(t *testing.T) {
	bad := regexp.MustCompile(`template\.(HTML|JS|CSS|URL|HTMLAttr|JSStr|Srcset)\b|safeHTML`)
	for _, f := range goFiles(t) {
		b, _ := os.ReadFile(f)
		if m := bad.Find(b); m != nil {
			t.Errorf("%s uses %q: agent-supplied text must stay escaped", f, m)
		}
	}
}

// No environment variable or flag can turn authentication off, so the web layer reads no
// environment at all.
func TestWebLayerReadsNoEnvironment(t *testing.T) {
	for _, f := range goFiles(t) {
		b, _ := os.ReadFile(f)
		if strings.Contains(string(b), "os.Getenv") || strings.Contains(string(b), "os.LookupEnv") || strings.Contains(string(b), "os.Environ") {
			t.Errorf("%s reads the environment", f)
		}
	}
}

// P12 AC3 / the CSP: no template has an inline script, inline handler, javascript: URL or
// style attribute, and none references another origin.
func TestTemplatesAreCSPClean(t *testing.T) {
	inline := regexp.MustCompile(`(?is)<script(?:\s[^>]*)?>[^<]|\son[a-z]+\s*=|javascript:|\sstyle\s*=|<style|(?:href|src|action)\s*=\s*["'](?:https?:)?//`)
	err := fs.WalkDir(templates.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := templates.FS.ReadFile(p)
		if m := inline.Find(b); m != nil {
			t.Errorf("%s: %q breaks the Content-Security-Policy", p, m)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
