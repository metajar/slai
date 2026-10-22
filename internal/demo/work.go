package demo

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/oorbt/slai/internal/git"
	"github.com/oorbt/slai/internal/memory"
	"github.com/oorbt/slai/internal/ratelimit"
	"github.com/oorbt/slai/internal/store"
)

func ago(d time.Duration) time.Time { return Now.Add(-d) }

type tk struct {
	title, desc, assignee, spec, files string
	steps                              []string // statuses to walk through, in order
	deps                               []string
	prio                               int
}

// seedWork fills tasks, reviews, events, messages, spend, rate limits and memory.
func seedWork(ctx context.Context, p *Project, repo *git.Repo, setClock func(time.Time)) error {
	st := p.Store
	cur := Now
	set := func(t time.Time) { cur = t; setClock(t) }
	specs := map[string]string{
		"S-queue": "# Retry with backoff\n\nJobs that fail should be retried with a growing delay.\n\n## Acceptance criteria\n1. A failing job is retried up to the configured limit.\n2. The delay between tries doubles each time.\n3. A job that succeeds is not retried.\n",
		"S-sse":   "# Live updates\n\nThe browser should hear about changes without polling.\n\n## Acceptance criteria\n1. A change reaches the browser within one second.\n2. A burst of changes becomes one notice per topic.\n3. A slow client is dropped, not waited for.\n",
	}
	for n, b := range specs {
		if err := write(p.Dir, ".slai/specs/"+n+".md", b); err != nil {
			return err
		}
	}
	if err := branchWith(ctx, p.Dir, "slai/eng-4/T-0007", "internal/queue/retry.go", repoFiles["internal/queue/retry.go"]+"\n// RetryBackoff doubles the delay after every failure.\nfunc RetryBackoff(n int, f func() error) error { return Retry(n, f) }\n", "[T-0007] retry with backoff"); err != nil {
		return err
	}
	if err := branchWith(ctx, p.Dir, "slai/eng-1/T-0008", "internal/api/handlers.go", repoFiles["internal/api/handlers.go"]+"\nfunc Ready(w http.ResponseWriter, r *http.Request) { w.Write([]byte(\"ready\")) }\n", "[T-0008] add /ready"); err != nil {
		return err
	}
	if err := branchWith(ctx, p.Dir, "slai/eng-2/T-0013", "docs/architecture.md", repoFiles["docs/architecture.md"]+"\n## Workers\nWorkers poll the queue.\n", "[T-0013] document workers"); err != nil {
		return err
	}

	tasks := []tk{
		{"Health endpoint", "Add GET /health.", "eng-1", "", `["internal/api/handlers.go"]`, []string{"in_progress", "in_review", "merged"}, nil, 0},
		{"Job queue: push and pop", "The queue holds jobs.", "eng-2", "", `["internal/queue/queue.go"]`, []string{"in_progress", "in_review", "merged"}, nil, 0},
		{"Lane check for paths", "Keep tool calls inside the worktree.", "eng-3", "", `["internal/hooks/lane.go"]`, []string{"in_progress", "in_review", "merged"}, nil, 0},
		{"Rate limit the API", "Throttle clients.", "eng-1", "", `["internal/api/handlers.go"]`, []string{"in_progress"}, nil, 2},
		{"Worker pool sizing", "Choose the number of workers.", "eng-2", "", `["internal/queue/queue.go"]`, []string{"in_progress"}, nil, 1},
		{"Configurable retry limit", "Decide where the limit lives.", "eng-3", "", `["internal/queue/retry.go"]`, []string{"in_progress", "blocked"}, nil, 3},
		{"Retry with backoff", "Failed jobs wait longer each time.", "eng-4", ".slai/specs/S-queue.md", `["internal/queue/retry.go"]`, []string{"in_progress", "in_review"}, nil, 3},
		{"Readiness probe", "Add GET /ready.", "eng-1", ".slai/specs/S-sse.md", `["internal/api/handlers.go"]`, []string{"in_progress", "in_review"}, nil, 1},
		{"Metrics endpoint", "Expose counters.", "eng-2", "", `["internal/api/handlers.go"]`, []string{"in_progress", "blocked"}, []string{"T-0005"}, 0},
		{"Structured logging", "JSON logs.", "eng-3", "", `["cmd/server/main.go"]`, []string{}, nil, 0},
		{"Old kanban tweak", "Superseded.", "eng-4", "", "[]", []string{"cancelled"}, nil, 0},
		{"Experimental retry loop", "Tried, did not hold up.", "eng-2", "", `["internal/queue/retry.go"]`, []string{"in_progress", "in_review", "merged", "reverted"}, nil, 0},
		{"Document workers", "Explain the workers.", "eng-2", "", `["docs/architecture.md"]`, []string{"in_progress", "in_review", "changes_requested"}, nil, 0},
	}
	t0 := ago(5 * time.Hour)
	for i, k := range tasks {
		set(t0.Add(time.Duration(i) * 9 * time.Minute))
		created, err := st.CreateTask(store.Task{Title: k.title, Description: k.desc, Assignee: k.assignee, CreatedBy: "orch",
			SpecPath: k.spec, Files: k.files, Priority: k.prio, Branch: fmt.Sprintf("slai/%s/T-%04d", k.assignee, i+1)})
		if err != nil {
			return err
		}
		for _, s := range k.steps {
			set(cur.Add(11 * time.Minute))
			switch s {
			case "reverted":
				err = st.MarkReverted(created.ID)
			default:
				err = st.UpdateTaskStatus(created.ID, s)
			}
			if err != nil {
				return fmt.Errorf("%s -> %s: %w", created.ID, s, err)
			}
		}
		if len(k.deps) > 0 {
			if err := st.SetTaskDeps(created.ID, k.deps); err != nil {
				return err
			}
		}
	}
	// the tasks above are numbered T-0001..T-0013; the two reviews use the branches made above
	fix := map[string]string{"T-0007": "slai/eng-4/T-0007", "T-0008": "slai/eng-1/T-0008", "T-0013": "slai/eng-2/T-0013"}
	for id, br := range fix {
		if err := st.UpdateTaskFields(id, store.Task{Branch: br}); err != nil {
			return err
		}
	}

	// AC claims and verdicts
	set(ago(75 * time.Minute))
	if _, err := st.SetACClaims("T-0007", []store.ACClaim{{AC: "AC 1", Met: true, Note: "TestRetryUpToLimit"}, {AC: "AC 2", Met: true, Note: "TestDelayDoubles"}, {AC: "AC 3", Met: false, Note: "not done: success path still sleeps"}}); err != nil {
		return err
	}
	if _, err := st.SetACClaims("T-0008", []store.ACClaim{{AC: "AC 1", Met: true, Note: "TestNoticeWithin1s"}, {AC: "AC 2", Met: true, Note: "TestBurstCoalesced"}, {AC: "AC 3", Met: true, Note: "TestSlowClientDropped"}}); err != nil {
		return err
	}
	set(ago(40 * time.Minute))
	if err := st.SaveACVerdicts("T-0008", "principal", []store.ACVerdict{{AC: "AC 1", Pass: true, Note: "seen in the test"}, {AC: "AC 2", Pass: true}}); err != nil {
		return err
	}

	// events: hooks (liveness), health, lane denials, refusals, questions
	ev := func(t time.Time, agent, kind string, data any) {
		set(t)
		_ = st.AddEvent(agent, kind, j(data))
	}
	for _, a := range []struct {
		id   string
		last time.Duration
	}{{"orch", 40 * time.Second}, {"principal", 90 * time.Second}, {"eng-1", 30 * time.Second}, {"eng-2", 26 * time.Minute}, {"eng-3", 7 * time.Minute}, {"eng-4", 9 * time.Minute}, {"pm", 2 * time.Hour}, {"fg-newbie", 3 * time.Hour}, {"fg-skeptic", 4 * time.Hour}} {
		ev(ago(a.last), a.id, "hook:pretooluse", map[string]any{"tool": "Bash"})
	}
	ev(ago(3*time.Hour), "eng-2", "stuck_detected", map[string]any{"cause": "sat on a permission prompt for 6 minutes"})
	ev(ago(2*time.Hour+50*time.Minute), "eng-2", "stuck_recovered", map[string]any{"cause": "prompt answered by the user"})
	ev(ago(95*time.Minute), "eng-4", "wake_gave_up", map[string]any{"cause": "rate limit; woken at reset"})
	ev(ago(4*time.Hour), "fg-skeptic", "agent_stopped", map[string]any{"cause": "tmux session ended"})
	for i, path := range []string{"/etc/hosts", "../../other-project/go.mod", "/tmp/scratch.txt"} {
		ev(ago(time.Duration(150-i*20)*time.Minute), "eng-1", "lane_denied", map[string]any{"tool": "Write", "path": path})
	}
	ev(ago(55*time.Minute), "", "merge_refused", map[string]any{"task_id": "T-0013", "cause": "tests failed on the merge result", "command": "go test ./...", "tail": "--- FAIL: TestWorkersDocumented (0.00s)\n    docs_test.go:12: docs/architecture.md has no \"Workers\" section for the new queue\nFAIL\nFAIL\tacme-api/docs\t0.011s", "exit": 1})
	ev(ago(20*time.Minute), "eng-3", "ask_user", map[string]any{"question": "Should the retry limit be configurable per job type or one global setting?", "task_id": "T-0006"})
	if err := st.SetAgentStatus("eng-3", "waiting_on_user"); err != nil {
		return err
	}
	set(ago(20 * time.Minute))
	_, _ = st.RaiseEscalation(store.Escalation{Agent: "eng-4", Kind: "rate_limited", Text: "eng-4 hit the usage cap; waits for the reset at 15:10", Since: ago(95 * time.Minute).Unix(), User: false})
	_, _ = st.RaiseEscalation(store.Escalation{Agent: "eng-2", Kind: "stalled", Text: "no hook for 26 minutes while T-0005 is in progress", Since: ago(16 * time.Minute).Unix(), User: true})

	// statuses
	for id, status := range map[string]string{"orch": "working", "principal": "working", "eng-1": "working", "eng-2": "working", "eng-4": "rate_limited", "fg-skeptic": "dead"} {
		set(ago(8 * time.Minute))
		if id == "eng-2" {
			set(ago(26 * time.Minute)) // silent since: it reads stale, not busy
		}
		if err := st.SetAgentStatus(id, status); err != nil {
			return err
		}
	}

	// messages
	for i, m := range []store.Message{
		{From: "user", To: "orch", Kind: "note", Body: "Please add retries to the job queue, and keep the API small."},
		{From: "orch", To: "eng-4", Kind: "assignment", TaskID: "T-0007", Body: "Task T-0007 (Retry with backoff) was assigned to you."},
		{From: "principal", To: "eng-2", Kind: "review", TaskID: "T-0013", Body: "Changes requested on T-0013 (round 1): add the Workers section the test expects."},
		{From: "eng-3", To: "orch", Kind: "ask", Body: "Is the retry limit per job type?"},
	} {
		set(ago(time.Duration(300-i*40) * time.Minute))
		if _, err := st.AddMessage(m); err != nil {
			return err
		}
	}

	// spend: $91.45 today against a $120 cap; eng-1 at 85% of its $40 cap
	for _, u := range []struct {
		id   string
		cost float64
	}{{"orch", 18.90}, {"principal", 12.15}, {"eng-1", 34.00}, {"eng-2", 14.30}, {"eng-3", 8.10}, {"eng-4", 4.00}} {
		set(ago(3 * time.Hour))
		if err := st.AddUsage(u.id, 120000, 30000, u.cost); err != nil {
			return err
		}
	}
	set(Now)
	reset := ago(-40 * time.Minute).Unix()
	if err := ratelimit.Save(st, ratelimit.Snapshot{FiveHour: &ratelimit.Window{UsedPercent: 41, ResetsAt: reset}, Auth: "oauth", Agent: "orch", At: Now.Unix()}); err != nil {
		return err
	}
	_ = st.SetKV("mcp.build.eng-2", "older-build")

	return seedMemory(ctx, p, repo, setClock)
}

func seedMemory(ctx context.Context, p *Project, repo *git.Repo, set func(time.Time)) error {
	set(ago(4 * time.Hour))
	m := memory.New(filepath.Join(p.Dir, ".slai", "memory"), p.Store, repo, p.Team.Project.IntegrationBranch)
	m.Now = func() time.Time { return ago(4 * time.Hour) }
	put := func(text, kind, status, by string, paths ...string) (memory.Fact, error) {
		var ps []memory.Path
		for _, x := range paths {
			ps = append(ps, memory.Path{Path: x})
		}
		f, _, err := m.Put(ctx, memory.Fact{Kind: kind, Text: text, Author: "eng-1", Status: status, VerifiedBy: by, Paths: ps})
		return f, err
	}
	if _, err := put("Retry delays are computed in internal/queue/retry.go, not in the workers.", memory.KindArchitecture, memory.StatusVerified, "principal", "internal/queue/retry.go"); err != nil {
		return err
	}
	if _, err := put("The lane check compares path prefixes, so a symlink inside the worktree can leave it.", memory.KindGotcha, memory.StatusVerified, "principal", "internal/hooks/lane.go"); err != nil {
		return err
	}
	if _, err := put("go test ./... runs the whole suite in about two seconds.", memory.KindCommand, memory.StatusProposed, "", "README.md"); err != nil {
		return err
	}
	stale, err := put("The health handler writes the plain text ok and nothing else.", memory.KindConvention, memory.StatusVerified, "principal", "internal/store/store.go")
	if err != nil {
		return err
	}
	_ = stale
	// the file the stale fact cites changes on integration afterwards
	if err := run(ctx, p.Dir, gitEnv, "git", "checkout", "-q", "slai/integration"); err != nil {
		return err
	}
	if err := write(p.Dir, "internal/store/store.go", repoFiles["internal/store/store.go"]+"\n// Open opens the database.\nfunc Open(path string) *Store { return &Store{path: path} }\n"); err != nil {
		return err
	}
	for _, s := range [][]string{{"git", "add", "-A"}, {"git", "commit", "-q", "-m", "store: add Open"}, {"git", "checkout", "-q", "main"}} {
		if err := run(ctx, p.Dir, gitEnv, s...); err != nil {
			return err
		}
	}
	// a broken file someone edited by hand
	// (what a user does with an editor: the demo plays that part, slai's own writers never would)
	badPath := filepath.Join(m.Dir, "f-0badf00d.md")
	if err := os.WriteFile(badPath, []byte("---\nkind: [oops\n---\nhand edit that broke the frontmatter\n"), 0o644); err != nil {
		return err
	}
	m.Now = func() time.Time { return Now }
	return m.Reindex(ctx)
}
