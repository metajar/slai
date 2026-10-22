package security_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/memory"
	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/web/board"
	"github.com/oorbt/slai/internal/web/pages"
	"github.com/oorbt/slai/internal/web/server"
	"github.com/oorbt/slai/internal/web/webtest"
)

// xss returns a payload tagged n, so a test can tell which field leaked or was rendered.
func xss(n int) string { return fmt.Sprintf("<img src=x onerror=alert(%d)>", n) }

// Field numbers: one per user- or agent-controlled value the pages can render.
const (
	fAgentModel = 1 + iota
	fAgentFocus
	fTaskTitle
	fTaskDesc
	fTaskFiles
	fTaskSpec
	fMsgBody
	fMsgFrom
	fEventCause
	fEventPath
	fPane
	fFactText
	fAgentID
	fTaskAssignee
	fProject
	fPreview
)

var fieldNames = map[int]string{
	fAgentModel: "agent model", fAgentFocus: "agent focus", fTaskTitle: "task title", fTaskDesc: "task description",
	fTaskFiles: "task files", fTaskSpec: "task spec_path", fMsgBody: "message body", fMsgFrom: "message sender",
	fEventCause: "event cause", fEventPath: "lane-denial path", fPane: "pane text", fFactText: "fact text",
	fAgentID: "agent id", fTaskAssignee: "task assignee", fProject: "project name", fPreview: "commit/revert plan",
}

// fakeActions is the supervisor-side of the server for these tests: it records sends, serves
// a pane built from the T-0044 real fixtures plus a planted payload, and delegates the
// board's task writes to the real service.
type fakeActions struct {
	*board.ServiceActions
	pane   string
	sent   []string
	pauses []string
}

func (f *fakeActions) SendUser(to, body string) (int64, error) {
	f.sent = append(f.sent, to+": "+body)
	return int64(len(f.sent)), nil
}
func (f *fakeActions) CommitPreview(context.Context) (string, error) {
	return "plan " + xss(fPreview) + "\n", nil
}
func (f *fakeActions) RevertPreview(_ context.Context, id string) (string, error) {
	return "revert " + id + " " + xss(fPreview) + "\n", nil
}
func (f *fakeActions) CapturePane(context.Context, string, bool) (string, error) { return f.pane, nil }
func (f *fakeActions) PauseAgent(id string) error                                { f.pauses = append(f.pauses, id); return nil }
func (f *fakeActions) ResumeAgent(id string) error                               { f.pauses = append(f.pauses, "-"+id); return nil }

// realPane concatenates the T-0044 captured panes and adds the payload.
func realPane(t testing.TB) string {
	t.Helper()
	files, _ := filepath.Glob("../../hooks/testdata/panes/*/*.txt")
	if len(files) < 10 {
		t.Fatalf("T-0044 pane fixtures not found (%d)", len(files))
	}
	var b strings.Builder
	for _, f := range files {
		d, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(d)
		b.WriteString("\n")
	}
	b.WriteString(xss(fPane) + "\n\x1b]52;c;ZXZpbA==\x07\x1b[31m" + xss(fPane) + "\x1b[0m\n")
	return b.String()
}

// seeded is a harness over the full module set with payloads in every controlled field.
type seeded struct {
	*webtest.Harness
	Act     *fakeActions
	Agents  []string
	Tasks   []string
	Facts   []string
	Message []string
}

type seedOpt func(*[]webtest.Option)

func readOnly() seedOpt { return func(o *[]webtest.Option) { *o = append(*o, webtest.ReadOnly()) } }

func newSeeded(t *testing.T, extra ...webtest.Option) *seeded {
	t.Helper()
	s := &seeded{Act: &fakeActions{pane: realPane(t)}}
	setup := webtest.Setup(func(st *store.Store, team *config.Team) {
		team.Project.Name = "demo " + xss(fProject)
		weird := "x\"><b>" + xss(fAgentID)
		team.Agents = []config.Agent{
			{ID: "orch", Role: config.RoleOrchestrator, Model: "opus"},
			{ID: "eng-1", Role: config.RoleEngineer, Model: xss(fAgentModel), Focus: xss(fAgentFocus)},
			{ID: weird, Role: config.RoleEngineer, Model: "sonnet"},
			{ID: "fg-1", Role: config.RoleFocusGroup, Model: "haiku"},
		}
		for _, a := range team.Agents {
			_ = st.UpsertAgent(store.Agent{ID: a.ID, Role: a.Role, Model: a.Model, TmuxSession: "s-" + a.ID})
			s.Agents = append(s.Agents, a.ID)
		}
		files, _ := json.Marshal([]string{xss(fTaskFiles), "../../etc/passwd"})
		mk := func(status string) string {
			tk, err := st.CreateTask(store.Task{Title: "t " + xss(fTaskTitle), Description: "d " + xss(fTaskDesc), Assignee: "eng-1", Status: status,
				Files: string(files), SpecPath: "../../etc/passwd" + xss(fTaskSpec), CreatedBy: "orch"})
			if err != nil {
				t.Fatal(err)
			}
			return tk.ID
		}
		for _, status := range []string{store.StatusAssigned, store.StatusInProgress, store.StatusInReview, store.StatusMerged, store.StatusBlocked} {
			s.Tasks = append(s.Tasks, mk(status))
		}
		weirdTask, _ := st.CreateTask(store.Task{Title: "w", Assignee: "x" + xss(fTaskAssignee), Status: store.StatusAssigned})
		s.Tasks = append(s.Tasks, weirdTask.ID)
		_ = st.SetAgentTask("eng-1", s.Tasks[1])
		_ = st.SetAgentStatus("eng-1", "working")
		_ = st.SetAgentStatus("orch", "waiting_on_user")
		for _, m := range []store.Message{
			{From: "eng-1", To: "orch", Kind: "ask", TaskID: s.Tasks[1], Body: "q " + xss(fMsgBody)},
			{From: "user", To: "eng-1", Kind: "note", Body: "c " + xss(fMsgBody)},
			{From: "x" + xss(fMsgFrom), To: "orch", Kind: "note", Body: "from weird sender"},
		} {
			if id, err := st.AddMessage(m); err == nil {
				s.Message = append(s.Message, fmt.Sprint(id))
			}
		}
		_ = st.AddEvent("eng-1", "stuck_nudge", `{"cause":"`+jsonEsc(xss(fEventCause))+`"}`)
		_ = st.AddEvent("eng-1", "lane_denied", `{"tool":"Write","path":"`+jsonEsc(xss(fEventPath))+`","reason":"`+jsonEsc(xss(fEventCause))+`"}`)
		_ = st.AddEvent("", "merge_refused", `{"task_id":"`+s.Tasks[2]+`","output":"`+jsonEsc(xss(fEventCause))+`"}`)
	})
	opts := []webtest.Option{webtest.WithModules(pages.All()...), setup,
		webtest.SupervisorRunning(func() bool { return true }),
		webtest.WithActionsFunc(func(h *webtest.Harness) server.Actions {
			s.Act.ServiceActions = &board.ServiceActions{
				Svc:      &service.Service{Store: h.Store, Team: h.Team, Dir: h.Dir},
				Priority: func(id, spec string) (int, error) { return h.Store.ApplyPriority(id, spec) },
			}
			return s.Act
		})}
	s.Harness = webtest.New(t, append(opts, extra...)...)
	// a hand-written fact file, as a user or a rogue process could leave it
	f := memory.Fact{Kind: memory.KindGotcha, Text: "fact " + xss(fFactText), Status: memory.StatusVerified, Author: "x", VerifiedBy: "user",
		IntegrationSHA: "abc", Created: "2026-10-22"}
	f.ID = memory.ID(f.Kind, f.Text, nil)
	dir := filepath.Join(s.Dir, ".slai", "memory")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, f.ID+".md"), f.Marshal(), 0o644)
	s.Facts = append(s.Facts, f.ID)
	// team.yaml for the New Agent form
	_ = os.MkdirAll(filepath.Join(s.Dir, ".slai"), 0o755)
	_ = config.Write(filepath.Join(s.Dir, ".slai", "team.yaml"), cloneTeam(s.Team))
	return s
}

func cloneTeam(t *config.Team) *config.Team {
	c := *t
	c.Agents = []config.Agent{{ID: "orch", Role: config.RoleOrchestrator, Model: "opus"}, {ID: "eng-1", Role: config.RoleEngineer, Model: "sonnet"}}
	c.ApplyDefaults()
	return &c
}

func jsonEsc(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

// candidates are the values tried for each {name} in a route pattern.
func (s *seeded) candidates() []string {
	var c []string
	c = append(c, s.Agents...)
	c = append(c, s.Tasks...)
	c = append(c, s.Facts...)
	c = append(c, s.Message...)
	c = append(c, "x", "../etc/passwd", "%2e%2e", "T-9999")
	return c
}

var paramRe = regexp.MustCompile(`\{[^}]*\}`)

// paths expands a route pattern into request paths: {$} is dropped and every other
// parameter takes each candidate in turn (all parameters of one request share the value).
func paths(pattern string, cands []string) []string {
	if !paramRe.MatchString(pattern) || pattern == "/{$}" {
		return []string{strings.ReplaceAll(pattern, "{$}", "")}
	}
	var out []string
	for _, c := range cands {
		out = append(out, paramRe.ReplaceAllString(pattern, strings.ReplaceAll(url.PathEscape(c), "%", "%")))
	}
	return out
}

// snapshot is everything a refused request must leave alone.
type snapshot struct {
	events, messages int
	tasks            string
	agents           string
	team             string
}

func (s *seeded) snap() snapshot {
	evs, _ := s.Store.RecentEvents(1_000_000)
	msgs, _ := s.Store.RecentMessages(1_000_000)
	ts, _ := s.Store.ListTasks("")
	as, _ := s.Store.GetAgents()
	tb, _ := json.Marshal(ts)
	ab, _ := json.Marshal(as)
	team, _ := os.ReadFile(filepath.Join(s.Dir, ".slai", "team.yaml"))
	return snapshot{len(evs), len(msgs), string(tb), string(ab), string(team)}
}

// diff names what differs between two snapshots (the values are too long to print).
func (a snapshot) diff(b snapshot) string {
	var d []string
	if a.events != b.events {
		d = append(d, fmt.Sprintf("events %d -> %d", a.events, b.events))
	}
	if a.messages != b.messages {
		d = append(d, fmt.Sprintf("messages %d -> %d", a.messages, b.messages))
	}
	if a.tasks != b.tasks {
		d = append(d, "tasks changed")
	}
	if a.agents != b.agents {
		d = append(d, "agents changed")
	}
	if a.team != b.team {
		d = append(d, "team.yaml changed")
	}
	return strings.Join(d, "; ")
}

// withNonLoopback is the harness option set for a non-loopback bind.
func withNonLoopback() []webtest.Option { return []webtest.Option{webtest.Listen(webtest.NonLoopback)} }
