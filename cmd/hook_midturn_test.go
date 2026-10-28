package cmd

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/hooks"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/supervisor"
)

// postHook runs the real hook entry point for a PostToolUse of tool by eng-1.
func postHook(t *testing.T, dir, tool string) string {
	t.Helper()
	env := map[string]string{"SLAI_AGENT_ID": "eng-1", "SLAI_PROJECT": dir}
	payload := `{"session_id":"s1","hook_event_name":"PostToolUse","tool_name":"` + tool + `","tool_input":{}}`
	return string(runHook("posttooluse", strings.NewReader(payload), func(k string) string { return env[k] }))
}

func noticeText(t *testing.T, out string) string {
	t.Helper()
	var d struct {
		H struct {
			Event string `json:"hookEventName"`
			Ctx   string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &d); err != nil || d.H.Event != "PostToolUse" {
		t.Fatalf("hook output = %q, %v", out, err)
	}
	return d.H.Ctx
}

func midturnProject(t *testing.T) (string, *store.Store) {
	t.Helper()
	dir := project(t, true) // solo: orch + eng-1
	st, err := store.Open(filepath.Join(dir, ".slai", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.UpsertAgent(store.Agent{ID: "eng-1", Role: "engineer", Status: "working"}); err != nil {
		t.Fatal(err)
	}
	return dir, st
}

func addMail(t *testing.T, st *store.Store, body string) int64 {
	t.Helper()
	id, err := st.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "note", Body: body})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestMidTurnNoticeOncePerNewItem(t *testing.T) {
	dir, st := midturnProject(t)
	if out := postHook(t, dir, "Bash"); out != "" {
		t.Fatalf("notice with no mail: %q", out)
	}
	addMail(t, st, "one")
	if got := noticeText(t, postHook(t, dir, "Bash")); got != "[slai] You have 1 new items. Call get_inbox." {
		t.Fatalf("notice = %q", got)
	}
	if out := postHook(t, dir, "Edit"); out != "" {
		t.Fatalf("the same item was announced twice: %q", out)
	}
	addMail(t, st, "two")
	addMail(t, st, "three")
	if got := noticeText(t, postHook(t, dir, "Bash")); !strings.Contains(got, "You have 2 new items") {
		t.Fatalf("notice for two new items = %q", got)
	}
	if out := postHook(t, dir, "Bash"); out != "" {
		t.Fatalf("repeated: %q", out)
	}
	// nothing was marked delivered: get_inbox still returns all three
	if ms, _ := st.UndeliveredFor("eng-1"); len(ms) != 3 {
		t.Fatalf("undelivered = %d, want 3 (the hook must not deliver)", len(ms))
	}
	// and the hook neither recorded an event nor touched the status
	if evs, _ := st.EventsAfter(0, "hook:posttooluse", 10); len(evs) != 0 {
		t.Fatalf("posttooluse recorded %d events", len(evs))
	}
	if a, _ := st.AgentByID("eng-1"); a.Status != "working" {
		t.Fatalf("status = %s", a.Status)
	}
}

func TestMidTurnNoticeSkips(t *testing.T) {
	dir, st := midturnProject(t)
	id := addMail(t, st, "already typed")
	if err := st.MarkWoken(id); err != nil {
		t.Fatal(err)
	}
	if out := postHook(t, dir, "Bash"); out != "" {
		t.Fatalf("mail the wake loop already typed was announced again: %q", out)
	}
	addMail(t, st, "fresh")
	if out := postHook(t, dir, "mcp__slai__get_inbox"); out != "" {
		t.Fatalf("notice right after the agent's own get_inbox: %q", out)
	}
	// get_inbox call skipped without consuming the item: the next tool call still announces it
	if got := noticeText(t, postHook(t, dir, "Bash")); !strings.Contains(got, "1 new items") {
		t.Fatalf("notice = %q", got)
	}
}

func TestMidTurnNoticeSkipsInboxHeldAssignment(t *testing.T) {
	dir, st := midturnProject(t)
	id := addMail(t, st, "assignment waiting for branch")
	if err := st.HoldInbox(id); err != nil {
		t.Fatal(err)
	}
	if out := postHook(t, dir, "Bash"); out != "" {
		t.Fatalf("held item announced before it can be claimed: %q", out)
	}
	if err := st.ReleaseInbox(id); err != nil {
		t.Fatal(err)
	}
	if got := noticeText(t, postHook(t, dir, "Bash")); !strings.Contains(got, "1 new items") {
		t.Fatalf("released item not announced: %q", got)
	}
}

func TestMidTurnNoticeRespectsHolds(t *testing.T) {
	for name, hold := range map[string]func(*store.Store){
		"agent paused": func(st *store.Store) { _ = st.SetAgentPaused("eng-1", true) },
		"global pause": func(st *store.Store) { _ = st.SetKV(supervisor.KVPaused, "1") },
		"team hold":    func(st *store.Store) { _ = st.SetKV(supervisor.KVTeamHold, "lunch") },
		"loop-guard hold": func(st *store.Store) {
			_ = st.SetKV(supervisor.KVHoldPrefix+"eng-1", itoa(time.Now().Add(time.Minute).Unix()))
		},
		"wake gate held": func(st *store.Store) { _ = st.SetKV("wake.skip.eng-1", "held|1") },
	} {
		t.Run(name, func(t *testing.T) {
			dir, st := midturnProject(t)
			addMail(t, st, "x")
			hold(st)
			if out := postHook(t, dir, "Bash"); out != "" {
				t.Fatalf("a hold was bypassed by a busy turn: %q", out)
			}
		})
	}
}

func TestMidTurnNoticeCarriesIdentityAfterAGap(t *testing.T) {
	dir, st := midturnProject(t)
	addMail(t, st, "x")
	_ = st.SetKV(hooks.KVIdentityPending+"eng-1", "compact")
	got := noticeText(t, postHook(t, dir, "Bash"))
	if !strings.HasPrefix(got, "You are eng-1, the engineer of this team") || !strings.HasSuffix(got, "[slai] You have 1 new items. Call get_inbox.") {
		t.Fatalf("notice = %q", got)
	}
	if _, ok, _ := st.GetKV(hooks.KVIdentityPending + "eng-1"); ok {
		t.Fatal("the identity marker should be consumed once the agent was re-grounded")
	}
}
