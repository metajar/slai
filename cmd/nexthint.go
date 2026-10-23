package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/supervisor"
)

const noTeamMessage = "no team yet in this directory. Next: `slai init` (or `slai`, which runs it)"

func hasTeam(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".slai", "team.yaml"))
	return err == nil
}

// nextStep is the one-line "what do I do now" for a configured team. Bare `slai` is
// the one way to start (decisions.md, Open question 5).
func nextStep(t *config.Team, supervisorUp bool, openTasks int) string {
	switch {
	case !supervisorUp:
		return "Next: `slai`"
	case openTasks == 0:
		return "Next: " + goalCommand(t)
	default:
		return "Next: `slai serve` or `b` in the dashboard"
	}
}

func goalCommand(t *config.Team) string {
	orch := "orch"
	for _, a := range t.Agents {
		if a.Role == config.RoleOrchestrator {
			orch = a.ID
		}
	}
	return fmt.Sprintf("`slai send %s \"<goal>\"`", orch)
}

// helpHint is the next-step line for `slai --help`, from what is on disk in dir. It
// reads state read-only and never creates files.
func helpHint(dir string) string {
	if !hasTeam(dir) {
		return "Next: `slai init`"
	}
	team, err := config.Load(filepath.Join(dir, ".slai", "team.yaml"))
	if err != nil {
		return "Next: `slai doctor`"
	}
	if _, up := supervisor.Running(dir); !up {
		return nextStep(team, false, 0)
	}
	open := 0
	dbPath := filepath.Join(dir, ".slai", "state.db")
	if _, err := os.Stat(dbPath); err == nil {
		if st, err := store.Open(dbPath); err == nil {
			open = countOpenTasks(st)
			st.Close()
		}
	}
	return nextStep(team, true, open)
}

func countOpenTasks(st *store.Store) int {
	tasks, err := st.ListTasks("")
	if err != nil {
		return 0
	}
	n := 0
	for _, t := range tasks {
		switch t.Status {
		case store.StatusMerged, store.StatusCancelled, store.StatusDone, store.StatusReverted:
		default:
			n++
		}
	}
	return n
}

func init() {
	// slaiNextStep is the state-dependent last line of the root help, evaluated when help renders.
	cobra.AddTemplateFunc("slaiNextStep", func() string {
		dir, _ := os.Getwd()
		return helpHint(dir)
	})
}
