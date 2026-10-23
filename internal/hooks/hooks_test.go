package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/store"
)

func newStore(t *testing.T, status, task string) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.UpsertAgent(store.Agent{ID: "e1", Role: "engineer", Status: status, CurrentTask: task}); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestTransitions(t *testing.T) {
	cases := []struct {
		name, event, from string
		p                 Payload
		want              string
	}{
		{"prompt idle", "UserPromptSubmit", "idle", Payload{}, "working"},
		{"prompt clears waiting_on_user", "userpromptsubmit", "waiting_on_user", Payload{}, "working"},
		{"pretool", "pretooluse", "idle", Payload{ToolName: "Bash"}, "working"},
		{"pretool after permission", "pretooluse", "waiting", Payload{}, "working"},
		{"pretool keeps wou", "pretooluse", "waiting_on_user", Payload{}, "waiting_on_user"},
		{"perm", "notification", "working", Payload{NotificationType: "permission_prompt"}, "waiting"},
		{"needs input", "notification", "working", Payload{NotificationType: "agent_needs_input"}, "waiting"},
		{"elicitation", "notification", "working", Payload{NotificationType: "elicitation_dialog"}, "waiting"},
		{"idle prompt", "notification", "working", Payload{NotificationType: "idle_prompt"}, "idle"},
		{"idle prompt keeps finished", "notification", "finished", Payload{NotificationType: "idle_prompt"}, "finished"},
		{"notif wou", "notification", "waiting_on_user", Payload{NotificationType: "permission_prompt"}, "waiting_on_user"},
		{"notif other", "notification", "working", Payload{NotificationType: "auth_success"}, "working"},
		{"notif legacy msg", "notification", "working", Payload{Message: "Claude needs your permission to use Bash"}, "waiting"},
		{"stop after work", "stop", "working", Payload{}, "finished"},
		{"stop idle", "stop", "idle", Payload{}, "idle"},
		{"stop waiting", "stop", "waiting", Payload{}, "idle"},
		{"stop finished", "stop", "finished", Payload{}, "finished"},
		{"stop wou", "stop", "waiting_on_user", Payload{}, "waiting_on_user"},
		{"session start", "SessionStart", "dead", Payload{}, "idle"},
		{"session start wou", "sessionstart", "waiting_on_user", Payload{}, "waiting_on_user"},
		{"unknown", "bogus", "working", Payload{}, "working"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := newStore(t, c.from, "")
			if _, err := Handle(st, "e1", c.event, c.p, Options{}); err != nil {
				t.Fatal(err)
			}
			ag, _ := st.AgentByID("e1")
			if ag.Status != c.want {
				t.Fatalf("got %s want %s", ag.Status, c.want)
			}
		})
	}
}

func TestEventAndTurns(t *testing.T) {
	st := newStore(t, "idle", "")
	for i := 0; i < 3; i++ {
		Handle(st, "e1", "userpromptsubmit", Payload{}, Options{})
	}
	Handle(st, "e1", "pretooluse", Payload{}, Options{})
	ag, _ := st.AgentByID("e1")
	if ag.Turns != 3 || ag.StatusAt == 0 {
		t.Fatalf("%+v", ag)
	}
	evs, _ := st.EventsByKind("e1", "hook:userpromptsubmit", 10)
	if len(evs) != 3 {
		t.Fatalf("events %d", len(evs))
	}
	if _, err := Handle(st, "nobody", "stop", Payload{}, Options{}); err == nil {
		t.Fatal("unknown agent should error")
	}
}

func TestMaxTurnsGuard(t *testing.T) {
	st := newStore(t, "idle", "T-0001")
	var fired int
	for i := 0; i < 5; i++ {
		r, err := Handle(st, "e1", "userpromptsubmit", Payload{}, Options{MaxTurnsPerTask: 3})
		if err != nil {
			t.Fatal(err)
		}
		if r.Guard != nil {
			fired++
			if i != 3 || r.Guard.Turns != 4 || r.Guard.Task != "T-0001" {
				t.Fatalf("guard at %d: %+v", i, r.Guard)
			}
		}
	}
	if fired != 1 {
		t.Fatalf("fired %d", fired)
	}
	evs, _ := st.EventsByKind("e1", "guard:max_turns", 10)
	if len(evs) != 1 {
		t.Fatal("no guard event")
	}
	var d map[string]any
	json.Unmarshal([]byte(evs[0].Data), &d)
	if d["agent"] != "e1" || d["task"] != "T-0001" || d["turns"] != float64(4) {
		t.Fatalf("%v", d)
	}
	msgs, _ := st.UndeliveredFor("e1")
	if len(msgs) != 1 || msgs[0].From != "slai" {
		t.Fatalf("msgs %+v", msgs)
	}
	// no task => no guard
	st2 := newStore(t, "idle", "")
	for i := 0; i < 3; i++ {
		if r, _ := Handle(st2, "e1", "userpromptsubmit", Payload{}, Options{MaxTurnsPerTask: 1}); r.Guard != nil {
			t.Fatal("guard without task")
		}
	}
}

func TestStopRecordsUsage(t *testing.T) {
	st := newStore(t, "working", "")
	tp := filepath.Join(t.TempDir(), "t.jsonl")
	os.WriteFile(tp, []byte(`{"message":{"id":"m","model":"claude-opus-5-5","usage":{"input_tokens":1,"output_tokens":9}}}`+"\n"), 0o644)
	p := Payload{TranscriptPath: tp}
	r, err := Handle(st, "e1", "stop", p, Options{})
	if err != nil || !r.UsageOK || r.Usage.Output != 9 {
		t.Fatalf("%+v %v", r, err)
	}
	r, _ = Handle(st, "e1", "stop", p, Options{})
	if r.Usage.Output != 0 {
		t.Fatalf("double counted: %+v", r)
	}
	if v, _ := st.TodaySpend(); v <= 0 {
		t.Fatal("no spend")
	}
}

func TestDetectFromPane(t *testing.T) {
	cases := []struct {
		name, text, want string
		ok               bool
	}{
		{"permission", "Bash command\n  ls\n Do you want to proceed?\n ❯ 1. Yes\n  2. No", "waiting", true},
		{"permission beats esc", "esc to interrupt\n Do you want to proceed?", "waiting", true},
		{"working", "✻ Thinking… (12s · esc to interrupt)", "working", true},
		{"idle box", "╭────╮\n│ >    │\n╰────╯\n  ? for shortcuts", "idle", true},
		{"idle shortcuts", "  ? for shortcuts", "idle", true},
		{"none", "$ ls\nfoo", "", false},
		{"empty", "", "", false},
	}
	for _, c := range cases {
		got, ok := DetectFromPane(c.text)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: got %q,%v", c.name, got, ok)
		}
	}
	// stale text above the tail window is ignored
	old := "esc to interrupt\n"
	for i := 0; i < 40; i++ {
		old += "line\n"
	}
	if _, ok := DetectFromPane(old); ok {
		t.Error("stale text matched")
	}
}

func TestDetectFromPaneTrustDialog(t *testing.T) {
	pane := " Accessing workspace:\n\n /tmp/x\n\n Quick safety check: Is this a project you created or one you trust? (Like your own code)\n\n ❯ 1. Yes, I trust this folder\n   2. No, exit\n"
	if st, ok := DetectFromPane(pane); !ok || st != StatusWaiting {
		t.Fatalf("trust dialog = %q,%v; want waiting", st, ok)
	}
}

func TestDetectRateLimitAndModalFixtures(t *testing.T) {
	read := func(n string) string {
		b, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	const sup, claude = "../supervisor/testdata/panes/", "testdata/panes/claude/"
	for _, c := range []struct{ file, want string }{
		{sup + "ratelimit_reset.txt", StatusRateLimited},
		{sup + "ratelimit_noreset.txt", StatusRateLimited},
		{claude + "permission-dialog.txt", StatusWaiting},
		{claude + "idle-prompt.txt", StatusIdle},
	} {
		if got, ok := DetectFromPane(read(c.file)); !ok || got != c.want {
			t.Errorf("%s = %q,%v; want %q", c.file, got, ok, c.want)
		}
	}
	if ModalExcerpt(read(claude+"permission-dialog.txt")) == "" || ModalExcerpt(read(claude+"idle-prompt.txt")) != "" {
		t.Error("ModalExcerpt should match the permission prompt only")
	}
}

func TestParseRateLimitReset(t *testing.T) {
	now := time.Date(2026, 1, 5, 14, 0, 0, 0, time.UTC)
	cases := []struct {
		in   string
		want string // "" = not parsed
	}{
		{"5-hour limit reached ∙ resets 3pm", "2026-01-05 15:00"},
		{"limit reached, resets at 3:30pm (America/New_York)", "2026-01-05 15:30"},
		{"resets 9am", "2026-01-06 09:00"}, // already past today: tomorrow
		{"resets at 15:45", "2026-01-05 15:45"},
		{"resets 12am", "2026-01-06 00:00"},
		{"usage limit reached", ""},
		{"resets in 5 hours", ""},
	}
	for _, c := range cases {
		got, ok := ParseRateLimitReset(c.in, now)
		if c.want == "" {
			if ok {
				t.Errorf("%q parsed as %v", c.in, got)
			}
			continue
		}
		if !ok || got.Format("2006-01-02 15:04") != c.want {
			t.Errorf("%q = %v,%v; want %s", c.in, got, ok, c.want)
		}
	}
}

func TestRateLimitTextInOldScrollbackIsIgnored(t *testing.T) {
	pane := "5-hour limit reached ∙ resets 3pm\n"
	for i := 0; i < 12; i++ {
		pane += "some later output\n"
	}
	pane += "│ >\n? for shortcuts\n"
	if st, ok := DetectFromPane(pane); !ok || st != StatusIdle {
		t.Fatalf("old rate-limit text = %q,%v; want idle", st, ok)
	}
}

func TestReviveFreshOnlyWhenSessionDidNotResume(t *testing.T) {
	for _, c := range []struct {
		source    string
		wantFresh bool
	}{{"startup", true}, {"resume", false}, {"", false}} {
		st := newStore(t, "idle", "")
		_ = st.UpsertAgent(store.Agent{ID: "orch", Role: "orchestrator"})
		_ = st.SetKV(KVRevived+"e1", strconv.FormatInt(time.Now().Unix(), 10))
		if _, err := Handle(st, "e1", "SessionStart", Payload{Source: c.source}, Options{}); err != nil {
			t.Fatal(err)
		}
		evs, _ := st.EventsByKind("e1", "revive_fresh", 5)
		msgs, _ := st.UndeliveredFor("orch")
		if got := len(evs) == 1; got != c.wantFresh || (len(msgs) == 1) != c.wantFresh {
			t.Errorf("source %q: events=%d orch notes=%d, wantFresh=%v", c.source, len(evs), len(msgs), c.wantFresh)
		}
	}
	// a SessionStart that is not a revive is never reported
	st := newStore(t, "idle", "")
	_, _ = Handle(st, "e1", "SessionStart", Payload{Source: "startup"}, Options{})
	if evs, _ := st.EventsByKind("e1", "revive_fresh", 5); len(evs) != 0 {
		t.Error("first start reported as revive_fresh")
	}
}

func TestSlaiToolPreUseLeavesMCPPendingStamp(t *testing.T) {
	st := newStore(t, "idle", "")
	_, _ = Handle(st, "e1", "PreToolUse", Payload{ToolName: "Bash"}, Options{})
	if _, ok, _ := st.GetKV(KVMCPPending + "e1"); ok {
		t.Fatal("non-slai tool left a pending stamp")
	}
	_, _ = Handle(st, "e1", "PreToolUse", Payload{ToolName: "mcp__slai__get_inbox"}, Options{})
	if _, ok, _ := st.GetKV(KVMCPPending + "e1"); !ok {
		t.Fatal("mcp__slai__ call left no pending stamp")
	}
}

func TestModalDetectionIgnoresQuotedPromptsAndSummarizesLiveOnes(t *testing.T) {
	read := func(n string) string {
		b, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	const sup, claude = "../supervisor/testdata/panes/", "testdata/panes/claude/"
	// slai's own note quoted in the orchestrator's pane is not a live prompt
	for _, f := range []string{sup + "orch_quoted_modal_note.txt", sup + "orch_quoted_modal_note_working.txt"} {
		if ex := ModalExcerpt(read(f)); ex != "" {
			t.Errorf("%s: quoted note matched as a modal: %q", f, ex)
		}
		if st, _ := DetectFromPane(read(f)); st == StatusWaiting {
			t.Errorf("%s: DetectFromPane read a quoted note as waiting", f)
		}
	}
	// a live prompt still matches, and the summary is safe to repeat
	for _, f := range []string{claude + "permission-dialog.txt"} {
		ex := ModalExcerpt(read(f))
		if !strings.HasPrefix(ex, "permission prompt") {
			t.Errorf("%s: summary = %q", f, ex)
		}
		if st, ok := DetectFromPane(read(f)); !ok || st != StatusWaiting {
			t.Errorf("%s: DetectFromPane = %q,%v", f, st, ok)
		}
		// self-poisoning check: the summary, shown in a pane, must not look like a prompt
		for _, bad := range []string{"Do you want", "❯", "1. Yes", "2. "} {
			if strings.Contains(ex, bad) {
				t.Errorf("%s: summary %q repeats %q", f, ex, bad)
			}
		}
		if ModalExcerpt("slai says: "+ex+"\n"+read(claude+"idle-prompt.txt")) != "" || ModalExcerpt(ex) != "" {
			t.Errorf("%s: the summary %q is itself detected as a modal", f, ex)
		}
	}
	if ex := ModalExcerpt(read(claude + "permission-dialog.txt")); !strings.Contains(ex, "spike-file.txt") {
		t.Errorf("summary should name the command, got %q", ex)
	}
	// a prompt buried in old scrollback (more than the live region) is history
	old := read(claude + "permission-dialog.txt")
	for i := 0; i < 20; i++ {
		old += "later output line\n"
	}
	if ModalExcerpt(old) != "" {
		t.Error("a prompt 20 lines up matched as live")
	}
}

func TestStablePaneIgnoresSpinnerAndFooter(t *testing.T) {
	a := "● Bash(x)\n✻ Deciphering… (1m 4s · ↓ 2.1k tokens)\n──────────────\n❯ \n──────────────\n  5h 10% $ 4.2"
	b := "● Bash(x)\n· Deciphering… (1m 5s · ↓ 2.4k tokens)\n──────────────\n❯ \n──────────────\n  5h 11% $ 4.3"
	if StablePane(a) != StablePane(b) {
		t.Fatalf("%q != %q", StablePane(a), StablePane(b))
	}
	if StablePane(a) == StablePane("● Bash(x)\n● Read(y)\n"+b) {
		t.Fatal("new tool line ignored")
	}
}

func TestToolRunning(t *testing.T) {
	if !ToolRunning("Bash(x)\n  (ctrl+b ctrl+b (twice) to run in background)\n❯ ") || ToolRunning("I saw (ctrl+b ctrl+b (twice) to run in background) once") {
		t.Fatal("ToolRunning wrong")
	}
}

func identityOf(agent string) string {
	return "You are " + agent + ", the engineer of this team (see `whoami`). You build."
}

// After a resume, compaction, /clear or a blank start right after a revive, the agent is
// queued an inbox note that opens with who it is, and its next wake line is marked.
func TestSessionStartReGroundsAfterAGap(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		revived      bool
		want         bool
	}{
		{"resume", "resume", false, true},
		{"compact", "compact", false, true},
		{"clear", "clear", false, true},
		{"startup after revive", "startup", true, true},
		{"first startup", "startup", false, false},
		{"unknown source", "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t, "idle", "")
			if tc.revived {
				st.SetKV(KVRevived+"e1", strconv.FormatInt(time.Now().Unix(), 10))
			}
			if _, err := Handle(st, "e1", "SessionStart", Payload{Source: tc.source}, Options{IdentityLine: identityOf}); err != nil {
				t.Fatal(err)
			}
			msgs, _ := st.UndeliveredFor("e1")
			pending, hasPending, _ := st.GetKV(KVIdentityPending + "e1")
			var identity []string
			for _, m := range msgs {
				if strings.HasPrefix(m.Body, "You are e1, the engineer of this team") {
					identity = append(identity, m.Body)
				}
			}
			if tc.want && (len(identity) != 1 || !hasPending || pending != tc.source) {
				t.Fatalf("no re-ground: msgs=%v pending=%v/%q", msgs, hasPending, pending)
			}
			if !tc.want && (len(identity) != 0 || hasPending) {
				t.Fatalf("re-grounded without a gap: %v %v", msgs, hasPending)
			}
		})
	}
}

// Without an IdentityLine (no team.yaml to build it from) nothing is queued.
func TestSessionStartWithoutIdentitySourceQueuesNothing(t *testing.T) {
	st := newStore(t, "idle", "")
	if _, err := Handle(st, "e1", "SessionStart", Payload{Source: "compact"}, Options{}); err != nil {
		t.Fatal(err)
	}
	if m, _ := st.UndeliveredFor("e1"); len(m) != 0 {
		t.Fatalf("msgs = %v", m)
	}
}

func lastSessionStartEvent(t *testing.T, st *store.Store, agent string) map[string]any {
	t.Helper()
	evs, _ := st.RecentEvents(50)
	for _, e := range evs { // newest first
		if e.Agent == agent && e.Kind == "hook:sessionstart" {
			var d map[string]any
			_ = json.Unmarshal([]byte(e.Data), &d)
			return d
		}
	}
	t.Fatal("no hook:sessionstart event")
	return nil
}

func TestSessionStartStoresSource(t *testing.T) {
	for _, src := range []string{"startup", "resume", "clear", "compact"} {
		st := newStore(t, "idle", "")
		if _, err := Handle(st, "e1", "SessionStart", Payload{Source: src, SessionID: "s1"}, Options{}); err != nil {
			t.Fatal(err)
		}
		if got := lastSessionStartEvent(t, st, "e1")["source"]; got != src {
			t.Errorf("source %q stored as %v", src, got)
		}
	}
}

// /clear starts a conversation under a new id: session-id follows it, so a later revive
// resumes the new conversation, not the cleared one.
func TestClearSessionStartRewritesSessionID(t *testing.T) {
	st := newStore(t, "idle", "")
	proj := t.TempDir()
	path := filepath.Join(proj, ".slai", "run", "e1", "session-id")
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte("old-id\n"), 0o644)
	if _, err := Handle(st, "e1", "SessionStart", Payload{Source: "clear", SessionID: "new-id"}, Options{ProjectDir: proj}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); strings.TrimSpace(string(b)) != "new-id" {
		t.Fatalf("session-id = %q", b)
	}
	// only /clear moves it: other sources, and an event without an id, change nothing
	Handle(st, "e1", "SessionStart", Payload{Source: "compact", SessionID: "other"}, Options{ProjectDir: proj})
	Handle(st, "e1", "SessionStart", Payload{Source: "startup", SessionID: "other"}, Options{ProjectDir: proj})
	Handle(st, "e1", "SessionStart", Payload{Source: "clear"}, Options{ProjectDir: proj})
	if b, _ := os.ReadFile(path); strings.TrimSpace(string(b)) != "new-id" {
		t.Fatalf("session-id = %q", b)
	}
}

// compact and clear re-ground through the trigger T-0029 wired; one note each, not two.
func TestCompactAndClearReGroundOnce(t *testing.T) {
	for _, src := range []string{"compact", "clear"} {
		st := newStore(t, "idle", "")
		Handle(st, "e1", "SessionStart", Payload{Source: src, SessionID: "x"}, Options{IdentityLine: identityOf})
		n := 0
		msgs, _ := st.UndeliveredFor("e1")
		for _, m := range msgs {
			if strings.HasPrefix(m.Body, "You are e1") {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%s: %d identity notes", src, n)
		}
	}
}

// A fresh (non-resumed) revive of the orchestrator is raised to the user; a resumed one,
// and a fresh revive of anyone else, is not.
func TestOrchestratorFreshReviveTellsTheUser(t *testing.T) {
	setup := func(t *testing.T, role string) *store.Store {
		st := newStore(t, "idle", "")
		st.UpsertAgent(store.Agent{ID: "boss", Role: role, Status: "idle"})
		st.SetKV(KVRevived+"boss", strconv.FormatInt(time.Now().Unix(), 10))
		return st
	}
	userItems := func(st *store.Store) []store.Escalation {
		var out []store.Escalation
		escs, _ := st.OpenEscalations()
		for _, e := range escs {
			if e.User && e.Kind == EscReviveFresh {
				out = append(out, e)
			}
		}
		return out
	}
	st := setup(t, "orchestrator")
	Handle(st, "boss", "SessionStart", Payload{Source: "startup"}, Options{})
	got := userItems(st)
	if len(got) != 1 || got[0].Agent != "boss" || !strings.Contains(got[0].Text, "orchestrator") {
		t.Fatalf("escalations = %+v", got)
	}
	if d := lastSessionStartEvent(t, st, "boss"); d["source"] != "startup" {
		t.Fatalf("%v", d)
	}
	evs, _ := st.RecentEvents(20)
	found := false
	for _, e := range evs {
		if e.Kind == "revive_fresh" && strings.Contains(e.Data, `"escalation":true`) && strings.Contains(e.Data, `"user":true`) {
			found = true
		}
	}
	if !found {
		t.Fatal("no user-facing revive_fresh event (notify keys on escalation:true)")
	}
	if st := setup(t, "orchestrator"); true {
		Handle(st, "boss", "SessionStart", Payload{Source: "resume"}, Options{})
		if len(userItems(st)) != 0 {
			t.Fatal("a resumed revive was raised to the user")
		}
	}
	if st := setup(t, "engineer"); true {
		Handle(st, "boss", "SessionStart", Payload{Source: "startup"}, Options{})
		if len(userItems(st)) != 0 {
			t.Fatal("a non-orchestrator revive was raised to the user")
		}
	}
}
