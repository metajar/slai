package memory

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/git"
	"github.com/oorbt/slai/internal/store"
)

func sh(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command(args[0], args[1:]...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
}

type env struct {
	m     *Memory
	repo  *git.Repo
	root  string
	calls *[][]string
}

func newEnv(t *testing.T) env {
	t.Helper()
	root := t.TempDir()
	sh(t, root, "git", "init", "-q", "-b", "main")
	write(t, root, "a.go", "package a\n")
	write(t, root, "b.go", "package b\n")
	sh(t, root, "git", "add", ".")
	sh(t, root, "git", "commit", "-qm", "init")
	sh(t, root, "git", "branch", "slai/integration")
	db, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var calls [][]string
	repo := &git.Repo{Dir: root, Trace: func(a []string) { calls = append(calls, a) }}
	m := New(filepath.Join(root, ".slai", "memory"), db, repo, "")
	m.Now = func() time.Time { return time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC) }
	return env{m, repo, root, &calls}
}

func write(t *testing.T, root, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// commitOnIntegration changes a file on slai/integration via a throwaway worktree.
func commitOnIntegration(t *testing.T, e env, name, body string) {
	t.Helper()
	wt := filepath.Join(t.TempDir(), "wt")
	sh(t, e.root, "git", "worktree", "add", "-q", wt, "slai/integration")
	if body == "" {
		sh(t, wt, "git", "rm", "-q", name)
	} else {
		write(t, wt, name, body)
		sh(t, wt, "git", "add", name)
	}
	sh(t, wt, "git", "commit", "-qm", "change "+name)
	sh(t, e.root, "git", "worktree", "remove", "--force", wt)
}

func put(t *testing.T, e env, text string, paths ...string) Fact {
	t.Helper()
	var ps []Path
	for _, p := range paths {
		ps = append(ps, Path{Path: p})
	}
	f, _, err := e.m.Put(context.Background(), Fact{Kind: KindGotcha, Text: text, Author: "eng-1", Paths: ps})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestRoundTripAndDeterministicID(t *testing.T) {
	e := newEnv(t)
	f := put(t, e, "Lane guard: resolves $VARs, ignores heredocs: \"x\" # y", "b.go", "a.go")
	got, err := e.m.Get(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, f) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, f)
	}
	if f.ID != ID(KindGotcha, "  lane GUARD: resolves $VARs, ignores heredocs: \"x\" # y ", []Path{{Path: "a.go"}, {Path: "b.go"}}) {
		t.Fatal("id not deterministic over normalised text and sorted paths")
	}
	if f.Created != "2026-10-22" || f.IntegrationSHA == "" || f.Paths[0].Blob == "" || f.Paths[0].Path != "a.go" {
		t.Fatalf("not grounded: %+v", f)
	}
	again, existed, err := e.m.Put(context.Background(), Fact{Kind: KindGotcha, Text: f.Text, Paths: []Path{{Path: "a.go"}, {Path: "b.go"}}})
	if err != nil || !existed || again.ID != f.ID {
		t.Fatalf("second put = %+v existed=%v err=%v", again, existed, err)
	}
}

func TestParseRejects(t *testing.T) {
	for name, in := range map[string]string{
		"no frontmatter": "hello",
		"unterminated":   "---\nid: f-1\n",
		"bad kind":       "---\nid: f-1\nkind: nope\nstatus: proposed\n---\ntext\n",
		"bad status":     "---\nid: f-1\nkind: gotcha\nstatus: stale\n---\ntext\n",
		"empty text":     "---\nid: f-1\nkind: gotcha\nstatus: proposed\n---\n",
		"unknown field":  "---\nid: f-1\nkind: gotcha\nstatus: proposed\nbogus: 1\n---\ntext\n",
	} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	if _, err := Parse([]byte("---\nid: f-1\nkind: gotcha\nstatus: proposed\npaths:\n  - {path: a.go, blob: abc}\ncreated: 2026-10-22\n---\ntext\n")); err != nil {
		t.Fatalf("flow-style paths and bare date: %v", err)
	}
}

func TestAtomicWriteLeavesNoPartialFile(t *testing.T) {
	e := newEnv(t)
	e.m.afterTemp = func() error { return errors.New("crash") }
	_, _, err := e.m.Put(context.Background(), Fact{Kind: KindGotcha, Text: "x", Paths: []Path{{Path: "a.go"}}})
	if err == nil {
		t.Fatal("expected failure")
	}
	ents, _ := os.ReadDir(e.m.Dir)
	if len(ents) != 0 {
		t.Fatalf("leftovers: %v", ents)
	}
	if rows, _ := e.m.DB.ListFacts(); len(rows) != 0 {
		t.Fatalf("index row without file: %v", rows)
	}
}

func TestConcurrentWritesKeepIndexConsistent(t *testing.T) {
	e := newEnv(t)
	var wg sync.WaitGroup
	for _, txt := range []string{"fact one", "fact two", "fact three", "fact four"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := e.m.Put(context.Background(), Fact{Kind: KindConvention, Text: txt, Paths: []Path{{Path: "a.go"}}}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	facts, bad, _ := e.m.List()
	rows, _ := e.m.DB.ListFacts()
	if len(facts) != 4 || len(bad) != 0 || len(rows) != 4 {
		t.Fatalf("files=%d invalid=%d rows=%d", len(facts), len(bad), len(rows))
	}
}

func TestCheckFreshModifiedDeletedRenamedAndOneLsTree(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	fa := put(t, e, "about a", "a.go", "b.go")
	count := func() int {
		n := 0
		for _, c := range *e.calls {
			if len(c) > 0 && c[0] == "ls-tree" {
				n++
			}
		}
		return n
	}
	*e.calls = nil
	r, err := e.m.Check(ctx, fa)
	if err != nil || r.Stale {
		t.Fatalf("unchanged = %+v %v", r, err)
	}
	if len(*e.calls) != 1 || count() != 1 {
		t.Fatalf("git calls = %v, want exactly one ls-tree", *e.calls)
	}

	commitOnIntegration(t, e, "a.go", "package a // changed\n")
	if r, _ := e.m.Check(ctx, fa); !r.Stale || !strings.Contains(r.Reason, "a.go changed") {
		t.Fatalf("modified = %+v", r)
	}
	fb := put(t, e, "about b only", "b.go")
	if r, _ := e.m.Check(ctx, fb); r.Stale {
		t.Fatal("untouched path went stale")
	}
	commitOnIntegration(t, e, "b.go", "") // deleted
	if r, _ := e.m.Check(ctx, fb); !r.Stale || !strings.Contains(r.Reason, "no longer exists") {
		t.Fatalf("deleted = %+v", r)
	}
	// renamed: the old path is gone
	fc := put(t, e, "about a again", "a.go")
	wt := filepath.Join(t.TempDir(), "wt")
	sh(t, e.root, "git", "worktree", "add", "-q", wt, "slai/integration")
	sh(t, wt, "git", "mv", "a.go", "renamed.go")
	sh(t, wt, "git", "commit", "-qm", "rename")
	sh(t, e.root, "git", "worktree", "remove", "--force", wt)
	if r, _ := e.m.Check(ctx, fc); !r.Stale {
		t.Fatalf("renamed = %+v", r)
	}
	// unknown path at record time
	if _, _, err := e.m.Put(ctx, Fact{Kind: KindGotcha, Text: "ghost", Paths: []Path{{Path: "nope.go"}}}); !errors.Is(err, ErrUnknownPath) {
		t.Fatalf("unknown path err = %v", err)
	}
	if _, _, err := e.m.Put(ctx, Fact{Kind: KindGotcha, Text: "a dir", Paths: []Path{{Path: ".slai"}}}); !errors.Is(err, ErrUnknownPath) {
		t.Fatalf("directory err = %v", err)
	}
}

func TestChecksIntegrationNotWorktree(t *testing.T) {
	e := newEnv(t)
	f := put(t, e, "about a", "a.go")
	sh(t, e.root, "git", "checkout", "-q", "-b", "work")
	write(t, e.root, "a.go", "package a // uncommitted\n")
	if r, err := e.m.Check(context.Background(), f); err != nil || r.Stale {
		t.Fatalf("worktree change leaked into check: %+v %v", r, err)
	}
	sh(t, e.root, "git", "commit", "-qam", "on work branch")
	if r, _ := e.m.Check(context.Background(), f); r.Stale {
		t.Fatal("a commit on another branch leaked into check")
	}
}

func TestReindexRebuildsIdenticalRows(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	put(t, e, "about a", "a.go")
	put(t, e, "about b", "b.go")
	put(t, e, "no paths at all")
	if err := os.WriteFile(filepath.Join(e.m.Dir, "f-deadbeef.md"), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	commitOnIntegration(t, e, "b.go", "package b // v2\n")
	if err := e.m.Reindex(ctx); err != nil {
		t.Fatal(err)
	}
	want, _ := e.m.DB.ListFacts()
	if len(want) != 4 {
		t.Fatalf("rows = %d", len(want))
	}
	var stale, invalid int
	for _, r := range want {
		if r.Stale {
			stale++
		}
		if r.Status == "invalid" {
			invalid++
		}
	}
	if stale != 1 || invalid != 1 {
		t.Fatalf("stale=%d invalid=%d", stale, invalid)
	}
	for _, r := range want {
		if err := e.m.DB.DeleteFact(r.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.m.Reindex(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := e.m.DB.ListFacts()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rebuilt rows differ:\n got %+v\nwant %+v", got, want)
	}
	// reindexing again changes nothing (clock is fixed)
	if err := e.m.Reindex(ctx); err != nil {
		t.Fatal(err)
	}
	if again, _ := e.m.DB.ListFacts(); !reflect.DeepEqual(again, want) {
		t.Fatal("reindex is not idempotent")
	}
	// a deleted file drops its row
	if err := os.Remove(filepath.Join(e.m.Dir, want[0].ID+".md")); err != nil {
		t.Fatal(err)
	}
	_ = e.m.Reindex(ctx)
	if rows, _ := e.m.DB.ListFacts(); len(rows) != 3 {
		t.Fatalf("rows after file delete = %d", len(rows))
	}
}

// Verify updates the index before it renames the file, and a failure leaves both as they were.
func TestVerifyUpdatesIndexBeforeRename(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	f := put(t, e, "about a", "a.go")
	e.m.beforeRename = func() {
		row, _ := e.m.DB.GetFact(f.ID)
		onDisk, _ := e.m.Get(f.ID)
		if row.Status != StatusVerified || row.VerifiedBy != "principal" || onDisk.Status != StatusProposed {
			t.Errorf("at rename time: row %q/%q, file %q (want the row ahead of the file)", row.Status, row.VerifiedBy, onDisk.Status)
		}
	}
	got, err := e.m.Verify(ctx, f.ID, "principal")
	if err != nil || got.Status != StatusVerified {
		t.Fatalf("verify: %+v %v", got, err)
	}
	if onDisk, _ := e.m.Get(f.ID); onDisk.VerifiedBy != "principal" {
		t.Fatalf("file after verify: %+v", onDisk)
	}
	// a failed rewrite restores the row
	e.m.beforeRename = nil
	g := put(t, e, "about b", "b.go")
	e.m.afterTemp = func() error { return errors.New("crash") }
	if _, err := e.m.Verify(ctx, g.ID, "principal"); err == nil {
		t.Fatal("expected failure")
	}
	if row, _ := e.m.DB.GetFact(g.ID); row.Status != StatusProposed {
		t.Fatalf("row after failed verify: %+v", row)
	}
	// the pre step failing also leaves the proposed file in place
	e.m.afterTemp = nil
	if onDisk, _ := e.m.Get(g.ID); onDisk.Status != StatusProposed {
		t.Fatalf("file after failed verify: %+v", onDisk)
	}
}

// Views asks git once for every cited path of every fact, and agrees with View per fact.
func TestViewsUseOneLsTreeAndAgreeWithViewPerFact(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	fa := put(t, e, "about a", "a.go")
	fb := put(t, e, "about b", "b.go")
	put(t, e, "about both", "a.go", "b.go")
	commitOnIntegration(t, e, "b.go", "package b // changed\n")
	*e.calls = nil
	vs, bad, err := e.m.Views(ctx)
	if err != nil || len(bad) != 0 || len(vs) != 3 {
		t.Fatalf("views = %d bad=%d err=%v", len(vs), len(bad), err)
	}
	n := 0
	for _, c := range *e.calls {
		if len(c) > 0 && c[0] == "ls-tree" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d ls-tree calls for 3 facts, want 1: %v", n, *e.calls)
	}
	states := map[string]string{}
	for _, v := range vs {
		one, err := e.m.View(ctx, v.Fact)
		if err != nil || one.State != v.State || one.Reason != v.Reason {
			t.Errorf("%s: batch %s/%q, single %s/%q (%v)", v.ID, v.State, v.Reason, one.State, one.Reason, err)
		}
		states[v.ID] = v.State
	}
	if states[fb.ID] != StateStale || states[fa.ID] == StateStale {
		t.Errorf("states = %v", states)
	}
	blobs, err := e.m.CurrentBlobs(ctx, fb)
	if err != nil || blobs["b.go"] == "" || blobs["b.go"] == fb.Paths[0].Blob {
		t.Errorf("current blobs = %v %v", blobs, err)
	}
}
