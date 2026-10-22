package cmd

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/memory"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// memFixture: a repo whose slai/integration branch holds a small Go project.
func memFixture(t *testing.T) (*memEnv, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	gitIn(t, dir, "init", "-b", "main")
	writeFile(t, dir, ".gitignore", ".slai/\n")
	writeFile(t, dir, "internal/lane/lane.go", "// Package lane decides where an agent may write. It is strict.\npackage lane\n")
	writeFile(t, dir, "internal/store/store.go", "// Package store keeps state in SQLite.\npackage store\n")
	writeFile(t, dir, "cmd/app/main.go", "package main\n\nfunc main() {}\n")
	writeFile(t, dir, "docs/README.md", "# Docs\n\nHow things work.\n")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-m", "init")
	gitIn(t, dir, "branch", "slai/integration")
	var out bytes.Buffer
	e, st, err := openMemEnv(dir, &out, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e.now = func() time.Time { return time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC) }
	e.mem.Now = e.now
	return e, &out
}

func (e *memEnv) put(t *testing.T, f memory.Fact) memory.Fact {
	t.Helper()
	if f.Author == "" {
		f.Author = "eng-1"
	}
	got, _, err := e.mem.Put(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// changeOnIntegration commits a new version of rel on slai/integration.
func changeOnIntegration(t *testing.T, e *memEnv, rel, body string) {
	t.Helper()
	gitIn(t, e.dir, "checkout", "-q", "slai/integration")
	writeFile(t, e.dir, rel, body)
	gitIn(t, e.dir, "commit", "-qam", "change "+rel)
	gitIn(t, e.dir, "checkout", "-q", "main")
}

func lanePath() []memory.Path { return []memory.Path{{Path: "internal/lane/lane.go"}} }

// AC 1: ls columns and states; --stale filters; show prints file + recorded vs current.
func TestMemoryLsAndShow(t *testing.T) {
	e, out := memFixture(t)
	ctx := context.Background()
	fresh := e.put(t, memory.Fact{Kind: memory.KindGotcha, Text: "Lane guard resolves variables first.", Status: memory.StatusVerified, VerifiedBy: "principal", Paths: lanePath()})
	unver := e.put(t, memory.Fact{Kind: memory.KindConvention, Text: "Stores return ErrNotFound.", Paths: []memory.Path{{Path: "internal/store/store.go"}}})
	nopath := e.put(t, memory.Fact{Kind: memory.KindCommand, Text: "go test ./...", Status: memory.StatusVerified, VerifiedBy: "principal"})
	old := e.put(t, memory.Fact{Kind: memory.KindArchitecture, Text: "App entry is cmd/app.", Status: memory.StatusVerified, VerifiedBy: "principal", Paths: []memory.Path{{Path: "cmd/app/main.go"}}})
	writeFile(t, e.mem.Dir, "f-badbad00.md", "not a fact\n")
	changeOnIntegration(t, e, "cmd/app/main.go", "package main\n\nfunc main() { println() }\n")

	if err := e.ls(ctx, false); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{fresh.ID: "fresh", unver.ID: "unverified", nopath.ID: "unverified", old.ID: "stale", "f-badbad00": "invalid"}
	lines := strings.Split(out.String(), "\n")
	if !strings.HasPrefix(lines[0], "ID") || !strings.Contains(lines[0], "KIND") || !strings.Contains(lines[0], "STATUS") || !strings.Contains(lines[0], "PATH") || !strings.Contains(lines[0], "AGE") {
		t.Fatalf("header = %q", lines[0])
	}
	for id, st := range want {
		found := false
		for _, l := range lines[1:] {
			if strings.HasPrefix(l, id) {
				found = true
				if f := strings.Fields(l); f[2] != st {
					t.Errorf("%s: state %q, want %q (%q)", id, f[2], st, l)
				}
			}
		}
		if !found {
			t.Errorf("%s missing from ls:\n%s", id, out.String())
		}
	}
	if !strings.Contains(out.String(), "internal/lane/lane.go") || !strings.Contains(out.String(), "0d") || !strings.Contains(out.String(), "! f-badbad00 is invalid") {
		t.Errorf("ls output:\n%s", out.String())
	}

	out.Reset()
	if err := e.ls(ctx, true); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, old.ID) || strings.Contains(s, fresh.ID) || strings.Contains(s, "f-badbad00") {
		t.Fatalf("--stale:\n%s", s)
	}

	out.Reset()
	if err := e.show(ctx, old.ID); err != nil {
		t.Fatal(err)
	}
	cur := gitIn(t, e.dir, "rev-parse", "slai/integration:cmd/app/main.go")
	s := out.String()
	for _, w := range []string{"id: " + old.ID, "App entry is cmd/app.", "grounding against slai/integration:",
		"recorded " + short(old.Paths[0].Blob), "current " + short(cur), "CHANGED", "status: stale"} {
		if !strings.Contains(s, w) {
			t.Errorf("show missing %q:\n%s", w, s)
		}
	}
	out.Reset()
	_ = e.show(ctx, fresh.ID)
	if s := out.String(); !strings.Contains(s, "  ok") || !strings.Contains(s, "status: fresh") {
		t.Errorf("show fresh:\n%s", s)
	}
	if err := e.show(ctx, "f-nope"); err == nil {
		t.Error("show of an unknown id should fail")
	}
}

func TestMemoryLsEmpty(t *testing.T) {
	e, out := memFixture(t)
	_ = e.ls(context.Background(), false)
	if !strings.Contains(out.String(), "No facts yet.") {
		t.Fatalf("%q", out.String())
	}
}

// AC 2 (M4) and T-0105: verify shows the command, asks, then runs it.
func TestMemoryVerifyRunsCheckAndSetsUser(t *testing.T) {
	e, out := memFixture(t)
	ctx := context.Background()
	ok := e.put(t, memory.Fact{Kind: memory.KindGotcha, Text: "Passes its check.", Paths: lanePath(), Check: "true"})
	bad := e.put(t, memory.Fact{Kind: memory.KindGotcha, Text: "Fails its check.", Paths: lanePath(), Check: "echo nope; false"})
	none := e.put(t, memory.Fact{Kind: memory.KindGotcha, Text: "Has no check.", Paths: lanePath()})
	yes := verifyOpts{Yes: true, AllowUnverified: true}

	if err := e.verify(ctx, ok.ID, yes); err != nil {
		t.Fatal(err)
	}
	f, _ := e.mem.Get(ok.ID)
	if f.Status != memory.StatusVerified || f.VerifiedBy != "user" {
		t.Fatalf("after verify: %+v", f)
	}
	if err := e.verify(ctx, bad.ID, yes); err == nil || !strings.Contains(out.String(), "nope") {
		t.Fatalf("failing check: err=%v out=%q", err, out.String())
	}
	if f, _ := e.mem.Get(bad.ID); f.Status != memory.StatusProposed || f.VerifiedBy != "" {
		t.Fatalf("failed check changed the fact: %+v", f)
	}
	if err := e.verify(ctx, none.ID, yes); err == nil || !strings.Contains(err.Error(), "no check command") {
		t.Fatalf("err = %v", err)
	}
}

// fakeRun records what would have run.
type fakeRun struct{ ran []string }

func (f *fakeRun) run(_ context.Context, c string) ([]byte, error) {
	f.ran = append(f.ran, c)
	return []byte("ran\n"), nil
}

// T-0105 AC1, AC4: the prompt shows the id, who wrote it and the command verbatim; an n or an
// empty answer runs nothing; a y runs it.
func TestMemoryVerifyAsksFirst(t *testing.T) {
	cmdline := `go test ./internal/lane -run 'TestX' && echo "a  b" | grep -c b`
	for _, tc := range []struct {
		name, answer string
		runs         bool
	}{{"n", "n\n", false}, {"empty", "\n", false}, {"EOF", "", false}, {"other", "maybe\n", false}, {"y", "y\n", true}, {"yes", "YES\n", true}} {
		t.Run(tc.name, func(t *testing.T) {
			e, out := memFixture(t)
			fr := &fakeRun{}
			e.runCheck, e.tty, e.in = fr.run, true, strings.NewReader(tc.answer)
			f := e.put(t, memory.Fact{Kind: memory.KindGotcha, Text: "Checked fact.", Paths: lanePath(), Check: cmdline, Status: memory.StatusVerified, VerifiedBy: "principal", Author: "eng-3"})
			if err := e.verify(context.Background(), f.ID, verifyOpts{}); err != nil {
				t.Fatal(err)
			}
			if (len(fr.ran) == 1) != tc.runs || (tc.runs && fr.ran[0] != cmdline) {
				t.Fatalf("ran %q, want run=%v", fr.ran, tc.runs)
			}
			want := "fact " + f.ID + " (eng-3, verified by principal)\ncheck command:\n  " + cmdline + "\nRun this check? [y/N] "
			if !strings.HasPrefix(out.String(), want) {
				t.Errorf("printed:\n%q\nwant prefix:\n%q", out.String(), want)
			}
			got, _ := e.mem.Get(f.ID)
			if tc.runs != (got.VerifiedBy == "user") {
				t.Errorf("verified_by = %q after answer %q", got.VerifiedBy, tc.answer)
			}
		})
	}
}

// T-0105 AC2: with no terminal the check is refused unless --yes is given.
func TestMemoryVerifyWithoutTTYNeedsYes(t *testing.T) {
	e, out := memFixture(t)
	fr := &fakeRun{}
	e.runCheck, e.tty = fr.run, false
	f := e.put(t, memory.Fact{Kind: memory.KindGotcha, Text: "Checked fact.", Paths: lanePath(), Check: "true", Status: memory.StatusVerified, VerifiedBy: "principal"})
	err := e.verify(context.Background(), f.ID, verifyOpts{})
	if err == nil || !strings.Contains(err.Error(), "no terminal") || !strings.Contains(err.Error(), "--yes") || len(fr.ran) != 0 {
		t.Fatalf("err=%v ran=%v", err, fr.ran)
	}
	if !strings.Contains(out.String(), "check command:\n  true") {
		t.Errorf("the command should still be shown:\n%s", out.String())
	}
	if err := e.verify(context.Background(), f.ID, verifyOpts{Yes: true}); err != nil || len(fr.ran) != 1 {
		t.Fatalf("--yes: err=%v ran=%v", err, fr.ran)
	}
}

// T-0105 AC3: a proposed or stale fact's check needs --allow-unverified, even with --yes.
func TestMemoryVerifyUnverifiedNeedsFlag(t *testing.T) {
	e, _ := memFixture(t)
	fr := &fakeRun{}
	e.runCheck = fr.run
	proposed := e.put(t, memory.Fact{Kind: memory.KindGotcha, Text: "Only proposed.", Paths: lanePath(), Check: "true"})
	stale := e.put(t, memory.Fact{Kind: memory.KindGotcha, Text: "Verified but stale.", Paths: []memory.Path{{Path: "cmd/app/main.go"}}, Check: "true", Status: memory.StatusVerified, VerifiedBy: "principal"})
	changeOnIntegration(t, e, "cmd/app/main.go", "package main\n\nfunc main() { println() }\n")
	for name, id := range map[string]string{"proposed": proposed.ID, "stale": stale.ID} {
		err := e.verify(context.Background(), id, verifyOpts{Yes: true})
		if err == nil || !strings.Contains(err.Error(), "--allow-unverified") || !strings.Contains(err.Error(), name) || len(fr.ran) != 0 {
			t.Fatalf("%s: err=%v ran=%v", name, err, fr.ran)
		}
	}
	if err := e.verify(context.Background(), proposed.ID, verifyOpts{Yes: true, AllowUnverified: true}); err != nil || len(fr.ran) != 1 {
		t.Fatalf("with the flag: err=%v ran=%v", err, fr.ran)
	}
}

func TestMemoryVerifyFlags(t *testing.T) {
	v, _, _ := memoryCmd().Find([]string{"verify"})
	for _, f := range []string{"yes", "allow-unverified"} {
		if v.Flags().Lookup(f) == nil {
			t.Errorf("verify has no --%s", f)
		}
	}
}

// AC 2: gc deletes only stale facts unused for 30 days, after asking; --yes skips the question.
func TestMemoryGC(t *testing.T) {
	setup := func(t *testing.T) (*memEnv, *bytes.Buffer, map[string]string) {
		e, out := memFixture(t)
		realNow := e.now
		e.mem.Now = func() time.Time { return realNow().AddDate(0, 0, -60) }
		oldStale := e.put(t, memory.Fact{Kind: memory.KindGotcha, Text: "Old stale one.", Paths: []memory.Path{{Path: "cmd/app/main.go"}}})
		oldFresh := e.put(t, memory.Fact{Kind: memory.KindGotcha, Text: "Old but fresh.", Paths: lanePath()})
		usedStale := e.put(t, memory.Fact{Kind: memory.KindGotcha, Text: "Old stale but used.", Paths: []memory.Path{{Path: "internal/store/store.go"}}})
		e.mem.Now = realNow
		newStale := e.put(t, memory.Fact{Kind: memory.KindGotcha, Text: "New stale one.", Paths: []memory.Path{{Path: "docs/README.md"}}})
		if err := e.mem.DB.TouchFacts([]string{usedStale.ID}, realNow().AddDate(0, 0, -2).Unix()); err != nil {
			t.Fatal(err)
		}
		changeOnIntegration(t, e, "cmd/app/main.go", "package main\n\nfunc main() { println() }\n")
		changeOnIntegration(t, e, "internal/store/store.go", "// Package store changed.\npackage store\n")
		changeOnIntegration(t, e, "docs/README.md", "# Docs v2\n")
		return e, out, map[string]string{"oldStale": oldStale.ID, "oldFresh": oldFresh.ID, "usedStale": usedStale.ID, "newStale": newStale.ID}
	}
	exists := func(e *memEnv, id string) bool {
		_, err := os.Stat(filepath.Join(e.mem.Dir, id+".md"))
		return err == nil
	}
	t.Run("asks and a no deletes nothing", func(t *testing.T) {
		e, out, ids := setup(t)
		e.in = strings.NewReader("n\n")
		if err := e.gc(context.Background(), false); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), ids["oldStale"]) || !strings.Contains(out.String(), "[y/N]") || strings.Contains(out.String(), ids["newStale"]) || strings.Contains(out.String(), ids["usedStale"]) {
			t.Fatalf("listing:\n%s", out.String())
		}
		for _, id := range ids {
			if !exists(e, id) {
				t.Fatalf("%s deleted without consent", id)
			}
		}
	})
	t.Run("yes deletes only the old unused stale fact", func(t *testing.T) {
		e, _, ids := setup(t)
		e.in = strings.NewReader("")
		if err := e.gc(context.Background(), true); err != nil {
			t.Fatal(err)
		}
		if exists(e, ids["oldStale"]) {
			t.Error("old stale fact survived")
		}
		for _, k := range []string{"oldFresh", "usedStale", "newStale"} {
			if !exists(e, ids[k]) {
				t.Errorf("%s was deleted", k)
			}
		}
		if _, err := e.mem.DB.GetFact(ids["oldStale"]); err == nil {
			t.Error("index row survived")
		}
	})
	t.Run("a y answer deletes", func(t *testing.T) {
		e, _, ids := setup(t)
		e.in = strings.NewReader("y\n")
		_ = e.gc(context.Background(), false)
		if exists(e, ids["oldStale"]) {
			t.Error("not deleted after y")
		}
	})
}

// AC 3: doctor section.
func TestDoctorMemorySection(t *testing.T) {
	e, out := memFixture(t)
	ctx := context.Background()
	if n := doctorMemory(ctx, e.dir, out); n != 0 || out.Len() != 0 {
		t.Fatalf("no memory dir should print nothing: %d %q", n, out.String())
	}
	f1 := e.put(t, memory.Fact{Kind: memory.KindGotcha, Text: "Fresh verified.", Status: memory.StatusVerified, VerifiedBy: "principal", Paths: lanePath()})
	e.put(t, memory.Fact{Kind: memory.KindGotcha, Text: "Proposed only.", Paths: lanePath()})
	f3 := e.put(t, memory.Fact{Kind: memory.KindGotcha, Text: "Verified then stale.", Status: memory.StatusVerified, VerifiedBy: "principal", Paths: []memory.Path{{Path: "cmd/app/main.go"}}})
	writeFile(t, e.mem.Dir, "f-badbad00.md", "---\nnonsense\n")
	changeOnIntegration(t, e, "cmd/app/main.go", "package main\n\nfunc main() { println() }\n")
	_ = f1
	memoryFileWarn = 3
	t.Cleanup(func() { memoryFileWarn = 500 })

	n := doctorMemory(ctx, e.dir, out)
	got := out.String()
	for _, w := range []string{"✓ team memory: 4 fact(s): 1 fresh, 1 stale, 1 unverified, 1 invalid",
		"! warn: memory file f-badbad00 is invalid",
		"! warn: verified fact " + f3.ID + " is stale",
		"! warn: 4 memory files (over 3)"} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in\n%s", w, got)
		}
	}
	if n != 0 {
		t.Errorf("warnings must not count as problems: %d", n)
	}
}

// AC 4: map is deterministic, lists packages and verify commands; an empty repo says so.
func TestMemoryMap(t *testing.T) {
	e, out := memFixture(t)
	e.test = "go test ./..."
	e.put(t, memory.Fact{Kind: memory.KindCommand, Text: "go vet ./...", Status: memory.StatusVerified, VerifiedBy: "principal"})
	if err := e.projectMap(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := out.String()
	out.Reset()
	_ = e.projectMap(context.Background())
	if out.String() != first {
		t.Fatalf("map is not deterministic:\n%s\n---\n%s", first, out.String())
	}
	want := `## Packages
- cmd/app — (no description)
- docs — Docs
- internal/lane — Package lane decides where an agent may write
- internal/store — Package store keeps state in SQLite

## Entry points
- cmd/app/main.go

## Verify
- go test ./...
- go vet ./...
`
	if first != want {
		t.Fatalf("map:\n%s\nwant:\n%s", first, want)
	}

	empty := t.TempDir()
	s, err := memory.ProjectMap(empty, memory.MapOptions{})
	if err != nil || !strings.Contains(s, "No project map") {
		t.Fatalf("empty: %q %v", s, err)
	}
}

func TestMemoryCommandRegistered(t *testing.T) {
	c := memoryCmd()
	for _, n := range []string{"ls", "show", "verify", "gc", "map"} {
		if sub, _, err := c.Find([]string{n}); err != nil || sub.Name() != n {
			t.Errorf("no %s subcommand", n)
		}
	}
	if ls, _, _ := c.Find([]string{"ls"}); ls.Flags().Lookup("stale") == nil {
		t.Error("ls has no --stale")
	}
}
