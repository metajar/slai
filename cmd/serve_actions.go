package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/git"
	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/supervisor"
	"github.com/oorbt/slai/internal/web/board"
)

// webActions is what `slai serve` may do on the user's behalf: send a message (the
// supervisor's own SendUser), and the board's writes, each through the function the CLI,
// TUI or tools already call. It adds no capability of its own.
type webActions struct {
	*supervisor.Supervisor
	tasks board.ServiceActions
}

func newWebActions(s *supervisor.Supervisor, team func() *config.Team) *webActions {
	repo := &git.Repo{Dir: s.Dir}
	return &webActions{Supervisor: s, tasks: board.ServiceActions{
		Svc:      &service.Service{Store: s.Store, Git: repo, Team: s.Team, Dir: s.Dir},
		Priority: s.SetPriority,
		RequestRedirect: func(a store.Agent, text string) error {
			_, err := supervisor.RequestRedirect(s.Store, a, text)
			return err
		},
		Git:         repo,
		Integration: func() string { return team().Project.IntegrationBranch },
	}}
}

func (w *webActions) SetPriority(id, spec string) (int, error) { return w.tasks.SetPriority(id, spec) }
func (w *webActions) CancelTask(id, reason string) error       { return w.tasks.CancelTask(id, reason) }
func (w *webActions) UnblockTask(id, note string) error        { return w.tasks.UnblockTask(id, note) }
func (w *webActions) CreateTask(a board.CreateArgs) (string, error) {
	return w.tasks.CreateTask(a)
}
func (w *webActions) Redirect(a store.Agent, text string) error { return w.tasks.Redirect(a, text) }
func (w *webActions) TaskDiff(ctx context.Context, t store.Task) (*board.Diff, error) {
	return w.tasks.TaskDiff(ctx, t)
}

// CapturePane reads an agent's tmux pane (with colours when withANSI) for the agents page.
// It is read-only: no key ever goes to a pane from the web.
func (w *webActions) CapturePane(ctx context.Context, agentID string, withANSI bool) (string, error) {
	ag, err := w.Store.AgentByID(agentID)
	if err != nil {
		return "", err
	}
	if ag.TmuxSession == "" {
		return "", fmt.Errorf("%s has no terminal session", agentID)
	}
	return w.Tmux.CapturePane(ctx, ag.TmuxSession, withANSI, 200)
}

// CommitPreview returns exactly what `slai commit --dry-run` prints. A plan that cannot be
// applied yet (staged changes, blockers) still comes back as text, with the reason last.
func (a *webActions) CommitPreview(ctx context.Context) (string, error) {
	r, err := buildLandRun(a.Supervisor, "")
	if err != nil {
		return "", err
	}
	var b strings.Builder
	r.Dry, r.Out, r.In = true, &b, strings.NewReader("")
	if err := runLand(ctx, r); err != nil {
		if b.Len() == 0 {
			return "", err
		}
		return b.String() + "\n" + err.Error() + "\n", nil
	}
	return b.String(), nil
}

// RevertPreview returns what `slai revert <task> --dry-run` prints before its dry-run note.
func (a *webActions) RevertPreview(ctx context.Context, taskID string) (string, error) {
	plan, err := a.PlanRevert(ctx, taskID, false)
	if err != nil {
		return "", err
	}
	return revertPlanText(plan), nil
}
