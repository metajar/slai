package supervisor

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/git"
	"github.com/oorbt/slai/internal/memory"
)

type countFS struct {
	reads atomic.Int32
	dirs  atomic.Int32
}

func (c *countFS) ReadDir(d string) ([]fs.DirEntry, error) { c.dirs.Add(1); return os.ReadDir(d) }
func (c *countFS) ReadFile(n string) ([]byte, error)       { c.reads.Add(1); return os.ReadFile(n) }

type memEnv struct {
	s     *Supervisor
	m     *memory.Memory
	cfs   *countFS
	calls *[][]string
}

func (e memEnv) lsTrees() int {
	n := 0
	for _, c := range *e.calls {
		if len(c) > 0 && c[0] == "ls-tree" {
			n++
		}
	}
	return n
}

func newMemEnv(t *testing.T) memEnv {
	t.Helper()
	s := unitEnv(t, team(5))
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir, "g.txt"), []byte("g\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sh(t, s.Dir, "git", "add", "g.txt")
	sh(t, s.Dir, "git", "commit", "-m", "g")
	sh(t, s.Dir, "git", "branch", "-f", s.Team.Project.IntegrationBranch, "HEAD")
	var calls [][]string
	repo := &git.Repo{Dir: s.Dir, Trace: func(a []string) { calls = append(calls, a) }}
	cfs := &countFS{}
	s.mem.fs, s.mem.git = cfs, repo
	m := memory.New(s.memoryDir(), s.Store, repo, s.Team.Project.IntegrationBranch)
	return memEnv{s, m, cfs, &calls}
}

// age moves a file's mtime back past the tick's freshness grace.
func age(t *testing.T, file string) {
	t.Helper()
	old := time.Now().Add(-3 * memoryNewFileGrace)
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}
}

func (e memEnv) put(t *testing.T, text, path string) memory.Fact {
	t.Helper()
	f, _, err := e.m.Put(context.Background(), memory.Fact{Kind: memory.KindGotcha, Text: text, Author: "eng-1", Paths: []memory.Path{{Path: path}}})
	if err != nil {
		t.Fatal(err)
	}
	age(t, filepath.Join(e.s.memoryDir(), f.ID+".md"))
	return f
}

func (e memEnv) commitOnIntegration(t *testing.T, name, body string) {
	t.Helper()
	wt := filepath.Join(t.TempDir(), "wt")
	br := e.s.Team.Project.IntegrationBranch
	sh(t, e.s.Dir, "git", "worktree", "add", wt, br)
	if err := os.WriteFile(filepath.Join(wt, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	sh(t, wt, "git", "add", name)
	sh(t, wt, "git", "commit", "-m", "change "+name)
	sh(t, e.s.Dir, "git", "worktree", "remove", "--force", wt)
}

// AC1: a valid hand edit is accepted as verified by the user within one tick.
func TestMemoryHandEditBecomesUserVerified(t *testing.T) {
	e := newMemEnv(t)
	ctx := context.Background()
	f := e.put(t, "f.txt holds the base text", "f.txt")
	e.s.memoryTick(ctx)
	file := filepath.Join(e.s.memoryDir(), f.ID+".md")
	data, _ := os.ReadFile(file)
	edited := strings.Replace(string(data), "holds the base text", "holds the base text, edited by hand", 1)
	if err := os.WriteFile(file, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	age(t, file)
	e.s.memoryTick(ctx)
	row, err := e.s.Store.GetFact(f.ID)
	if err != nil || row.Status != memory.StatusVerified || row.VerifiedBy != "user" || row.Stale {
		t.Fatalf("row = %+v %v", row, err)
	}
	got, err := e.m.Get(f.ID)
	if err != nil || got.VerifiedBy != "user" || !strings.Contains(got.Text, "edited by hand") {
		t.Fatalf("file = %+v %v", got, err)
	}
	if !hasEvent(e.s, "memory_edited") {
		t.Fatal("no memory_edited event")
	}
	before := e.cfs.reads.Load()
	e.s.memoryTick(ctx)
	if e.cfs.reads.Load() != before || countEvents(e.s, "memory_edited") != 1 {
		t.Fatal("the accepted edit was processed again")
	}
}

// AC2: a hand edit that breaks the frontmatter is flagged invalid and left on disk.
func TestMemoryBrokenHandEditIsInvalidAndKept(t *testing.T) {
	e := newMemEnv(t)
	ctx := context.Background()
	f := e.put(t, "f.txt holds the base text", "f.txt")
	e.s.memoryTick(ctx)
	file := filepath.Join(e.s.memoryDir(), f.ID+".md")
	if err := os.WriteFile(file, []byte("---\nkind: [oops\n---\nhi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	age(t, file)
	e.s.memoryTick(ctx)
	row, _ := e.s.Store.GetFact(f.ID)
	if row.Status != "invalid" || row.InvalidReason == "" {
		t.Fatalf("row = %+v", row)
	}
	if b, err := os.ReadFile(file); err != nil || !strings.Contains(string(b), "oops") {
		t.Fatalf("invalid file was changed or removed: %q %v", b, err)
	}
	e.s.memoryTick(ctx)
	e.s.memoryTick(ctx)
	if countEvents(e.s, "memory_invalid") != 1 {
		t.Fatalf("memory_invalid events = %d, want 1", countEvents(e.s, "memory_invalid"))
	}
}

// AC (addendum): a tick with no changed files opens no file contents.
func TestMemoryTickIsStatOnlyWhenNothingChanged(t *testing.T) {
	e := newMemEnv(t)
	ctx := context.Background()
	e.put(t, "f.txt holds the base text", "f.txt")
	e.put(t, "g.txt holds g", "g.txt")
	e.s.memoryTick(ctx) // first sight of each file: read once
	e.s.memoryTick(ctx)
	base := e.cfs.reads.Load()
	for i := 0; i < 3; i++ {
		e.s.memoryTick(ctx)
	}
	if got := e.cfs.reads.Load(); got != base {
		t.Fatalf("unchanged tree: %d content reads", got-base)
	}
	if e.cfs.dirs.Load() < 4 {
		t.Fatal("the tick did not even list the directory")
	}
}

// AC3: on a merge, only facts citing a changed path are re-checked.
func TestMemoryMergeStalesOnlyFactsCitingTouchedPaths(t *testing.T) {
	e := newMemEnv(t)
	ctx := context.Background()
	ff := e.put(t, "f.txt holds the base text", "f.txt")
	fg := e.put(t, "g.txt holds g", "g.txt")
	e.s.memoryTick(ctx) // first tick: full reindex, records the integration commit
	e.commitOnIntegration(t, "f.txt", "changed\n")
	*e.calls = nil
	e.s.memoryTick(ctx)
	if n := e.lsTrees(); n != 1 {
		t.Fatalf("ls-tree calls = %d, want 1 (only the fact citing f.txt)", n)
	}
	rf, _ := e.s.Store.GetFact(ff.ID)
	rg, _ := e.s.Store.GetFact(fg.ID)
	if !rf.Stale || !strings.Contains(rf.StaleReason, "f.txt") || rg.Stale {
		t.Fatalf("stale: f=%+v g=%+v", rf, rg)
	}
	if !hasEvent(e.s, "memory_stale") {
		t.Fatal("no memory_stale event")
	}
	*e.calls = nil
	e.s.memoryTick(ctx)
	if n := e.lsTrees(); n != 0 {
		t.Fatalf("nothing moved, yet %d ls-tree calls", n)
	}
}

// AC4: a fact that is still fresh against a newer integration commit gets its index sha
// bumped and its file is not rewritten.
func TestMemoryFreshFactShaBumpedWithoutRewrite(t *testing.T) {
	e := newMemEnv(t)
	ctx := context.Background()
	fg := e.put(t, "g.txt holds g", "g.txt")
	e.s.memoryTick(ctx)
	file := filepath.Join(e.s.memoryDir(), fg.ID+".md")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}
	st0, _ := os.Stat(file)
	before, _ := e.s.Store.GetFact(fg.ID)
	e.commitOnIntegration(t, "f.txt", "changed\n")
	e.s.memoryTick(ctx)
	after, _ := e.s.Store.GetFact(fg.ID)
	cur, _ := e.s.memoryGit().RevParse(ctx, e.s.Team.Project.IntegrationBranch)
	if after.Stale || after.CheckedSHA != cur || after.CheckedSHA == before.CheckedSHA {
		t.Fatalf("before %+v after %+v want sha %s", before, after, cur)
	}
	st1, _ := os.Stat(file)
	if !st1.ModTime().Equal(st0.ModTime()) {
		t.Fatal("the fact file was rewritten")
	}
}

// A rewrite by another slai process (confirm: rename, then index update) must not be
// mistaken for a hand edit: a file younger than the grace is left to a later tick,
// whether or not it has an index row.
func TestMemoryTickSkipsFreshIndexedFile(t *testing.T) {
	e := newMemEnv(t)
	ctx := context.Background()
	f := e.put(t, "f.txt holds the base text", "f.txt")
	e.s.memoryTick(ctx)
	file := filepath.Join(e.s.memoryDir(), f.ID+".md")
	data, _ := os.ReadFile(file)
	// the file is rewritten, but its index row is not updated yet (the window)
	if err := os.WriteFile(file, []byte(strings.Replace(string(data), "status: proposed", "status: verified\nverified_by: principal", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	e.s.memoryTick(ctx)
	if hasEvent(e.s, "memory_edited") {
		t.Fatal("a file younger than the grace was taken for a hand edit")
	}
	age(t, file)
	e.s.memoryTick(ctx)
	if !hasEvent(e.s, "memory_edited") {
		t.Fatal("the edit was never picked up once the file aged")
	}
}
