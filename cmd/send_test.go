package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/store"
)

func squad() *config.Team { return config.Preset("full", "demo", "x", "main") }

func TestSendTargets(t *testing.T) {
	team := squad()
	var engs []string
	for _, a := range team.Agents {
		if a.Role == config.RoleEngineer {
			engs = append(engs, a.ID)
		}
	}
	if len(engs) < 2 {
		t.Fatalf("preset needs >=2 engineers, got %v", engs)
	}
	all, err := sendTargets(team, "all")
	if err != nil || len(all) != len(team.Agents) {
		t.Fatalf("all = %v, %v", all, err)
	}
	for _, name := range []string{"engineers", "engineer"} {
		got, err := sendTargets(team, name)
		if err != nil || !reflect.DeepEqual(got, engs) {
			t.Errorf("%s = %v, %v; want %v", name, got, err, engs)
		}
	}
	if got, err := sendTargets(team, "focus-group"); err != nil || len(got) == 0 {
		t.Errorf("focus-group = %v, %v", got, err)
	}
	if got, err := sendTargets(team, "orch"); err != nil || !reflect.DeepEqual(got, []string{"orch"}) {
		t.Errorf("orch = %v, %v", got, err)
	}
}

func TestSendTargetsUnknownListsValid(t *testing.T) {
	_, err := sendTargets(squad(), "nobody")
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{`"nobody"`, "orch", "agents:", "groups:", "all", "engineer", "focus-group"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

func TestSendTargetsRoleAbsent(t *testing.T) {
	// solo has no focus group: a valid role name with no members is an error, not a silent no-op.
	if got, err := sendTargets(config.Preset("solo", "demo", "x", "main"), "focus-group"); err == nil {
		t.Fatalf("got %v, want error", got)
	}
}

func TestSendTargetsAgentIDWinsOverGroup(t *testing.T) {
	team := squad()
	team.Agents[1].ID = "engineers" // collides with the engineer group
	got, err := sendTargets(team, "engineers")
	if err != nil || !reflect.DeepEqual(got, []string{"engineers"}) {
		t.Fatalf("got %v, %v; want only the agent", got, err)
	}
	team.Agents[0].ID = "all"
	if got, _ := sendTargets(team, "all"); !reflect.DeepEqual(got, []string{"all"}) {
		t.Fatalf("all collision: %v", got)
	}
}

func runSend(t *testing.T, args ...string) (string, error) {
	t.Helper()
	c := sendCmd()
	c.SetArgs(args)
	c.SilenceUsage, c.SilenceErrors = true, true
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	err := c.ExecuteContext(context.Background())
	w.Close()
	os.Stdout = old
	b := new(bytes.Buffer)
	b.ReadFrom(r)
	return b.String(), err
}

func TestSendCommandQueuesPerMember(t *testing.T) {
	project(t, true) // solo: orch + eng-1
	out, err := runSend(t, "all", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "queued for all: orch (message 1), eng-1 (message 2)") {
		t.Fatalf("out = %q", out)
	}
	out, err = runSend(t, "eng-1", "hi")
	if err != nil || out != "queued message 3 for eng-1\n" {
		t.Fatalf("single: %q, %v", out, err)
	}
	if _, err := runSend(t, "nobody", "x"); err == nil || !strings.Contains(err.Error(), "groups:") {
		t.Fatalf("unknown: %v", err)
	}
	dir, _ := os.Getwd()
	st, err := store.Open(filepath.Join(dir, ".slai", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	msgs, _ := st.RecentMessages(10)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3", len(msgs))
	}
}
