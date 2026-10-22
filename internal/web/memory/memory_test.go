package memory_test

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/git"
	mem "github.com/oorbt/slai/internal/memory"
	webmem "github.com/oorbt/slai/internal/web/memory"
	"github.com/oorbt/slai/internal/web/pages"
	"github.com/oorbt/slai/internal/web/webtest"
)

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

type fixture struct {
	h     *webtest.Harness
	m     *mem.Memory
	ids   map[string]string
	faces map[string]mem.Fact
}

func commit(t *testing.T, dir, file, body string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(filepath.Join(dir, file)), 0o755)
	os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644)
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-qm", "edit "+file)
}

// newFixture builds a project with a git repo (branch slai/integration), and facts in every
// state: fresh (verified, files unchanged), stale (a cited file changed), gone (a cited file
// deleted), unverified (proposed), unverified (cites nothing) and invalid (a broken file).
func newFixture(t *testing.T, opts ...webtest.Option) *fixture {
	t.Helper()
	fx := &fixture{ids: map[string]string{}}
	all := append([]webtest.Option{webtest.WithModules(webmem.Module)}, opts...)
	fx.h = webtest.New(t, all...)
	dir := fx.h.Dir
	run(t, dir, "init", "-q", "-b", "main")
	commit(t, dir, "a.go", "package a // v1\n")
	commit(t, dir, "b.go", "package b // v1\n")
	commit(t, dir, "c.go", "package c // v1\n")
	run(t, dir, "branch", "slai/integration")
	team := config.Preset("solo", "p", "d", "main")
	fx.h.Team.Project.IntegrationBranch = team.Project.IntegrationBranch
	if fx.h.Team.Project.IntegrationBranch == "" {
		fx.h.Team.Project.IntegrationBranch = "slai/integration"
	}
	fx.m = mem.New(filepath.Join(dir, ".slai", "memory"), fx.h.Store, &git.Repo{Dir: dir}, "")
	put := func(role, kind, text, author, status string, paths ...string) string {
		var ps []mem.Path
		for _, p := range paths {
			ps = append(ps, mem.Path{Path: p})
		}
		f, _, err := fx.m.Put(context.Background(), mem.Fact{Kind: kind, Text: text, Author: author, Status: status, Paths: ps})
		if err != nil {
			t.Fatalf("put %s: %v", role, err)
		}
		fx.ids[role] = f.ID
		return f.ID
	}
	put("fresh", mem.KindConvention, "a.go starts every file with the package line", "principal", mem.StatusVerified, "a.go")
	put("stale", mem.KindGotcha, "b.go is the only place that parses the header <b>raw</b>", "principal", mem.StatusVerified, "b.go")
	put("gone", mem.KindArchitecture, "c.go owns the retry loop", "principal", mem.StatusVerified, "c.go", "a.go")
	put("proposed", mem.KindCommand, "Run go test ./... before review", "eng-1", mem.StatusProposed, "a.go")
	put("nopath", mem.KindDecisionPointer, "Why the queue is not durable", "orch", mem.StatusVerified)
	os.WriteFile(filepath.Join(fx.m.Dir, "f-deadbeef.md"), []byte("---\nid: [oops\n---\nbroken <script>alert(1)</script>\n"), 0o644)
	// integration moves: b.go changes, c.go is deleted
	run(t, dir, "checkout", "-q", "slai/integration")
	commit(t, dir, "b.go", "package b // v2\n")
	run(t, dir, "rm", "-q", "c.go")
	run(t, dir, "commit", "-qm", "drop c.go")
	fx.h.Login()
	return fx
}

func (fx *fixture) get(path string) *webtest.Resp {
	fx.h.T.Helper()
	return fx.h.Get(path)
}

func row(body, id string) string {
	i := strings.Index(body, `href="/memory/`+id+`"`)
	if i < 0 {
		return ""
	}
	i = strings.LastIndex(body[:i], "<tr")
	j := strings.Index(body[i:], "</tr>")
	return body[i : i+j]
}

// P10 AC1: the list shows every fact with its state, stale rows distinct, and filters by state.
func TestListShowsStatesAndFiltersByStatus(t *testing.T) {
	fx := newFixture(t)
	r := fx.get("/memory")
	fx.h.CheckPage(r)
	want := map[string]string{"fresh": "fresh", "stale": "stale", "gone": "stale", "proposed": "unverified", "nopath": "unverified"}
	for role, state := range want {
		rw := row(r.Body, fx.ids[role])
		if rw == "" {
			t.Fatalf("%s: no row", role)
		}
		if !strings.Contains(rw, `data-state="`+state+`"`) || !strings.Contains(rw, "slai-fact-"+state) {
			t.Errorf("%s should be %s:\n%s", role, state, rw)
		}
	}
	if !strings.Contains(r.Body, "f-deadbeef") || !strings.Contains(row(r.Body, "f-deadbeef"), `data-state="invalid"`) {
		t.Error("the broken file is not listed as invalid")
	}
	if !strings.Contains(r.Body, "6 facts: 1 fresh, 2 stale, 2 unverified, 1 invalid") {
		t.Errorf("summary: %s", regexp.MustCompile(`id="mem-summary">[^<]*`).FindString(r.Body))
	}
	// stale is distinct by words and glyph, not colour alone
	if !strings.Contains(row(r.Body, fx.ids["stale"]), "⚠") || !strings.Contains(row(r.Body, fx.ids["stale"]), "b.go changed") || !strings.Contains(row(r.Body, fx.ids["gone"]), "c.go no longer exists") {
		t.Errorf("stale rows lack the reason:\n%s", row(r.Body, fx.ids["stale"]))
	}
	r = fx.get("/memory?status=stale")
	if strings.Count(r.Body, `class="slai-fact `) != 2 || strings.Contains(r.Body, fx.ids["fresh"]) {
		t.Errorf("status=stale shows %d rows", strings.Count(r.Body, `class="slai-fact `))
	}
	r = fx.get("/memory?kind=command")
	if strings.Count(r.Body, `class="slai-fact `) != 1 || !strings.Contains(r.Body, fx.ids["proposed"]) {
		t.Error("kind filter")
	}
	r = fx.get("/memory?q=retry")
	if strings.Count(r.Body, `class="slai-fact `) != 1 || !strings.Contains(r.Body, fx.ids["gone"]) {
		t.Error("text search")
	}
	if r = fx.get("/memory?q=nothing-matches"); !strings.Contains(r.Body, "No fact matches this filter") {
		t.Error("empty filter state")
	}
}

// P10 AC2 + FG AC26: the detail shows the file and a per-path blob comparison; for stale and
// invalid facts it says what changed, and it offers no replacement fact.
func TestDetailShowsFileBlobComparisonAndWhatChanged(t *testing.T) {
	fx := newFixture(t)
	b := fx.get("/memory/" + fx.ids["gone"])
	fx.h.CheckPage(b)
	for _, w := range []string{"path no longer exists", "c.go", "a.go", "What changed", "c.go: path no longer exists (recorded ", ".slai/memory/" + fx.ids["gone"] + ".md", "integration_sha"} {
		if !strings.Contains(b.Body, w) {
			t.Errorf("gone detail lacks %q", w)
		}
	}
	if !regexp.MustCompile(`(?s)a\.go</code></td><td><code>([0-9a-f]{12})</code></td><td><code>([0-9a-f]{12})</code>.*?same`).MatchString(b.Body) {
		t.Error("an unchanged path should show the same recorded and current blob and 'same'")
	}
	s := fx.get("/memory/" + fx.ids["stale"])
	if !strings.Contains(s.Body, "b.go: recorded ") || !strings.Contains(s.Body, "changed") {
		t.Errorf("stale detail:\n%s", between(s.Body, `id="fact-changes"`, "</ul>"))
	}
	// the current blob is the real one on integration
	m := regexp.MustCompile(`b\.go</code></td><td><code>([0-9a-f]{12})</code></td><td><code>([0-9a-f]{12})</code>`).FindStringSubmatch(s.Body)
	if len(m) != 3 || m[1] == m[2] {
		t.Errorf("recorded and current blob should differ for a stale path: %v", m)
	}
	// a fresh fact has no "what changed" box
	if f := fx.get("/memory/" + fx.ids["fresh"]); strings.Contains(f.Body, "What changed") || !strings.Contains(f.Body, "verified") {
		t.Error("fresh detail")
	}
	// invalid: the parse error and the file, escaped, nothing replaces it
	i := fx.get("/memory/f-deadbeef")
	fx.h.CheckPage(i)
	if i.Code != 200 || !strings.Contains(i.Body, "Parse error:") || !strings.Contains(i.Body, "&lt;script&gt;alert(1)&lt;/script&gt;") || strings.Contains(i.Body, "<script>alert") {
		t.Errorf("invalid detail: %d", i.Code)
	}
	// unverified: says so
	if u := fx.get("/memory/" + fx.ids["nopath"]); !strings.Contains(u.Body, "cites no file, so it cannot be grounded") {
		t.Error("a fact without paths should say it cannot be grounded")
	}
}

func between(s, a, b string) string {
	i := strings.Index(s, a)
	if i < 0 {
		return ""
	}
	j := strings.Index(s[i:], b)
	if j < 0 {
		return s[i:]
	}
	return s[i : i+j]
}

// The state of every fact on the page is the one the CLI and doctor compute (internal/memory),
// and asking git once for all facts agrees with asking once per fact.
func TestStatesMatchTheSharedDefinition(t *testing.T) {
	fx := newFixture(t)
	vs, bad, err := fx.m.Views(context.Background())
	if err != nil || len(bad) != 1 {
		t.Fatalf("views: %v bad=%d", err, len(bad))
	}
	facts, _, _ := fx.m.List()
	for i, f := range facts {
		one, err := fx.m.View(context.Background(), f)
		if err != nil {
			t.Fatal(err)
		}
		if one.State != vs[i].State || one.Reason != vs[i].Reason {
			t.Errorf("%s: per-fact %s/%q, batch %s/%q", f.ID, one.State, one.Reason, vs[i].State, vs[i].Reason)
		}
		if !strings.Contains(row(fx.get("/memory").Body, f.ID), `data-state="`+one.State+`"`) {
			t.Errorf("%s: the page disagrees with the shared state %s", f.ID, one.State)
		}
	}
}

// Which tasks were given a fact comes from the memory_used events.
func TestUsedByComesFromMemoryUsedEvents(t *testing.T) {
	fx := newFixture(t)
	fx.h.Store.AddEvent("eng-1", "memory_used", fmt.Sprintf(`{"task":"T-0007","agent":"eng-1","fact_ids":[%q,%q],"tokens":420}`, fx.ids["fresh"], fx.ids["stale"]))
	fx.h.Store.AddEvent("eng-1", "memory_used", fmt.Sprintf(`{"task":"T-0009","agent":"eng-1","fact_ids":[%q],"tokens":300}`, fx.ids["fresh"]))
	r := fx.get("/memory")
	if rw := row(r.Body, fx.ids["fresh"]); !strings.Contains(rw, `href="/tasks/T-0007"`) || !strings.Contains(rw, `href="/tasks/T-0009"`) {
		t.Errorf("fresh row: %s", rw)
	}
	if rw := row(r.Body, fx.ids["gone"]); !strings.Contains(rw, "not used yet") {
		t.Errorf("an unused fact should say so: %s", rw)
	}
	d := fx.get("/memory/" + fx.ids["fresh"])
	if !strings.Contains(d.Body, "Given to 2 tasks (2 deliveries)") || !strings.Contains(d.Body, "420 tokens") {
		t.Errorf("detail: %s", between(d.Body, `id="fact-used"`, "</div></div>"))
	}
}

// Fact text is data: it renders escaped, on the list and on the detail.
func TestFactTextIsEscaped(t *testing.T) {
	fx := newFixture(t)
	for _, p := range []string{"/memory", "/memory/" + fx.ids["stale"]} {
		r := fx.get(p)
		if strings.Contains(r.Body, "<b>raw</b>") || !strings.Contains(r.Body, "&lt;b&gt;raw&lt;/b&gt;") {
			t.Errorf("%s: fact text not escaped", p)
		}
	}
}

func TestUnknownFactAndTraversalAreRefused(t *testing.T) {
	fx := newFixture(t)
	os.WriteFile(filepath.Join(fx.h.Dir, "secret.md"), []byte("TOP-SECRET"), 0o644)
	for _, p := range []string{"/memory/f-00000000", "/memory/..%2F..%2Fsecret", "/memory/secret"} {
		r := fx.get(p)
		if r.Code == 200 || strings.Contains(r.Body, "TOP-SECRET") {
			t.Errorf("%s: %d", p, r.Code)
		}
	}
	if r := fx.get("/memory/f-00000000"); r.Code != http.StatusUnprocessableEntity || !strings.Contains(r.Body, "no memory fact") {
		t.Errorf("unknown id: %d", r.Code)
	}
}

func TestEmptyMemoryHasANextAction(t *testing.T) {
	h := webtest.New(t, webtest.WithModules(webmem.Module))
	run(t, h.Dir, "init", "-q", "-b", "main")
	h.Login()
	r := h.Get("/memory")
	h.CheckPage(r)
	if !strings.Contains(r.Body, "No facts yet. Agents propose facts with") {
		t.Error("empty state")
	}
}

// P10 AC3: the web layer never writes a memory file (or any file): there is no write route for
// memory, and no non-test source under internal/web calls a file-mutating os function.
func TestWebLayerWritesNoFilesAndHasNoMemoryWriteRoute(t *testing.T) {
	h := webtest.New(t, webtest.WithModules(pages.All()...))
	for _, rt := range h.Srv.Routes() {
		if rt.Write() && strings.Contains(strings.ToLower(rt.Pattern+rt.Action), "memory") {
			t.Errorf("%s %s is a memory write route", rt.Method, rt.Pattern)
		}
	}
	bad := regexp.MustCompile(`os\.(WriteFile|Create|Rename|Remove|RemoveAll|OpenFile|Mkdir|MkdirAll|Truncate|Chmod)\b|ioutil\.WriteFile`)
	err := filepath.WalkDir("..", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		if strings.HasPrefix(filepath.ToSlash(p), "../webtest/") { // the test harness seeds fixtures; it is not served code
			return nil
		}
		b, _ := os.ReadFile(p)
		if m := bad.Find(b); m != nil {
			t.Errorf("%s calls %s: the web layer must not write files", p, m)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Read-only mode and the memory page: still readable, nothing to switch off.
func TestMemoryPageWorksInReadOnlyMode(t *testing.T) {
	fx := newFixture(t, webtest.ReadOnly())
	r := fx.get("/memory")
	fx.h.CheckPage(r)
	if !strings.Contains(r.Body, "Read-only: edit, re-verify or delete a fact with") {
		t.Error("the page should say where edits happen")
	}
}
