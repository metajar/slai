package board

import (
	"context"
	"errors"

	"github.com/oorbt/slai/internal/git"
	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
)

// ServiceActions implements Actions, Redirector and Differ over the same service, supervisor
// and git calls the CLI uses, acting as service.UserCaller. cmd wires the functions in; the
// web layer adds no logic of its own here.
type ServiceActions struct {
	Svc *service.Service
	// Priority is Supervisor.SetPriority: the function `slai priority` calls.
	Priority func(taskID, spec string) (int, error)
	// RequestRedirect is supervisor.RequestRedirect over the store: the TUI's `c` key path.
	RequestRedirect func(a store.Agent, text string) error
	Git             *git.Repo
	// Integration is the integration branch the diffs are against.
	Integration func() string
}

func (a ServiceActions) SetPriority(taskID, spec string) (int, error) {
	if a.Priority == nil {
		return 0, errNoActions
	}
	return a.Priority(taskID, spec)
}

func (a ServiceActions) CancelTask(taskID, reason string) error {
	if a.Svc == nil {
		return errNoActions
	}
	_, err := a.Svc.CancelTask(context.Background(), service.UserCaller, service.CancelTaskArgs{TaskID: taskID, Reason: reason})
	return err
}

func (a ServiceActions) UnblockTask(taskID, note string) error {
	if a.Svc == nil {
		return errNoActions
	}
	_, err := a.Svc.UpdateTask(context.Background(), service.UserCaller, service.UpdateTaskArgs{TaskID: taskID, Status: store.StatusInProgress, Description: note})
	return err
}

func (a ServiceActions) CreateTask(c CreateArgs) (string, error) {
	if a.Svc == nil {
		return "", errNoActions
	}
	res, err := a.Svc.CreateTask(context.Background(), service.UserCaller, service.CreateTaskArgs{
		Title: c.Title, Description: c.Description, Assignee: c.Assignee, SpecPath: c.SpecPath, Files: c.Files, DependsOn: c.DependsOn})
	if err != nil {
		return "", err
	}
	m, _ := res.(map[string]any)
	id, _ := m["task_id"].(string)
	return id, nil
}

func (a ServiceActions) Redirect(ag store.Agent, text string) error {
	if a.RequestRedirect == nil {
		return errNoActions
	}
	return a.RequestRedirect(ag, text)
}

// TaskDiff reads base...branch without pinning anything (unlike the principal's review_diff,
// which records the commit it showed).
func (a ServiceActions) TaskDiff(ctx context.Context, t store.Task) (*Diff, error) {
	if a.Git == nil || a.Integration == nil {
		return nil, errors.New("no repository")
	}
	if t.Branch == "" {
		return nil, errors.New("the task has no branch")
	}
	base := a.Integration()
	d, err := a.Git.Diff(ctx, base, t.Branch, service.MaxDiffLines)
	if err != nil {
		return nil, err
	}
	return &Diff{Patch: d.Patch, Files: d.Files, Truncated: d.Truncated, Base: base}, nil
}
