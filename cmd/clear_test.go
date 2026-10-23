package cmd

import (
	"bytes"
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
)

func clearTeam() *config.Team {
	t := config.Preset("squad", "demo", "d", "main")
	t.Agents = []config.Agent{
		{ID: "orch", Role: config.RoleOrchestrator, Model: "opus"},
		{ID: "prin", Role: config.RolePrincipal, Model: "opus"},
		{ID: "eng-1", Role: config.RoleEngineer, Model: "sonnet"},
		{ID: "eng-2", Role: config.RoleEngineer, Model: "sonnet"},
	}
	return t
}

// A1: naming nobody is an error with usage, never "clear everyone".
func TestClearNeedsATarget(t *testing.T) {
	team := clearTeam()
	_, err := clearTargets(team, nil, "", false)
	if err == nil || !strings.Contains(err.Error(), "slai clear <agent>") {
		t.Fatalf("no-arg error = %v", err)
	}
	if ids, _ := clearTargets(team, []string{"eng-2", "orch", "eng-2"}, "", false); strings.Join(ids, ",") != "eng-2,orch" {
		t.Fatalf("named: %v", ids)
	}
	if ids, _ := clearTargets(team, nil, "engineer", false); strings.Join(ids, ",") != "eng-1,eng-2" {
		t.Fatalf("--role: %v", ids)
	}
	if ids, _ := clearTargets(team, nil, "", true); len(ids) != 4 {
		t.Fatalf("--all: %v", ids)
	}
	if _, err := clearTargets(team, nil, "ghost", false); err == nil || !strings.Contains(err.Error(), "engineer") {
		t.Fatalf("unknown role: %v", err)
	}
	if _, err := clearTargets(team, []string{"eng-1"}, "", true); err == nil {
		t.Fatal("--all with names accepted")
	}
	cmd := clearCmd()
	cmd.SetArgs(nil)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	project(t, true)
	if err := cmd.ExecuteContext(context.Background()); err == nil || !strings.Contains(err.Error(), "slai clear <agent>") {
		t.Fatalf("slai clear with no argument: %v", err)
	}
}

// A1: one line per agent: cleared, deferred, skipped (reason) or failed. The supervisor is
// played by the test: it writes the outcomes the engine would.
func TestRunClearReportsEachState(t *testing.T) {
	old := clearPoll
	clearPoll = 5 * time.Millisecond
	t.Cleanup(func() { clearPoll = old })
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	team := clearTeam()
	go func() { // the supervisor
		for i := 0; i < 400; i++ {
			time.Sleep(5 * time.Millisecond)
			for _, id := range []string{"eng-1", "eng-2", "prin"} {
				req, ok := service.LoadReset(st, id)
				if !ok {
					continue
				}
				switch id {
				case "eng-1":
					st.SetKV(service.KVResetRes+id, `{"req":`+strconv.FormatInt(req.At, 10)+`,"state":"cleared"}`)
					st.DeleteKV(service.KVResetReq + id)
				case "eng-2":
					req.Deferred = true
					service.SaveReset(st, id, req)
				case "prin":
					st.SetKV(service.KVResetRes+id, `{"req":`+strconv.FormatInt(req.At, 10)+`,"state":"skipped","detail":"it has unrecorded work"}`)
					st.DeleteKV(service.KVResetReq + id)
				}
			}
		}
	}()
	var out, prog bytes.Buffer
	failed := runClear(context.Background(), &out, &prog, st, team, []string{"eng-1", "eng-2", "prin", "orch"}, 5*time.Second)
	if failed != 0 {
		t.Fatalf("failed = %d", failed)
	}
	got := out.String()
	for _, want := range []string{"eng-1: cleared\n", "eng-2: deferred (busy; will clear when idle)\n", "prin: skipped (it has unrecorded work)\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}
