package board

import (
	"context"
	"errors"

	"github.com/oorbt/slai/internal/store"
)

// CreateArgs is the create-task form: the same fields as the create_task tool.
type CreateArgs struct {
	Title, Description, Assignee, SpecPath string
	DependsOn, Files                       []string
}

// Actions is what the board writes through. Each method is the function the CLI, TUI or
// service already offers (the web layer adds none of its own): SetPriority is the one
// `slai priority` calls, the others are the orchestrator's task tools called as the user.
type Actions interface {
	SetPriority(taskID, spec string) (int, error)
	CancelTask(taskID, reason string) error
	UnblockTask(taskID, note string) error
	CreateTask(a CreateArgs) (string, error)
}

// Redirector interrupts an agent's turn and delivers a course correction, the way the
// TUI's `c` key does (the supervisor re-checks the pane before pressing a key).
type Redirector interface {
	Redirect(a store.Agent, text string) error
}

// Diff is a task branch's changes against the integration branch.
type Diff struct {
	Patch     string
	Files     []string
	Truncated bool
	Base      string // what it is against, e.g. slai/integration
}

// Differ reads a task branch's diff without side effects (it must not pin a review).
type Differ interface {
	TaskDiff(ctx context.Context, t store.Task) (*Diff, error)
}

var errNoActions = errors.New("this server has no connection to the task service, so it cannot change tasks")
