package tui

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/oorbt/slai/internal/git"
	"github.com/oorbt/slai/internal/memory"
	"github.com/oorbt/slai/internal/store"
)

func memGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

type memFix struct {
	m   *Model
	mm  *memory.Memory
	st  *store.Store
	dir string
	ids map[string]string
}

// newMemFix is a project with a git repo (branch slai/integration) and facts: fresh, stale
// (its file changed on integration), unverified (proposed) and an invalid file.
func newMemFix(t *testing.T) *memFix {
	t.Helper()
	dir := t.TempDir()
	memGit(t, dir, "init", "-q", "-b", "main")
	for f, body := range map[string]string{"a.go": "package a\n", "b.go": "package b\n"} {
		os.WriteFile(filepath.Join(dir, f), []byte(body), 0o644)
	}
	memGit(t, dir, "add", "-A")
	memGit(t, dir, "commit", "-qm", "init")
	memGit(t, dir, "branch", "slai/integration")
	st := openStore(t)
	mm := memory.New(filepath.Join(dir, ".slai", "memory"), st, &git.Repo{Dir: dir}, "")
	fx := &memFix{mm: mm, st: st, dir: dir, ids: map[string]string{}}
	put := func(name, text, status string, paths ...string) {
		var ps []memory.Path
		for _, p := range paths {
			ps = append(ps, memory.Path{Path: p})
		}
		f, _, err := mm.Put(context.Background(), memory.Fact{Kind: memory.KindGotcha, Text: text, Author: "principal", Status: status, Paths: ps})
		if err != nil {
			t.Fatal(err)
		}
		fx.ids[name] = f.ID
	}
	put("fresh", "a.go starts with the package line", memory.StatusVerified, "a.go")
	put("stale", "b.go parses the header", memory.StatusVerified, "b.go", "a.go")
	put("proposed", "run go test before review", memory.StatusProposed, "a.go")
	os.WriteFile(filepath.Join(mm.Dir, "f-deadbeef.md"), []byte("---\nid: [oops\n---\nbroken\n"), 0o644)
	memGit(t, dir, "checkout", "-q", "slai/integration")
	os.WriteFile(filepath.Join(dir, "b.go"), []byte("package b // v2\n"), 0o644)
	memGit(t, dir, "commit", "-qam", "change b")
	fx.m = New(Options{Project: "p", Dir: dir, Team: testTeam(), Store: st, Memory: mm})
	fx.m.w, fx.m.h = 140, 30
	return fx
}

// load runs the Memory view's load command the way the runtime would.
func (fx *memFix) load(t *testing.T) {
	t.Helper()
	fx.m.handleKey(key("M"))
	if fx.m.ov != ovMemory {
		t.Fatalf("M did not open the Memory view (ov=%v)", fx.m.ov)
	}
	msg := fx.m.memLoadCmd()()
	fx.m.Update(msg)
}

// M6 AC1: the list shows each fact with its cited file and state; stale rows are distinct and
// come first; the state filter narrows it.
func TestMemoryViewListsFactsWithStateAndFilters(t *testing.T) {
	fx := newMemFix(t)
	fx.load(t)
	out := screen(fx.m)
	for _, w := range []string{"Team memory", "4 facts: 1 fresh, 1 stale, 1 unverified, 1 invalid", fx.ids["fresh"], "a.go +1", "f-deadbeef"} {
		if !strings.Contains(out, w) {
			t.Errorf("view lacks %q:\n%s", w, out)
		}
	}
	line := func(id string) string {
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, id) {
				return l
			}
		}
		return ""
	}
	if l := line(fx.ids["stale"]); !strings.Contains(l, "⚠ stale") || !strings.Contains(l, "b.go changed") {
		t.Errorf("stale row: %q", l)
	}
	if l := line(fx.ids["fresh"]); !strings.Contains(l, "✓ fresh") || strings.Contains(l, "changed") {
		t.Errorf("fresh row: %q", l)
	}
	if l := line("f-deadbeef"); !strings.Contains(l, "✗ invalid") {
		t.Errorf("invalid row: %q", l)
	}
	// what needs attention comes first
	if strings.Index(out, fx.ids["stale"]) > strings.Index(out, fx.ids["fresh"]) {
		t.Error("the stale fact is listed after the fresh one")
	}
	// filter: f cycles all -> fresh -> stale -> unverified -> invalid -> all
	for _, c := range []struct {
		presses   int
		want, not string
	}{
		{1, fx.ids["fresh"], fx.ids["stale"]}, {2, fx.ids["stale"], fx.ids["fresh"]}, {3, fx.ids["proposed"], fx.ids["stale"]}, {4, "f-deadbeef", fx.ids["fresh"]},
	} {
		fx.m.mem.filter = 0
		for i := 0; i < c.presses; i++ {
			fx.m.handleKey(key("f"))
		}
		o := screen(fx.m)
		if !strings.Contains(o, c.want) || strings.Contains(o, c.not) || !strings.Contains(o, "filter: ") {
			t.Errorf("filter %d:\n%s", c.presses, o)
		}
	}
	fx.m.handleKey(key("f"))
	if !strings.Contains(screen(fx.m), fx.ids["fresh"]) {
		t.Error("the cycle should wrap back to all")
	}
	fx.m.handleKey(key("esc"))
	if fx.m.ov != ovNone {
		t.Error("esc should close the view")
	}
}

func TestMemoryDetailShowsTheFileAndBlobComparison(t *testing.T) {
	fx := newMemFix(t)
	fx.load(t)
	fx.m.mem.cur = 0 // the stale one is first
	_, cmd := fx.m.handleKey(key("enter"))
	if cmd == nil {
		t.Fatal("enter should load the current blobs")
	}
	fx.m.Update(cmd())
	out := screen(fx.m)
	for _, w := range []string{fx.ids["stale"] + ".md", "Cited files: recorded blob vs now on integration", "b.go", "CHANGED", "same", "status: verified"} {
		if !strings.Contains(out, w) {
			t.Errorf("detail lacks %q:\n%s", w, out)
		}
	}
	fx.m.handleKey(key("esc"))
	if fx.m.ov != ovMemory || fx.m.mem.detail {
		t.Error("esc in the detail should return to the list")
	}
}

// M6 AC3: delete asks first; only y deletes, through the memory package (file and index row);
// anything else keeps the fact.
func TestMemoryDeleteAsksFirstAndGoesThroughTheMemoryPackage(t *testing.T) {
	fx := newMemFix(t)
	fx.load(t)
	id := fx.ids["fresh"]
	for i, r := range fx.m.memRows() {
		if r.ID == id {
			fx.m.mem.cur = i
		}
	}
	file := filepath.Join(fx.mm.Dir, id+".md")
	if _, cmd := fx.m.handleKey(key("d")); cmd != nil || fx.m.mem.confirm != id {
		t.Fatalf("d should only ask (confirm=%q)", fx.m.mem.confirm)
	}
	if o := screen(fx.m); !strings.Contains(o, "Delete "+id+"?") {
		t.Errorf("the footer does not ask:\n%s", o)
	}
	fx.m.handleKey(key("n"))
	if _, err := os.Stat(file); err != nil || fx.m.mem.confirm != "" {
		t.Fatal("n must keep the fact")
	}
	fx.m.handleKey(key("d"))
	_, cmd := fx.m.handleKey(key("y"))
	if cmd == nil {
		t.Fatal("y should start the delete")
	}
	done := cmd().(memDoneMsg)
	if done.err != nil || done.note != "deleted "+id {
		t.Fatalf("delete: %+v", done)
	}
	if _, err := os.Stat(file); err == nil {
		t.Error("the file is still there")
	}
	if _, err := fx.st.GetFact(id); err == nil {
		t.Error("the index row is still there")
	}
	// the other facts are untouched
	if _, err := os.Stat(filepath.Join(fx.mm.Dir, fx.ids["stale"]+".md")); err != nil {
		t.Error("another fact was deleted")
	}
}

// M6 AC3: edit hands the edited bytes to AcceptEdit (valid: verified by the user and re-grounded;
// invalid: refused with the reason, the file left as saved). The TUI writes no file itself.
func TestMemoryEditGoesThroughAcceptEdit(t *testing.T) {
	fx := newMemFix(t)
	fx.load(t)
	id := fx.ids["proposed"]
	path := filepath.Join(fx.mm.Dir, id+".md")
	raw, _ := os.ReadFile(path)
	edited := strings.Replace(string(raw), "run go test before review", "run go test ./... before review", 1)
	os.WriteFile(path, []byte(edited), 0o644) // what the editor did
	done := acceptEdited(fx.mm, id, path, nil)
	if done.err != nil || !strings.Contains(done.note, "verified by user") {
		t.Fatalf("valid edit: %+v", done)
	}
	if f, err := fx.mm.Get(id); err != nil || f.VerifiedBy != "user" || f.Status != memory.StatusVerified {
		t.Errorf("fact after edit: %+v %v", f, err)
	}
	// a broken edit: refused, left as saved
	os.WriteFile(path, []byte("---\nnot: [valid\n---\nx\n"), 0o644)
	done = acceptEdited(fx.mm, id, path, nil)
	if done.err == nil || !strings.Contains(done.err.Error(), "is invalid") {
		t.Fatalf("broken edit: %+v", done)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "not: [valid") {
		t.Error("the broken file was changed")
	}
	// an editor failure changes nothing
	if d := acceptEdited(fx.mm, id, path, os.ErrProcessDone); d.err == nil || !strings.Contains(d.err.Error(), "editor") {
		t.Errorf("editor failure: %+v", d)
	}
	// e opens the editor for the selected fact (a command; nothing is written by the key itself)
	fx.m.mem.cur = 0
	before, _ := os.ReadFile(filepath.Join(fx.mm.Dir, fx.m.memRows()[0].ID+".md"))
	if _, cmd := fx.m.handleKey(key("e")); cmd == nil {
		t.Error("e should open the editor")
	}
	if after, _ := os.ReadFile(filepath.Join(fx.mm.Dir, fx.m.memRows()[0].ID+".md")); string(after) != string(before) {
		t.Error("pressing e changed the file")
	}
}

// The TUI never writes or removes a file itself: memory files change only through the memory
// package.
func TestTUIWritesNoMemoryFiles(t *testing.T) {
	bad := regexp.MustCompile(`os\.(WriteFile|Create|Rename|Remove|RemoveAll|OpenFile|Truncate)\b`)
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		if b, _ := os.ReadFile(p); bad.Match(b) {
			t.Errorf("%s writes or removes files itself", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func memEvent(task string, ids []string, tokens int) store.Event {
	b := `{"task_id":"` + task + `","agent":"eng-1","tokens":` + strconv.Itoa(tokens) + `,"fact_ids":[`
	for i, id := range ids {
		if i > 0 {
			b += ","
		}
		b += `"` + id + `"`
	}
	return store.Event{Kind: "memory_used", Data: b + "]}"}
}

// M6 AC2 + AC4: the task detail names the facts of the last delivery with their text, and the
// stats line compares the task's tokens with tasks that never got a pack (n/a without one).
func TestTaskMemorySectionAndStatsLine(t *testing.T) {
	text := func(id string) (string, bool) {
		return map[string]string{"f-1": "a.go starts with package", "f-2": "b.go parses the header"}[id], id != "f-gone"
	}
	evs := []store.Event{memEvent("T-1", []string{"f-1"}, 300), memEvent("T-1", []string{"f-1", "f-2", "f-gone"}, 540), memEvent("T-9", []string{"f-2"}, 100)}
	tokens := map[string]int64{"T-1": 400_000, "T-2": 500_000, "T-3": 700_000, "T-9": 90_000}
	out := ansi.Strip(memoryUsage("T-1", evs, tokens, text))
	for _, w := range []string{"Memory used (last delivery, 3 facts)", "f-1  a.go starts with package", "f-2  b.go parses the header", "f-gone  (no longer on disk)",
		"facts delivered: 3 over 2 deliveries", "pack tokens: 840", "task tokens: 400k vs 600k average of 2 tasks without a pack (-33%)"} {
		if !strings.Contains(out, w) {
			t.Errorf("section lacks %q:\n%s", w, out)
		}
	}
	// no baseline: every other task got a pack too
	if o := ansi.Strip(memoryUsage("T-1", evs, map[string]int64{"T-1": 400_000, "T-9": 90_000}, text)); !strings.Contains(o, "vs n/a (no task without a pack to compare)") {
		t.Errorf("no baseline:\n%s", o)
	}
	// no tokens recorded for the task
	if o := ansi.Strip(memoryUsage("T-1", evs, map[string]int64{"T-2": 5}, text)); !strings.Contains(o, "task tokens: n/a (none recorded for this task)") {
		t.Errorf("no tokens:\n%s", o)
	}
	// no delivery at all
	if o := ansi.Strip(memoryUsage("T-5", evs, tokens, text)); !strings.Contains(o, "No memory pack was delivered for this task.") {
		t.Errorf("no delivery:\n%s", o)
	}
}

// End to end through the task detail command: events and files on disk produce the section.
func TestTaskDetailCarriesTheMemorySection(t *testing.T) {
	fx := newMemFix(t)
	tk, _ := fx.st.CreateTask(store.Task{Title: "uses memory", Assignee: "eng-1"})
	fx.st.AddEvent("eng-1", "memory_used", `{"task_id":"`+tk.ID+`","agent":"eng-1","tokens":210,"fact_ids":["`+fx.ids["fresh"]+`"]}`)
	msg := fx.m.detailCmd(tk)().(detailMsg)
	out := ansi.Strip(msg.memory)
	if !strings.Contains(out, fx.ids["fresh"]) || !strings.Contains(out, "a.go starts with the package line") || !strings.Contains(out, "pack tokens: 210") {
		t.Errorf("memory section:\n%s", out)
	}
	fx.m.Update(msg)
	if !strings.Contains(ansi.Strip(fx.m.vp.View()), "Memory used") && !strings.Contains(ansi.Strip(renderDetail(msg.task, "", nil, msg.memory)), "Memory used") {
		t.Error("the detail view does not show it")
	}
}

func TestMemoryViewWithoutMemoryOrFacts(t *testing.T) {
	m := New(Options{Project: "p", Team: testTeam(), Store: openStore(t)})
	m.handleKey(key("M"))
	if m.ov == ovMemory || !strings.Contains(m.status, "no team memory") {
		t.Errorf("without Options.Memory the view should not open (ov=%v status=%q)", m.ov, m.status)
	}
	dir := t.TempDir()
	memGit(t, dir, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("x"), 0o644)
	memGit(t, dir, "add", "-A")
	memGit(t, dir, "commit", "-qm", "i")
	memGit(t, dir, "branch", "slai/integration")
	m = New(Options{Project: "p", Team: testTeam(), Store: openStore(t), Memory: memory.New(filepath.Join(dir, ".slai", "memory"), openStore(t), &git.Repo{Dir: dir}, "")})
	m.w, m.h = 100, 20
	m.handleKey(key("M"))
	m.Update(m.memLoadCmd()())
	if o := screen(m); !strings.Contains(o, "No facts yet.") || !strings.Contains(o, "0 facts") {
		t.Errorf("empty:\n%s", o)
	}
}

func TestMemoryKeyIsInHelpAndFooterHints(t *testing.T) {
	if !strings.Contains(helpText, "  M            Team memory") {
		t.Error("help does not list M")
	}
	found := false
	for _, h := range listHints {
		found = found || h.key == "M"
	}
	if !found {
		t.Error("the footer hints do not list M")
	}
}
