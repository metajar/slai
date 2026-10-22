package demo

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/oorbt/slai/internal/store"
)

// T-0107 AC1: the demo refuses a directory that could be a real project.
func TestSeedRefusesRealProjects(t *testing.T) {
	ctx := context.Background()
	mk := func(files map[string]string) string {
		d := t.TempDir()
		for n, b := range files {
			p := filepath.Join(d, n)
			_ = os.MkdirAll(filepath.Dir(p), 0o755)
			if err := os.WriteFile(p, []byte(b), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return d
	}
	for name, d := range map[string]string{
		"a project with a team.yaml": mk(map[string]string{".slai/team.yaml": "version: 1\n"}),
		"a project with a state.db":  mk(map[string]string{".slai/state.db": "x"}),
		"a git checkout":             mk(map[string]string{".git/HEAD": "ref: refs/heads/main\n"}),
		"any other non-empty folder": mk(map[string]string{"main.go": "package main\n"}),
	} {
		if _, err := Seed(ctx, d, Options{}); err == nil {
			t.Errorf("%s was seeded", name)
		}
		if _, err := os.Stat(filepath.Join(d, ".slai", DEMOMarker)); err == nil {
			t.Errorf("%s: a marker was written", name)
		}
	}
	// the real project's files are untouched by a refused seed
	d := mk(map[string]string{".slai/team.yaml": "version: 1\n", "main.go": "package main\n"})
	_, _ = Seed(ctx, d, Options{})
	if b, _ := os.ReadFile(filepath.Join(d, "main.go")); string(b) != "package main\n" {
		t.Error("a refused seed changed a file")
	}
	// empty, missing and previous demos are fine
	if p, err := Seed(ctx, filepath.Join(t.TempDir(), "new"), Options{}); err != nil {
		t.Fatalf("missing dir: %v", err)
	} else {
		dir := p.Dir
		p.Store.Close()
		again, err := Seed(ctx, dir, Options{FirstRun: true})
		if err != nil {
			t.Fatalf("a previous demo: %v", err)
		}
		again.Store.Close()
	}
	if p, err := Seed(ctx, t.TempDir(), Options{}); err != nil {
		t.Fatalf("empty dir: %v", err)
	} else {
		p.Store.Close()
	}
}

func TestSeedFillsEveryColumnAndState(t *testing.T) {
	p, err := Seed(context.Background(), t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Store.Close()
	st := p.Store
	seen := map[string]int{}
	tasks, _ := st.ListTasks("")
	for _, k := range tasks {
		seen[k.Status]++
	}
	for _, s := range []string{store.StatusAssigned, store.StatusInProgress, store.StatusBlocked, store.StatusInReview, store.StatusMerged,
		store.StatusCancelled, store.StatusReverted, store.StatusChangesRequested} {
		if seen[s] == 0 {
			t.Errorf("no task in %s (%v)", s, seen)
		}
	}
	if w, _ := st.WaitingOn("T-0009"); len(w) == 0 {
		t.Error("no task waits on another")
	}
	if _, claims, _ := st.LatestACClaims("T-0007"); len(claims) != 3 {
		t.Errorf("T-0007 claims = %v", claims)
	}
	if un, _ := st.UnansweredClaims("T-0008"); len(un) != 1 {
		t.Errorf("T-0008 unanswered = %v, want one", un)
	}
	if esc, _ := st.OpenEscalations(); len(esc) < 2 {
		t.Errorf("escalations = %v", esc)
	}
	if rows, _ := st.ListFacts(); len(rows) != 5 {
		t.Errorf("memory rows = %d, want 5 (4 facts and one invalid file)", len(rows))
	}
	statuses := map[string]bool{}
	agents, _ := st.GetAgents()
	for _, a := range agents {
		statuses[a.Status] = true
	}
	for _, s := range []string{"working", "waiting_on_user", "rate_limited", "dead", "idle"} {
		if !statuses[s] {
			t.Errorf("no agent is %s", s)
		}
	}
	// first run: a team and nothing else
	fr, err := Seed(context.Background(), t.TempDir(), Options{FirstRun: true})
	if err != nil {
		t.Fatal(err)
	}
	defer fr.Store.Close()
	if ts, _ := fr.Store.ListTasks(""); len(ts) != 0 {
		t.Errorf("first run has %d tasks", len(ts))
	}
}
