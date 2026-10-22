package cmd

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/demo"
	"github.com/oorbt/slai/internal/web/pages"
	"github.com/oorbt/slai/internal/web/server"
)

const demoAddr = "127.0.0.1:8123"

// demoServer seeds a demo into a fresh directory and returns a client for it.
func demoServer(t *testing.T, readOnly bool) (get func(path string) (int, string), dir string) {
	t.Helper()
	dir = t.TempDir()
	p, err := demo.Seed(context.Background(), dir, demo.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Store.Close() })
	o := demoOptions(p)
	o.Listen, o.ReadOnly = demoAddr, readOnly
	o.Modules = pages.All()
	srv, err := server.New(o)
	if err != nil {
		t.Fatal(err)
	}
	srv.SetBound(demoAddr)
	var cookie *http.Cookie
	get = func(path string) (int, string) {
		req := httptest.NewRequest("GET", path, nil)
		req.Host = demoAddr
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		for _, c := range rec.Result().Cookies() {
			if c.Name == "slai_session" {
				cookie = c
			}
		}
		return rec.Code, rec.Body.String()
	}
	get("/") // loopback: the first visit starts the session
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return get, dir
}

type shotPage struct{ name, path string }

func shotPages(t *testing.T) []shotPage {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "docs", "serve-screens", "pages.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []shotPage
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		fs := strings.Fields(l)
		if len(fs) != 2 {
			t.Fatalf("pages.txt line %q", l)
		}
		out = append(out, shotPage{fs[0], fs[1]})
	}
	return out
}

// not pages: fragments, the stream, assets and the cookie setter
var notAPage = regexp.MustCompile(`^(/f/.*|/events|/dashboard\.css|/theme|/agents/\{id\}/pane)$`)

func patternRe(pattern string) *regexp.Regexp {
	if pattern == "/{$}" {
		return regexp.MustCompile(`^/$`)
	}
	return regexp.MustCompile("^" + regexp.MustCompile(`\\\{[^}]*\\\}`).ReplaceAllString(regexp.QuoteMeta(pattern), `[^/]+`) + "$")
}

// T-0107 AC2: every route of the route table that renders a page has screenshots, and each
// page in the list renders (200) with its three PNGs on disk.
func TestEveryPageRouteHasScreens(t *testing.T) {
	get, _ := demoServer(t, false)
	pagesList := shotPages(t)
	o := demoOptions(&demo.Project{})
	o.Modules = pages.All()
	o.Listen = demoAddr
	o.Dir = t.TempDir()
	srv, err := server.New(o)
	if err != nil {
		t.Fatal(err)
	}
	covered := map[string]bool{}
	for _, r := range srv.Routes() {
		if r.Method != "GET" || notAPage.MatchString(r.Pattern) {
			continue
		}
		re := patternRe(r.Pattern)
		for _, p := range pagesList {
			if re.MatchString(strings.SplitN(p.path, "?", 2)[0]) {
				covered[r.Pattern] = true
			}
		}
		if !covered[r.Pattern] {
			t.Errorf("page route %s has no line in docs/serve-screens/pages.txt", r.Pattern)
		}
	}
	for _, p := range pagesList {
		if code, body := get(p.path); code != 200 || !strings.Contains(body, "<html") {
			t.Errorf("%s (%s): status %d", p.name, p.path, code)
		}
		for _, size := range []string{"light-1280", "dark-1280", "light-360"} {
			f := filepath.Join("..", "docs", "serve-screens", p.name+"-"+size+".png")
			if fi, err := os.Stat(f); err != nil || fi.Size() < 1000 {
				t.Errorf("missing screenshot %s (run docs/serve-screens/capture.sh)", filepath.Base(f))
			}
		}
	}
	for _, v := range []string{"dashboard-first-run", "dashboard-read-only", "board-read-only", "agent-detail-read-only", "sign-in",
		"board-illegal-move", "agent-new-error", "agent-new-success"} {
		for _, size := range []string{"light-1280", "dark-1280", "light-360"} {
			if _, err := os.Stat(filepath.Join("..", "docs", "serve-screens", v+"-"+size+".png")); err != nil {
				t.Errorf("missing state screenshot %s-%s", v, size)
			}
		}
	}
}

// T-0107 AC3: two seeds render byte-identical pages (fixed clock and ids).
func TestDemoPagesAreReproducible(t *testing.T) {
	a, dirA := demoServer(t, false)
	b, dirB := demoServer(t, false)
	for _, p := range shotPages(t) {
		_, x := a(p.path)
		_, y := b(p.path)
		x, y = strings.ReplaceAll(x, dirA, "<DIR>"), strings.ReplaceAll(y, dirB, "<DIR>")
		if x != y {
			t.Errorf("%s renders differently on a second seed", p.name)
		}
	}
}

// --demo never goes near the project it was started from: it takes no project and refuses
// a non-empty directory.
func TestDemoCommandRefusesARealProject(t *testing.T) {
	real := t.TempDir()
	if err := os.MkdirAll(filepath.Join(real, ".slai"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, ".slai", "team.yaml"), []byte("version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := serveCmd()
	c.SetArgs([]string{"--demo", "--demo-dir", real})
	c.SetOut(os.Stderr)
	if err := c.ExecuteContext(context.Background()); err == nil {
		t.Fatal("serve --demo ran against a real project directory")
	}
	if b, _ := os.ReadFile(filepath.Join(real, ".slai", "team.yaml")); string(b) != "version: 1\n" {
		t.Fatal("the real team.yaml was touched")
	}
}
