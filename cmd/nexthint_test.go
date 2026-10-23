package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/supervisor"
)

// project makes a scratch repo dir with a solo team (and chdirs into it).
func project(t *testing.T, withTeam bool) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	if withTeam {
		if err := os.MkdirAll(filepath.Join(dir, ".slai"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := config.Write(filepath.Join(dir, ".slai", "team.yaml"), config.Preset("solo", "demo", "x", "main")); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func runStatus(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	c := statusCmd()
	c.SetOut(&buf)
	c.SetArgs(nil)
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	err := c.ExecuteContext(context.Background())
	w.Close()
	os.Stdout = old
	b := new(bytes.Buffer)
	b.ReadFrom(r)
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestStatusGoldenNoTeam(t *testing.T) {
	project(t, false)
	if got, want := runStatus(t), noTeamMessage+"\n"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if strings.Contains(noTeamMessage, "no such file") || !strings.Contains(noTeamMessage, "Next: `slai init` (or `slai`, which runs it)") {
		t.Fatal(noTeamMessage)
	}
}

func TestStatusGoldenSupervisorDown(t *testing.T) {
	project(t, true)
	want := "orch         orchestrator         opus     -                \n" +
		"eng-1        engineer             sonnet   -                \n" +
		"\n" +
		"supervisor: not running. Next: `slai`\n"
	if got := runStatus(t); got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestStatusGoldenUpNoTasksAndRunning(t *testing.T) {
	dir := project(t, true)
	l, err := supervisor.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	got := runStatus(t)
	if !strings.HasSuffix(got, "Next: `slai send orch \"<goal>\"`\n") || !strings.Contains(got, "supervisor: running") {
		t.Fatalf("no tasks:\n%s", got)
	}
	st, err := store.Open(filepath.Join(dir, ".slai", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTask(store.Task{Title: "do it", Status: store.StatusInProgress}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	got = runStatus(t)
	if !strings.HasSuffix(got, "Next: `slai serve` or `b` in the dashboard\n") {
		t.Fatalf("tasks running:\n%s", got)
	}
}

func TestHelpHintPerState(t *testing.T) {
	dir := project(t, false)
	if got := helpHint(dir); got != "Next: `slai init`" {
		t.Fatalf("no team: %q", got)
	}
	dir = project(t, true)
	if got := helpHint(dir); got != "Next: `slai`" {
		t.Fatalf("supervisor down: %q", got)
	}
	l, err := supervisor.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if got := helpHint(dir); got != "Next: `slai send orch \"<goal>\"`" {
		t.Fatalf("up, no tasks: %q", got)
	}
	st, _ := store.Open(filepath.Join(dir, ".slai", "state.db"))
	if _, err := st.CreateTask(store.Task{Title: "do it", Status: store.StatusInProgress}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if got := helpHint(dir); got != "Next: `slai serve` or `b` in the dashboard" {
		t.Fatalf("tasks running: %q", got)
	}
}

func TestRootHelpEndsWithFooterAndHint(t *testing.T) {
	project(t, false)
	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(buf.String(), "\n"+rootHelpFooter+"\nNext: `slai init`\n") {
		t.Fatalf("help tail:\n%s", buf.String())
	}
}

func TestUpHelpNamesSlai(t *testing.T) {
	if got := upCmd().Short; got != "Start all agents and the supervisor without the dashboard (`slai` does this and opens it)" {
		t.Fatal(got)
	}
}

func TestInitCostNoticeSaysWillStart(t *testing.T) {
	got := initCostNotice(costTeam(0, 0, "", ""))
	if want := "Will start 2 Claude sessions when you run `slai`. Spend caps: OFF (set defaults.spend_caps in .slai/team.yaml)"; got != want {
		t.Fatalf("got %q", got)
	}
}
