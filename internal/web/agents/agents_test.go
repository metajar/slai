package agents_test

import (
	"context"
	"errors"
	"fmt"
	"github.com/oorbt/slai/internal/ratelimit"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/supervisor"
	"github.com/oorbt/slai/internal/web/agents"
	"github.com/oorbt/slai/internal/web/webtest"
)

// the harness clock
var now = time.Unix(1_800_000_000, 0)

// fakeActions records SendUser and serves a canned pane, like the supervisor-backed actions.
type fakeActions struct {
	sent     []string
	pane     map[string]string // agent -> capture
	paneErr  error
	plain    map[string]string // capture without colours (modal check)
	paused   []string
	resumed  []string
	captures int
}

// The agents page does not use the previews; they exist to satisfy server.Actions.
func (f *fakeActions) CommitPreview(context.Context) (string, error)         { return "", nil }
func (f *fakeActions) RevertPreview(context.Context, string) (string, error) { return "", nil }

func (f *fakeActions) SendUser(to, body string) (int64, error) {
	f.sent = append(f.sent, to+": "+body)
	return int64(len(f.sent)), nil
}

func (f *fakeActions) CapturePane(_ context.Context, id string, ansi bool) (string, error) {
	f.captures++
	if f.paneErr != nil {
		return "", f.paneErr
	}
	if !ansi && f.plain != nil {
		return f.plain[id], nil
	}
	return f.pane[id], nil
}

func (f *fakeActions) PauseAgent(id string) error  { f.paused = append(f.paused, id); return nil }
func (f *fakeActions) ResumeAgent(id string) error { f.resumed = append(f.resumed, id); return nil }

// hx posts the way the page's own htmx does (so OK/Fail answer with fragments, not redirects).
func hx(h *webtest.Harness, path string, form url.Values) *webtest.Resp {
	req := h.Browser("POST", path, form)
	req.Header.Set("HX-Request", "true")
	return h.Do(req)
}

func at(st *store.Store, d time.Duration) { st.SetClock(func() time.Time { return now.Add(-d) }) }

func seed(t *testing.T, fa *fakeActions, extra ...webtest.Option) *webtest.Harness {
	t.Helper()
	opts := []webtest.Option{webtest.WithModules(agents.Module), webtest.WithActions(fa),
		webtest.SupervisorRunning(func() bool { return true }),
		webtest.Setup(func(st *store.Store, team *config.Team) {
			at(st, 0)
			team.Agents = []config.Agent{
				{ID: "orch", Role: config.RoleOrchestrator, Model: "opus"},
				{ID: "eng-1", Role: config.RoleEngineer, Model: "sonnet"},
				{ID: "eng-2", Role: config.RoleEngineer, Model: "glm", Harness: config.HarnessPi},
				{ID: "fg", Role: config.RoleFocusGroup, Model: "haiku"},
			}
			for _, a := range team.Agents {
				_ = st.UpsertAgent(store.Agent{ID: a.ID, Role: a.Role, Model: a.Model, TmuxSession: "s-" + a.ID})
			}
			tk, _ := st.CreateTask(store.Task{Title: "Build the thing", Assignee: "eng-1", Status: store.StatusInProgress})
			_ = st.SetAgentTask("eng-1", tk.ID)
			_ = st.SetAgentStatus("eng-1", "working")
			_ = st.SetAgentStatus("orch", "waiting_on_user")
			_ = st.SetAgentStatus("fg", "dead")
		})}
	h := webtest.New(t, append(opts, extra...)...)
	h.Login()
	return h
}

// P3 AC1: the list shows what the TUI rows show for the same store state.
func TestListMirrorsStoreRows(t *testing.T) {
	h := seed(t, &fakeActions{})
	rows, err := agents.BuildRows(h.Store, h.Team, now)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]agents.Row{}
	var order []string
	for _, r := range rows {
		got[r.ID] = r
		order = append(order, r.ID)
	}
	if strings.Join(order, ",") != "orch,eng-1,eng-2,fg" {
		t.Errorf("order = %v (orchestrator, engineers, focus group)", order)
	}
	if r := got["orch"]; r.StatusLabel != "needs you" || !r.Waiting || r.Model != "opus" {
		t.Errorf("orch row %+v", r)
	}
	if r := got["eng-1"]; r.Task == "" || r.TaskTitle != "Build the thing" || r.Status != "working" || r.Gauges == "" || strings.HasPrefix(r.Gauges, "n/a") {
		t.Errorf("eng-1 row %+v", r)
	}
	if r := got["eng-2"]; r.Tag != "pi" || r.Harness != "pi" || !strings.HasPrefix(r.Gauges, "n/a — not supported by pi") {
		t.Errorf("pi row %+v", r)
	}
	if r := got["fg"]; r.StatusLabel != "dead" || r.Liveness != "dead" {
		t.Errorf("fg row %+v", r)
	}
	page := h.Get("/agents")
	h.CheckPage(page)
	for _, want := range []string{"Orchestrator", "Engineers", "Focus group", "needs you", "Build the thing", "n/a — not supported by pi", "dead", ">eng-1<", "Harness"} {
		if !strings.Contains(page.Body, want) {
			t.Errorf("list lacks %q", want)
		}
	}
	if !strings.Contains(page.Body, `href="/agents/new"`) {
		t.Error("no New agent link")
	}
}

func TestListEmptyState(t *testing.T) {
	h := webtest.New(t, webtest.WithModules(agents.Module), webtest.Setup(func(st *store.Store, team *config.Team) { team.Agents = nil }))
	h.Login()
	r := h.Get("/agents")
	h.CheckPage(r)
	if !strings.Contains(r.Body, "No agents yet") || !strings.Contains(r.Body, "slai up") {
		t.Errorf("empty state: %.300s", r.Body)
	}
}

// P3 AC2: the detail shows the last 5 health events with their cause, and the lane-denial
// count with the newest 3 denied tool/path.
func TestDetailHealthEventsAndLaneDenials(t *testing.T) {
	h := seed(t, &fakeActions{})
	st := h.Store
	for i := 1; i <= 4; i++ {
		at(st, time.Duration(50-i)*time.Minute)
		_ = st.AddEvent("eng-1", "lane_denied", fmt.Sprintf(`{"tool":"Write","path":"/etc/file%d","reason":"outside lane"}`, i))
	}
	for i := 1; i <= 7; i++ {
		at(st, time.Duration(100-i)*time.Minute)
		_ = st.AddEvent("eng-1", "stuck_nudge", fmt.Sprintf(`{"cause":"nothing happened for %d minutes"}`, i))
	}
	at(st, 0)
	r := h.Get("/agents/eng-1")
	h.CheckPage(r)
	for i := 3; i <= 7; i++ { // newest five of the seven stuck_nudge events
		if !strings.Contains(r.Body, fmt.Sprintf("nothing happened for %d minutes", i)) {
			t.Errorf("health event %d missing", i)
		}
	}
	if strings.Contains(r.Body, "nothing happened for 1 minutes") {
		t.Error("more than five health events listed")
	}
	if !strings.Contains(r.Body, "4 in the last 24h") {
		t.Errorf("denial count missing: %.0s", r.Body)
	}
	for _, p := range []string{"/etc/file4", "/etc/file3", "/etc/file2"} {
		if !strings.Contains(r.Body, p) {
			t.Errorf("denial %s missing", p)
		}
	}
	if strings.Contains(r.Body, "/etc/file1") {
		t.Error("more than three denials listed")
	}
	if r := h.Get("/agents/nobody"); r.Code != 404 {
		t.Errorf("unknown agent: %d", r.Code)
	}
}

var paneFixture = "\x1b[1;31mFAIL\x1b[0m test \x1b[32mok\x1b[39m\n" +
	"\x1b]52;c;ZXZpbA==\x07copied\n\x1b[2J\x1b[Hafter clear\n" +
	"<script>alert('x')</script>\n" +
	"ANTHROPIC_API_KEY=sk-ant-api03-AbCdEfGhIjKlMnOpQrStUv\n"

// P3 AC3: the pane fragment keeps only whitelisted spans and escaped text, hides a fake key,
// and the detail page carries the "may contain secrets" notice.
func TestPaneFragment(t *testing.T) {
	fa := &fakeActions{pane: map[string]string{"eng-1": paneFixture}}
	h := seed(t, fa)
	r := h.Get("/agents/eng-1/pane")
	h.CheckHeaders(r)
	if r.Code != 200 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	for _, want := range []string{`<span class="p-b p-fg1">FAIL</span>`, `<span class="p-fg2">ok</span>`, "copied", "after clear", "&lt;script&gt;alert(&#39;x&#39;)&lt;/script&gt;", "[redacted]"} {
		if !strings.Contains(r.Body, want) {
			t.Errorf("fragment lacks %q:\n%s", want, r.Body)
		}
	}
	for _, bad := range []string{"\x1b", "<script", "ZXZpbA", "sk-ant", "AbCdEf"} {
		if strings.Contains(r.Body, bad) {
			t.Errorf("fragment contains %q", bad)
		}
	}
	d := h.Get("/agents/eng-1")
	h.CheckPage(d)
	if !strings.Contains(d.Body, "May contain secrets") || !strings.Contains(d.Body, "best-effort") {
		t.Error("no secrets notice on the detail page")
	}
	if n := strings.Count(d.Body, `src="/static/slai.js"`); n != 1 {
		t.Errorf("the detail page loads slai.js %d times: without it the live pane never updates", n)
	}
	if !strings.Contains(d.Body, `data-live="pane:eng-1"`) || !strings.Contains(d.Body, `data-live-src="/agents/eng-1/pane"`) || strings.Contains(d.Body, "hx-trigger") {
		t.Error("detail's pane is not a live region on pane:eng-1 (or still polls)")
	}
}

// P3 AC3/4 (addendum 7): 500 lines / 200 KB come back as <= 200 lines and 64 KB.
func TestPaneFragmentIsCapped(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&b, "\x1b[33m%s\x1b[0m n=%d\n", strings.Repeat("y", 400), i)
	}
	h := seed(t, &fakeActions{pane: map[string]string{"eng-1": b.String()}})
	r := h.Get("/agents/eng-1/pane")
	if len(r.Body) > 64<<10 {
		t.Errorf("fragment is %d bytes", len(r.Body))
	}
	if n := strings.Count(r.Body, "\n"); n > 205 {
		t.Errorf("%d lines", n)
	}
	if !strings.Contains(r.Body, "n=499") || strings.Contains(r.Body, "n=0\n") {
		t.Error("should keep the newest lines")
	}
	if !strings.Contains(r.Body, "Older output was cut") {
		t.Error("no truncation note")
	}
}

func TestPaneUnavailableStates(t *testing.T) {
	h := seed(t, &fakeActions{paneErr: errors.New("no server running")})
	if r := h.Get("/agents/eng-1/pane"); !strings.Contains(r.Body, "Could not read the pane: no server running") {
		t.Errorf("%s", r.Body)
	}
	if r := h.Get("/agents/fg/pane"); !strings.Contains(r.Body, "fg is not running") {
		t.Errorf("dead agent: %s", r.Body)
	}
	plain := webtest.New(t, webtest.WithModules(agents.Module)) // fake Actions without a pane source
	plain.Login()
	_ = plain.Store.UpsertAgent(store.Agent{ID: "eng-1", Role: "engineer"})
	_ = plain.Store.SetAgentStatus("eng-1", "working")
	if r := plain.Get("/agents/eng-1/pane"); !strings.Contains(r.Body, "no terminal connection") {
		t.Errorf("%s", r.Body)
	}
}

// P3 AC5: send, redirect (with the TUI's refusals), pause and resume call the same functions
// as the TUI, record one web_action each, and a refused redirect queues nothing.
func TestAgentActions(t *testing.T) {
	fa := &fakeActions{pane: map[string]string{}}
	h := seed(t, fa)
	post := func(path string, body string) *webtest.Resp {
		return hx(h, path, url.Values{"body": {body}})
	}

	if r := post("/agents/eng-1/send", "hello"); r.Code != 303 && r.Code != 200 {
		t.Fatalf("send: %d %s", r.Code, r.Body)
	}
	if len(fa.sent) != 1 || fa.sent[0] != "eng-1: hello" {
		t.Errorf("sent = %v", fa.sent)
	}
	if r := hx(h, "/agents/eng-1/send", url.Values{"body": {""}}); r.Code != 422 || !strings.Contains(r.Body, "message is empty") {
		t.Errorf("empty send: %d %s", r.Code, r.Body)
	}
	if r := post("/agents/eng-1/send", strings.Repeat("x", 4001)); r.Code != 422 {
		t.Errorf("over-long send accepted: %d", r.Code)
	}

	// redirect a busy agent: the correction is queued and the supervisor is asked to interrupt
	if r := post("/agents/eng-1/redirect", "use the other approach"); r.Code != 303 && r.Code != 200 {
		t.Fatalf("redirect: %d %s", r.Code, r.Body)
	}
	if v, ok, _ := h.Store.GetKV(supervisor.KVRedirectPrefix + "eng-1"); !ok || v == "" {
		t.Error("no redirect request for the supervisor")
	}
	msgs, _ := h.Store.RecentMessages(10)
	found := false
	for _, m := range msgs {
		found = found || (m.To == "eng-1" && strings.HasSuffix(m.Body, "use the other approach") && strings.HasPrefix(m.Body, supervisor.CoursePrefix))
	}
	if !found {
		t.Errorf("course correction not queued: %+v", msgs)
	}

	// refusals: nothing is queued
	refuse := func(name, path, want string, setup func()) {
		t.Run(name, func(t *testing.T) {
			_ = h.Store.DeleteKV(supervisor.KVRedirectPrefix + "eng-1")
			before, _ := h.Store.RecentMessages(100)
			if setup != nil {
				setup()
			}
			r := hx(h, path, url.Values{"body": {"redirect me"}})
			if r.Code != 422 || !strings.Contains(r.Body, want) {
				t.Fatalf("%d %s (want %q)", r.Code, r.Body, want)
			}
			after, _ := h.Store.RecentMessages(100)
			if len(after) != len(before) {
				t.Error("a refused redirect queued a message")
			}
			if v, _, _ := h.Store.GetKV(supervisor.KVRedirectPrefix + "eng-1"); v != "" {
				t.Error("a refused redirect asked the supervisor to interrupt")
			}
		})
	}
	refuse("not busy", "/agents/orch/redirect", "waiting for your answer", nil)
	refuse("dead", "/agents/fg/redirect", "not running", nil)
	refuse("modal open", "/agents/eng-1/redirect", "permission dialog open", func() {
		fa.plain = map[string]string{"eng-1": "Do you want to proceed?\n❯ 1. Yes\n  2. No\n\nEsc to cancel\n"}
	})
	fa.plain = nil
	refuse("agent paused", "/agents/eng-1/redirect", "is paused", func() { _ = h.Store.SetAgentPaused("eng-1", true) })
	_ = h.Store.SetAgentPaused("eng-1", false)
	refuse("team paused", "/agents/eng-1/redirect", "team is paused", func() { _ = h.Store.SetKV(supervisor.KVPaused, "1") })
	_ = h.Store.SetKV(supervisor.KVPaused, "0")
	_ = h.Store.SetAgentStatus("eng-1", "idle")
	refuse("idle", "/agents/eng-1/redirect", "not busy", nil)

	// pause / resume go through the supervisor's own methods and refresh the page
	r := hx(h, "/agents/eng-1/pause", nil)
	if r.Code != 200 || r.Header.Get("HX-Refresh") != "true" || len(fa.paused) != 1 || fa.paused[0] != "eng-1" {
		t.Errorf("pause: %d %v %q", r.Code, fa.paused, r.Header.Get("HX-Refresh"))
	}
	if r := hx(h, "/agents/eng-1/resume", nil); r.Code != 200 || len(fa.resumed) != 1 {
		t.Errorf("resume: %d %v", r.Code, fa.resumed)
	}

	acts := h.WebActions()
	count := map[string]int{}
	for _, a := range acts {
		count[fmt.Sprint(a["action"])]++
	}
	if count["send"] != 3 || count["redirect"] != 7 || count["pause"] != 1 || count["resume"] != 1 {
		t.Errorf("web_action counts = %v", count)
	}
}

func TestRedirectNeedsASupervisor(t *testing.T) {
	h := seed(t, &fakeActions{}, webtest.SupervisorRunning(func() bool { return false }))
	r := hx(h, "/agents/eng-1/redirect", url.Values{"body": {"x"}})
	if r.Code != 422 || !strings.Contains(r.Body, "no supervisor is running") {
		t.Errorf("%d %s", r.Code, r.Body)
	}
}

// Read-only: every write is 403 and the controls are hidden.
func TestReadOnly(t *testing.T) {
	h := seed(t, &fakeActions{}, webtest.ReadOnly())
	for _, p := range []string{"/agents/eng-1/send", "/agents/eng-1/redirect", "/agents/eng-1/pause", "/agents/eng-1/resume", "/agents"} {
		if r := h.Post(p, url.Values{"body": {"x"}}); r.Code != 403 {
			t.Errorf("POST %s = %d", p, r.Code)
		}
	}
	d := h.Get("/agents/eng-1")
	if strings.Contains(d.Body, `hx-post="/agents/eng-1/send"`) || !strings.Contains(d.Body, "Read-only mode") {
		t.Error("write controls shown in read-only mode")
	}
	if strings.Contains(h.Get("/agents").Body, `href="/agents/new"`) {
		t.Error("New agent link shown in read-only mode")
	}
	if len(h.WebActions()) != 0 {
		t.Error("refused writes recorded web_action events")
	}
}

// P3 / addendum AC18: the disabled controls name the TUI key or command, from one table that
// matches the TUI's own help text.
func TestCLIOnlyActionsMatchTUIHelp(t *testing.T) {
	help, err := os.ReadFile("../../tui/help.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range agents.CLIOnlyActions {
		if a.Key == "" {
			continue
		}
		if !strings.Contains(string(help), "  "+a.Key+" ") {
			t.Errorf("TUI help has no entry for key %q (%s)", a.Key, a.Name)
		}
	}
	h := seed(t, &fakeActions{})
	d := h.Get("/agents/eng-1").Body
	for _, want := range []string{"Revive", "Restart fresh", "Reassign tasks", "<kbd>v</kbd>", "<kbd>R</kbd>", "slai up", "update_task"} {
		if !strings.Contains(d, want) {
			t.Errorf("detail lacks %q", want)
		}
	}
	if strings.Contains(d, `hx-post="/agents/eng-1/revive"`) || strings.Contains(d, "/restart") {
		t.Error("the page offers a CLI-only action")
	}
}

// ---- New Agent form ----

func teamFile(t *testing.T, h *webtest.Harness) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(h.Dir, ".slai"), 0o755); err != nil {
		t.Fatal(err)
	}
	team := config.Preset("solo", "p", "d", "main")
	team.Agents = []config.Agent{{ID: "orch", Role: config.RoleOrchestrator, Model: "opus"}, {ID: "eng-1", Role: config.RoleEngineer, Model: "sonnet"}}
	team.ApplyDefaults()
	path := filepath.Join(h.Dir, ".slai", "team.yaml")
	if err := config.Write(path, team); err != nil {
		t.Fatal(err)
	}
	return path
}

func newForm(over map[string]string) url.Values {
	v := url.Values{"role": {"engineer"}, "harness": {"claude"}, "id": {"eng-2"}, "focus": {"frontend"}, "model": {"sonnet"}, "persona": {""}, "perm": {"auto"}}
	for k, s := range over {
		v.Set(k, s)
	}
	return v
}

// addendum AC20: every field has one line of help; the submit area says what the button does.
func TestNewAgentFormRendersHelp(t *testing.T) {
	h := seed(t, &fakeActions{})
	teamFile(t, h)
	r := h.Get("/agents/new")
	h.CheckPage(r)
	for _, id := range []string{"h-role", "h-harness", "h-id", "h-focus", "h-model", "h-persona", "h-perm"} {
		if !strings.Contains(r.Body, `id="`+id+`"`) {
			t.Errorf("no help line %s", id)
		}
	}
	for _, want := range []string{"key in <code>models:</code>", "Harness", "Focus", "Permission mode", `value="eng-2"`} {
		if !strings.Contains(r.Body, want) {
			t.Errorf("form lacks %q", want)
		}
	}
	i, j := strings.Index(r.Body, "This adds an entry to <code>team.yaml</code>"), strings.Index(r.Body, `type="submit"`)
	if i < 0 || j < 0 || i > j {
		t.Errorf("the team.yaml sentence must come before the submit button (%d, %d)", i, j)
	}
}

// addendum AC1: an invalid field is refused with the validator's message inline; nothing is written.
func TestNewAgentRejectsInvalidFields(t *testing.T) {
	cases := []struct {
		name string
		over map[string]string
		want string
	}{
		{"id", map[string]string{"id": "Not A Valid ID!"}, "id"},
		{"empty id", map[string]string{"id": ""}, "id"},
		{"role", map[string]string{"role": "wizard"}, "role"},
		{"model", map[string]string{"model": "no-such-model"}, "model"},
		{"harness", map[string]string{"harness": "emacs"}, "harness"},
		{"permission", map[string]string{"perm": "yolo"}, "permission"},
		{"second orchestrator", map[string]string{"role": "orchestrator", "id": "orch-2", "model": "opus"}, "orchestrator"},
	}
	h := seed(t, &fakeActions{})
	path := teamFile(t, h)
	before, _ := os.ReadFile(path)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := hx(h, "/agents", newForm(c.over))
			if r.Code != 422 {
				t.Fatalf("status %d: %.300s", r.Code, r.Body)
			}
			if !strings.Contains(strings.ToLower(r.Body), c.want) || !strings.Contains(r.Body, `role="alert"`) {
				t.Errorf("no inline message mentioning %q:\n%s", c.want, r.Body)
			}
			after, _ := os.ReadFile(path)
			if string(after) != string(before) {
				t.Error("team.yaml changed")
			}
		})
	}
}

// addendum AC2: a duplicate id is refused and team.yaml is unchanged.
func TestNewAgentRejectsDuplicateID(t *testing.T) {
	h := seed(t, &fakeActions{})
	path := teamFile(t, h)
	before, _ := os.ReadFile(path)
	r := hx(h, "/agents", newForm(map[string]string{"id": "eng-1"}))
	if r.Code != 422 || !strings.Contains(strings.ToLower(r.Body), "duplicate") && !strings.Contains(r.Body, "eng-1") {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Error("team.yaml changed")
	}
}

// addendum AC3: the written team.yaml equals the TUI form's own path (AddAgent + Write) for the
// same inputs, for a claude agent, a focus group and a pi agent.
func TestNewAgentWritesWhatTheTUIPathWrites(t *testing.T) {
	cases := []struct {
		name  string
		over  map[string]string
		agent config.Agent
	}{
		{"claude engineer", nil, config.Agent{ID: "eng-2", Role: "engineer", Model: "sonnet", Focus: "frontend", PermissionMode: "auto"}},
		{"focus group", map[string]string{"role": "focus_group", "id": "fg-1", "model": "haiku", "persona": " A busy nurse ", "perm": ""},
			config.Agent{ID: "fg-1", Role: "focus_group", Model: "haiku", Focus: "frontend", Persona: "A busy nurse"}},
		{"pi harness", map[string]string{"harness": "pi", "model": "zai/glm-5.3", "perm": "auto"},
			config.Agent{ID: "eng-2", Role: "engineer", Model: "zai/glm-5.3", Focus: "frontend", Harness: "pi"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := seed(t, &fakeActions{})
			path := teamFile(t, h)
			want, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := config.AddAgent(want, c.agent); err != nil {
				t.Fatal(err)
			}
			wantPath := filepath.Join(t.TempDir(), "team.yaml")
			if err := config.Write(wantPath, want); err != nil {
				t.Fatal(err)
			}
			r := hx(h, "/agents", newForm(c.over))
			if r.Code != 200 || !strings.Contains(r.Body, "added to team.yaml") {
				t.Fatalf("%d %s", r.Code, r.Body)
			}
			got, _ := os.ReadFile(path)
			exp, _ := os.ReadFile(wantPath)
			if string(got) != string(exp) {
				t.Errorf("team.yaml differs from the TUI path:\n%s\n---\n%s", got, exp)
			}
			acts := h.WebActions()
			if len(acts) != 1 || acts[0]["action"] != "new_agent" || acts[0]["target"] != c.agent.ID {
				t.Errorf("web_action = %v", acts)
			}
		})
	}
}

func TestNewAgentNeedsHarnessBinary(t *testing.T) {
	h := seed(t, &fakeActions{})
	path := teamFile(t, h)
	before, _ := os.ReadFile(path)
	agents.SetLookPath(func(string) (string, error) { return "", errors.New("not found") })
	t.Cleanup(agents.ResetLookPath)
	r := hx(h, "/agents", newForm(map[string]string{"harness": "opencode", "model": "zai/glm-5.3"}))
	if r.Code != 422 || !strings.Contains(r.Body, "opencode not found on PATH") {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Error("team.yaml changed")
	}
}

// addendum AC5: after the supervisor reconciles, the new agent shows on the agents list.
func TestNewAgentAppearsAfterReconcile(t *testing.T) {
	h := seed(t, &fakeActions{})
	path := teamFile(t, h)
	if r := hx(h, "/agents", newForm(nil)); r.Code != 200 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	team, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	h.Team.Agents = team.Agents     // the server's hot-reloaded view of team.yaml
	for _, a := range team.Agents { // the fake supervisor's reconcile: it creates the agent row
		_ = h.Store.UpsertAgent(store.Agent{ID: a.ID, Role: a.Role, Model: a.Model})
	}
	if r := h.Get("/agents"); !strings.Contains(r.Body, ">eng-2<") {
		t.Errorf("new agent not on the list:\n%s", r.Body)
	}
}

// Without a supervisor-backed Actions the pause still sets the same store flag.
func TestPauseFallsBackToTheStore(t *testing.T) {
	h := webtest.New(t, webtest.WithModules(agents.Module))
	h.Login()
	_ = h.Store.UpsertAgent(store.Agent{ID: "eng-1", Role: "engineer"})
	if r := hx(h, "/agents/eng-1/pause", nil); r.Code != 200 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	if p, _ := h.Store.PausedAgents(); !p["eng-1"] {
		t.Error("agent not paused")
	}
	hx(h, "/agents/eng-1/resume", nil)
	if p, _ := h.Store.PausedAgents(); p["eng-1"] {
		t.Error("agent still paused")
	}
}

// A form post without htmx (no JavaScript) gets the whole page back with the error inline.
func TestNewAgentErrorWithoutHTMXIsAFullPage(t *testing.T) {
	h := seed(t, &fakeActions{})
	teamFile(t, h)
	r := h.Do(h.Browser("POST", "/agents", newForm(map[string]string{"role": "wizard"})))
	if r.Code != 422 || !strings.Contains(r.Body, "<html") || !strings.Contains(r.Body, `role="alert"`) {
		t.Fatalf("%d %.200s", r.Code, r.Body)
	}
}

// T-0115 item 6: the list no longer repeats "see the Dashboard" on every row; the plan limits
// are shown once above the table, and a harness without gauges says so in its own cell.
func TestAgentsListShowsPlanLimitsOnceNotAColumn(t *testing.T) {
	h := seed(t, &fakeActions{pane: map[string]string{}})
	_ = ratelimit.Save(h.Store, ratelimit.Snapshot{FiveHour: &ratelimit.Window{UsedPercent: 41, ResetsAt: 1_800_000_000 + 2400}, At: 1_800_000_000})
	r := h.Get("/agents")
	h.CheckPage(r)
	if strings.Contains(r.Body, "5h / weekly gauges") || strings.Contains(r.Body, "see the Dashboard") {
		t.Error("the per-row gauges column is still there")
	}
	if strings.Count(r.Body, `id="plan-limits"`) != 1 || !strings.Contains(r.Body, "5-hour window 41%") || !strings.Contains(r.Body, "Weekly window n/a") {
		t.Errorf("the plan limits line is missing or repeated")
	}
	if !strings.Contains(r.Body, "n/a — not supported by pi") {
		t.Error("an agent on pi does not say it has no gauges")
	}
}
