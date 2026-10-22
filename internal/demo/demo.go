// Package demo builds a reproducible fake project for `slai serve --demo`: a temporary git
// repository, a team.yaml and a state.db filled with agents in every health state, tasks in
// every column, reviews with AC claims and verdicts, questions, spend, rate limits, memory
// facts and canned panes. Every timestamp comes from a fixed clock and every id from a fixed
// order, so two runs produce the same pages. It never touches a real project: Seed refuses
// any directory that is not empty (or a previous demo).
package demo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/oorbt/slai/internal/buildinfo"
	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/git"
	"github.com/oorbt/slai/internal/store"
)

// Now is the demo's "current time": every page is rendered at this instant.
var Now = time.Date(2026, 10, 22, 14, 30, 0, 0, time.UTC)

// marker is the file that says a directory is a demo project (and may be re-seeded).
const marker = ".slai/DEMO"

// DEMOMarker is the marker file name under .slai/ (tests look for it).
const DEMOMarker = "DEMO"

// ErrNotDemo is returned for a directory that is not empty and not a demo project.
var ErrNotDemo = errors.New("demo: refusing to seed a directory that is not empty: it may be a real project (use a new, empty directory)")

// Options selects a variant of the demo state.
type Options struct {
	// FirstRun seeds the team only: no tasks, no history, nothing to answer.
	FirstRun bool
}

// Project is a seeded demo.
type Project struct {
	Dir   string
	Store *store.Store
	Team  *config.Team
	Opts  Options
}

// Check refuses dir unless it is missing, empty, or a demo project left by a previous run.
func Check(dir string) error {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(ents) == 0 {
		return nil
	}
	if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
		return nil
	}
	return ErrNotDemo
}

// Seed builds the demo project in dir (see Check) and returns it open. The caller closes
// Store.
func Seed(ctx context.Context, dir string, opts Options) (*Project, error) {
	if err := Check(dir); err != nil {
		return nil, err
	}
	if err := os.RemoveAll(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, ".slai", "specs"), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, marker), []byte("demo project: safe to delete and re-seed\n"), 0o644); err != nil {
		return nil, err
	}
	team := buildTeam()
	if err := config.Write(filepath.Join(dir, ".slai", "team.yaml"), team); err != nil {
		return nil, err
	}
	repo := &git.Repo{Dir: dir}
	if err := seedRepo(ctx, dir); err != nil {
		return nil, err
	}
	st, err := store.Open(filepath.Join(dir, ".slai", "state.db"))
	if err != nil {
		return nil, err
	}
	at := Now.Add(-6 * time.Hour)
	st.SetClock(func() time.Time { return at })
	set := func(t time.Time) { at = t }
	p := &Project{Dir: dir, Store: st, Team: team, Opts: opts}
	if err := seedAgents(st, team, set); err != nil {
		return nil, err
	}
	if !opts.FirstRun {
		if err := seedWork(ctx, p, repo, set); err != nil {
			return nil, err
		}
	}
	set(Now)
	_ = st.SetSupervisorHeartbeat(Now.Unix())
	_ = st.SetKV("supervisor.build", buildinfo.ID()) // the supervisor runs this build: no old-build banner
	st.SetClock(func() time.Time { return Now })
	return p, nil
}

func buildTeam() *config.Team {
	t := config.Preset("squad", "acme-api", "A small REST API with a worker queue", "main")
	t.Defaults.Caps = config.SpendCaps{DailyUSD: 120, PerAgentUSD: 40}
	t.Agents = []config.Agent{
		{ID: "orch", Role: config.RoleOrchestrator, Model: "opus", PermissionMode: config.DefaultWorkingPermissionMode},
		{ID: "pm", Role: config.RolePM, Model: "sonnet", PermissionMode: config.DefaultWorkingPermissionMode},
		{ID: "principal", Role: config.RolePrincipal, Model: "fable", PermissionMode: config.DefaultWorkingPermissionMode},
		{ID: "eng-1", Role: config.RoleEngineer, Model: "sonnet", Focus: "backend", PermissionMode: config.DefaultWorkingPermissionMode},
		{ID: "eng-2", Role: config.RoleEngineer, Model: "sonnet", Focus: "frontend", PermissionMode: config.DefaultWorkingPermissionMode},
		{ID: "eng-3", Role: config.RoleEngineer, Model: "sonnet", Focus: "tests", PermissionMode: config.DefaultWorkingPermissionMode},
		{ID: "eng-4", Role: config.RoleEngineer, Model: "sonnet", Focus: "docs", PermissionMode: config.DefaultWorkingPermissionMode},
		{ID: "fg-newbie", Role: config.RoleFocusGroup, Model: "sonnet", Focus: "a first-time user"},
		{ID: "fg-skeptic", Role: config.RoleFocusGroup, Model: "sonnet", Focus: "a security-minded reviewer"},
	}
	return t
}

func run(ctx context.Context, dir string, env []string, args ...string) error {
	c := exec.CommandContext(ctx, args[0], args[1:]...)
	c.Dir = dir
	c.Env = append(os.Environ(), env...)
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

var gitEnv = []string{"GIT_AUTHOR_NAME=slai demo", "GIT_AUTHOR_EMAIL=demo@example.invalid", "GIT_COMMITTER_NAME=slai demo",
	"GIT_COMMITTER_EMAIL=demo@example.invalid", "GIT_AUTHOR_DATE=2026-10-22T08:00:00Z", "GIT_COMMITTER_DATE=2026-10-22T08:00:00Z",
	"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null"}

func write(dir, name, body string) error {
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(body), 0o644)
}

var repoFiles = map[string]string{
	"README.md":                "# acme-api\n\nA small REST API with a worker queue.\n",
	"cmd/server/main.go":       "package main\n\nfunc main() {\n\tserve(\":8080\")\n}\n",
	"internal/queue/queue.go":  "package queue\n\n// Queue holds jobs until a worker takes them.\ntype Queue struct{ jobs []string }\n\nfunc (q *Queue) Push(j string) { q.jobs = append(q.jobs, j) }\n",
	"internal/queue/retry.go":  "package queue\n\n// Retry runs f up to n times.\nfunc Retry(n int, f func() error) error {\n\tvar err error\n\tfor i := 0; i < n; i++ {\n\t\tif err = f(); err == nil {\n\t\t\treturn nil\n\t\t}\n\t}\n\treturn err\n}\n",
	"internal/api/handlers.go": "package api\n\nimport \"net/http\"\n\nfunc Health(w http.ResponseWriter, r *http.Request) { w.Write([]byte(\"ok\")) }\n",
	"internal/store/store.go":  "package store\n\n// Store is the job database.\ntype Store struct{ path string }\n",
	"internal/hooks/lane.go":   "package hooks\n\n// Lane decides whether a tool call stays inside its worktree.\nfunc Lane(path, root string) bool { return len(path) >= len(root) && path[:len(root)] == root }\n",
	"docs/architecture.md":     "# Architecture\n\nThe server accepts jobs, the queue holds them, workers run them.\n",
}

func seedRepo(ctx context.Context, dir string) error {
	steps := [][]string{{"git", "init", "-q", "-b", "main"}}
	for _, s := range steps {
		if err := run(ctx, dir, gitEnv, s...); err != nil {
			return err
		}
	}
	for n, b := range repoFiles {
		if err := write(dir, n, b); err != nil {
			return err
		}
	}
	if err := write(dir, ".gitignore", ".slai/\n"); err != nil {
		return err
	}
	for _, s := range [][]string{{"git", "add", "-A"}, {"git", "commit", "-q", "-m", "initial import"}, {"git", "branch", "slai/integration"}} {
		if err := run(ctx, dir, gitEnv, s...); err != nil {
			return err
		}
	}
	return nil
}

// branchWith creates branch from integration with one commit that changes file.
func branchWith(ctx context.Context, dir, branch, file, body, msg string) error {
	for _, s := range [][]string{{"git", "checkout", "-q", "-b", branch, "slai/integration"}} {
		if err := run(ctx, dir, gitEnv, s...); err != nil {
			return err
		}
	}
	if err := write(dir, file, body); err != nil {
		return err
	}
	for _, s := range [][]string{{"git", "add", "-A"}, {"git", "commit", "-q", "-m", msg}, {"git", "checkout", "-q", "main"}} {
		if err := run(ctx, dir, gitEnv, s...); err != nil {
			return err
		}
	}
	return nil
}

func seedAgents(st *store.Store, team *config.Team, set func(time.Time)) error {
	set(Now.Add(-6 * time.Hour))
	for _, a := range team.Agents {
		if err := st.UpsertAgent(store.Agent{ID: a.ID, Role: a.Role, Model: a.Model, Status: "idle", TmuxSession: "demo_" + a.ID}); err != nil {
			return err
		}
	}
	return nil
}

// Pane returns the canned terminal capture (with colours) for an agent.
func Pane(agent string) string {
	body, ok := panes[agent]
	if !ok {
		body = panes["default"]
	}
	return body
}

var panes = map[string]string{
	"eng-1": "\x1b[1;32m●\x1b[0m Reading \x1b[36minternal/queue/retry.go\x1b[0m\n\n" +
		"\x1b[1;32m●\x1b[0m Update(internal/queue/retry.go)\n  \x1b[32m+ func RetryBackoff(n int, base time.Duration, f func() error) error {\x1b[0m\n  \x1b[31m- func Retry(n int, f func() error) error {\x1b[0m\n\n" +
		"\x1b[1;32m●\x1b[0m Bash(go test ./internal/queue/...)\n  ⎿  \x1b[32mok\x1b[0m  acme-api/internal/queue  0.412s\n\n" +
		"\x1b[33m✻ Refactoring…\x1b[0m (1m 12s · ↓ 3.1k tokens)\n",
	"eng-3": "\x1b[1;33m▲\x1b[0m Waiting on you\n\n" +
		"  Should the retry limit be configurable per job type or one global setting?\n" +
		"  <script>alert('pane text stays text')</script>\n" +
		"  api_key=sk-demo-0000000000000000000000000000\n",
	"default": "\x1b[2m(idle)\x1b[0m\n❯ \n",
}

// Text of the canned previews and doctor report.
const (
	CommitPlan = "slai commit (dry run)\n\nBranch: main (clean)\nPlan: merge slai/integration into main, 3 commits ahead\n\n  internal/queue/retry.go | 12 ++++++++++++\n  internal/api/handlers.go |  4 ++--\n  2 files changed, 14 insertions(+), 2 deletions(-)\n\nConflict check: trial merge-tree against main, no working-tree changes. No conflicts.\n"
	RevertPlan = "slai revert (dry run)\n\nWould revert the merge of the task onto slai/integration with `git revert -m 1`.\nFiles touched: internal/queue/retry.go\n"
	DoctorText = "slai doctor\n\n  ok    tmux available (3.4)\n  ok    git available (2.43)\n  ok    team.yaml valid\n  warn  build: 1 agent runs an older build\n  ok    hooks arriving for 6 of 6 live agents\n  warn  orphans: 1 orphaned team (slai_ab12_)\n\n2 warnings: build, orphans\n"
)

func j(v any) string { b, _ := json.Marshal(v); return string(b) }
