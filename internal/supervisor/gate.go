package supervisor

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/git"
	"github.com/oorbt/slai/internal/store"
)

// Merge-time test gate (review-loop-audit.md A). Two branches can each pass their own
// tests and still fail together. With project.test_command set, the merge is made in a
// scratch checkout, the command runs on the merge result, and only if it exits 0 is the
// integration branch fast-forwarded to it. Otherwise integration is never touched.

// GateError is a test_command that failed (or hung) on a merge result.
type GateError struct {
	Command  string
	ExitCode int // -1 when it did not exit normally
	TimedOut bool
	Timeout  time.Duration
	Tail     string // last lines of its output
}

func (e *GateError) Error() string {
	if e.TimedOut {
		return fmt.Sprintf("test command %q timed out after %s", e.Command, e.Timeout)
	}
	return fmt.Sprintf("test command %q failed (exit %d)", e.Command, e.ExitCode)
}

// GateTailLines is how much of the output the refusal notes carry.
const GateTailLines = 40

func (s *Supervisor) gateDir() string { return filepath.Join(s.Dir, ".slai", "worktrees", "_gate") }

func (s *Supervisor) testTimeout() time.Duration {
	if n := s.Team.Project.TestTimeoutSeconds; n > 0 {
		return time.Duration(n) * time.Second
	}
	return config.DefaultTestTimeoutSeconds * time.Second
}

// gatedMerge merges toMerge onto the integration branch only if test_command passes on
// the result. repo is the integration checkout. Errors: *git.ConflictError, *GateError,
// or anything else (transient; the caller retries).
func (s *Supervisor) gatedMerge(ctx context.Context, repo *git.Repo, toMerge, message string) error {
	integ := s.Team.Project.IntegrationBranch
	gate := s.gateDir()
	if err := s.Git.AddDetached(ctx, gate, integ); err != nil {
		return fmt.Errorf("scratch checkout for the test gate: %w", err)
	}
	defer func() { _ = s.Git.RemoveWorktree(ctx, gate) }()
	g := &git.Repo{Dir: gate}
	if err := g.MergeNoFF(ctx, toMerge, message); err != nil {
		return err
	}
	if gerr := s.runTestCommand(ctx, gate); gerr != nil {
		return gerr
	}
	sha, err := g.CurrentHead(ctx)
	if err != nil {
		return err
	}
	return repo.FastForward(ctx, sha)
}

// runTestCommand runs test_command in dir with the configured timeout. nil = exit 0.
func (s *Supervisor) runTestCommand(ctx context.Context, dir string) error {
	command := s.Team.Project.TestCommand
	timeout := s.testTimeout()
	var out tailBuffer
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("test command could not start: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if err == nil {
			return nil
		}
		code := -1
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		return &GateError{Command: command, ExitCode: code, Tail: out.Tail(GateTailLines)}
	case <-timer.C:
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // the shell and whatever it started
		<-done
		return &GateError{Command: command, ExitCode: -1, TimedOut: true, Timeout: timeout, Tail: out.Tail(GateTailLines)}
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return ctx.Err()
	}
}

// tailBuffer keeps the end of what is written to it (the last 64 KiB).
type tailBuffer struct{ b bytes.Buffer }

const tailMax = 64 << 10

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b.Write(p)
	if t.b.Len() > tailMax {
		rest := append([]byte(nil), t.b.Bytes()[t.b.Len()-tailMax:]...)
		t.b.Reset()
		t.b.Write(rest)
	}
	return len(p), nil
}

// Tail is the last n lines.
func (t *tailBuffer) Tail(n int) string {
	lines := strings.Split(strings.TrimRight(t.b.String(), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// refuseFailedGate handles a merge result whose tests failed: integration did not
// move, the task stays in review, and the engineer and the principals each get one
// note with the tail of the output. The approval event is consumed, so this happens
// once; the engineer fixes the branch and requests review again.
func (s *Supervisor) refuseFailedGate(task store.Task, ev store.Event, branch string, ge *GateError) {
	if task.Status != "in_review" {
		_ = s.Store.UpdateTaskStatus(task.ID, "in_review")
	}
	why := ge.Error()
	cause := "the tests failed on the merge result"
	if ge.TimedOut {
		cause = "the tests timed out on the merge result"
	}
	s.event(task.Assignee, "merge_refused", map[string]any{"task_id": task.ID, "branch": branch, "cause": cause,
		"command": ge.Command, "timed_out": ge.TimedOut, "exit": ge.ExitCode, "tail": ge.Tail})
	integ := s.Team.Project.IntegrationBranch
	body := func(forEngineer bool) string {
		next := fmt.Sprintf("expect a new review request from %s once they have fixed it", task.Assignee)
		if forEngineer {
			next = fmt.Sprintf("Merge %s into your branch in your worktree, make `%s` pass, commit, and call request_review again", integ, ge.Command)
		}
		return fmt.Sprintf("[%s] NOT merged: %s on the result of merging %s into %s, so %s was left unchanged.\nLast %d lines of output:\n%s\n%s.",
			task.ID, why, branch, integ, integ, GateTailLines, ge.Tail, next)
	}
	s.notify(task.Assignee, task.ID, body(true))
	seen := map[string]bool{task.Assignee: true}
	tell := func(to string) {
		if to == "" || seen[to] {
			return
		}
		seen[to] = true
		s.notify(to, task.ID, body(false))
	}
	tell(ev.Agent)
	for _, a := range s.Team.Agents {
		if a.Role == config.RolePrincipal {
			tell(a.ID)
		}
	}
}
