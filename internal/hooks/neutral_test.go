package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/store"
)

func neutralStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.UpsertAgent(store.Agent{ID: "g1", Role: "engineer"}); err != nil {
		t.Fatal(err)
	}
	return st
}

// Every row of the neutral -> Claude mapping table (decisions.md, T-0027) behaves
// like the Claude event it maps to: same status, same recorded hook event.
func TestNeutralEventMappingTable(t *testing.T) {
	rows := []struct {
		neutral    string
		from       string // agent status before
		wantStatus string
		wantEvent  string
	}{
		{"session_start", "dead", StatusIdle, "hook:sessionstart"},
		{"prompt", StatusIdle, StatusWorking, "hook:userpromptsubmit"},
		{"tool", StatusIdle, StatusWorking, "hook:pretooluse"},
		{"tool_end", StatusIdle, StatusWorking, "hook:posttooluse"},
		{"idle", StatusWorking, StatusIdle, "hook:notification"},
		{"waiting", StatusWorking, StatusWaiting, "hook:notification"},
		{"stop", StatusWorking, StatusFinished, "hook:stop"},
		{"stop", StatusIdle, StatusIdle, "hook:stop"},
		{"guard_ready", StatusIdle, StatusIdle, "hook:guard_ready"},
	}
	for _, r := range rows {
		t.Run(r.neutral+"/"+r.from, func(t *testing.T) {
			st := neutralStore(t)
			_ = st.SetAgentStatus("g1", r.from)
			ev, p, neutral, err := Adapt(r.neutral, Payload{Event: r.neutral, Tool: "bash", SessionID: "s1"})
			if err != nil || !neutral {
				t.Fatalf("Adapt: %v neutral=%v", err, neutral)
			}
			if _, err := Handle(st, "g1", ev, p, Options{}); err != nil {
				t.Fatal(err)
			}
			a, _ := st.AgentByID("g1")
			if a.Status != r.wantStatus {
				t.Errorf("status = %s, want %s", a.Status, r.wantStatus)
			}
			evs, _ := st.EventsByKind("g1", r.wantEvent, 1)
			if len(evs) != 1 {
				t.Fatalf("no %s event recorded", r.wantEvent)
			}
		})
	}
}

func TestNeutralToolNameAndInput(t *testing.T) {
	_, p, _, _ := Adapt("tool", Payload{Tool: "write", ToolInput: json.RawMessage(`{"path":"x"}`)})
	// tool names and the target key are normalized to the ones the lane guard knows
	var in map[string]string
	_ = json.Unmarshal(p.ToolInput, &in)
	if p.ToolName != "Write" || in["file_path"] != "x" {
		t.Fatalf("payload = %+v", p)
	}
}

func TestAdaptDistinguishesNeutralFromClaude(t *testing.T) {
	// Claude's own Stop hook: no event field, hook_event_name only
	ev, _, neutral, _ := Adapt("stop", Payload{HookEventName: "Stop"})
	if ev != "stop" || neutral {
		t.Errorf("claude stop = %s neutral=%v", ev, neutral)
	}
	ev, _, neutral, _ = Adapt("STOP", Payload{Event: "stop"})
	if ev != "stop" || !neutral {
		t.Errorf("neutral stop = %s neutral=%v", ev, neutral)
	}
	// event from the payload alone
	ev, _, neutral, _ = Adapt("", Payload{Event: "prompt"})
	if ev != "userpromptsubmit" || !neutral {
		t.Errorf("payload-only event = %s neutral=%v", ev, neutral)
	}
	// event from Claude's hook_event_name alone, as before
	ev, _, neutral, _ = Adapt("", Payload{HookEventName: "PreToolUse"})
	if ev != "pretooluse" || neutral {
		t.Errorf("claude fallback = %s neutral=%v", ev, neutral)
	}
	if _, _, _, err := Adapt("tool", Payload{Event: "bogus"}); err != nil {
		t.Errorf("the command-line event wins over a conflicting payload event: %v", err)
	}
	if _, _, _, err := Adapt("", Payload{Event: "bogus"}); err == nil {
		t.Error("unknown neutral event accepted")
	}
}

func TestNeutralUsageIsAddedAsADelta(t *testing.T) {
	st := neutralStore(t)
	for i := 0; i < 2; i++ {
		_, p, _, _ := Adapt("stop", Payload{Event: "stop", Usage: &NeutralUsage{Prompt: 1000, Output: 50, Cost: 0.01}})
		res, err := Handle(st, "g1", "stop", p, Options{})
		if err != nil || !res.UsageOK || res.Usage.Prompt != 1000 {
			t.Fatalf("handle: %+v %v", res, err)
		}
	}
	if got, _ := st.AgentSpendToday("g1"); got < 0.0199 || got > 0.0201 {
		t.Fatalf("spend = %v, want two deltas of 0.01", got)
	}
	// negative or empty usage adds nothing
	_, p, _, _ := Adapt("tool", Payload{Event: "tool", Usage: &NeutralUsage{Prompt: -5}})
	if res, _ := Handle(st, "g1", "pretooluse", p, Options{}); res.UsageOK {
		t.Error("empty usage recorded")
	}
}

func TestSaveSessionID(t *testing.T) {
	d := t.TempDir()
	if err := SaveSessionID(d, "g1", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(d, ".slai", "run", "g1", "session-id")); err == nil {
		t.Fatal("wrote an empty id")
	}
	_ = SaveSessionID(d, "g1", "ses_1")
	_ = SaveSessionID(d, "g1", "ses_2") // a restart under a new id replaces it
	b, _ := os.ReadFile(filepath.Join(d, ".slai", "run", "g1", "session-id"))
	if string(b) != "ses_2\n" {
		t.Fatalf("session-id = %q", b)
	}
}

func TestNeutralDecisionJSON(t *testing.T) {
	var m map[string]string
	if err := json.Unmarshal(Decision{Deny: true, Reason: "outside your lane"}.NeutralJSON(), &m); err != nil || m["decision"] != "deny" || m["reason"] != "outside your lane" {
		t.Fatalf("%v %v", m, err)
	}
}

// A plugin names tools in lower case and keys the target differently; the lane
// guard must still judge them (an unrecognised tool name would be allowed silently).
func TestNeutralToolCallsAreCheckedAgainstTheLane(t *testing.T) {
	proj := t.TempDir()
	wt := filepath.Join(proj, ".slai", "worktrees", "g1")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	st := neutralStore(t)
	_ = st.UpsertAgent(store.Agent{ID: "g1", Role: "engineer", Worktree: wt})
	_ = st.UpsertAgent(store.Agent{ID: "boss", Role: "orchestrator"})
	cases := []struct {
		name  string
		agent string
		tool  string
		input string
		deny  bool
	}{
		{"opencode write inside the worktree", "g1", "write", `{"filePath":"` + wt + `/a.txt","content":"x"}`, false},
		{"opencode write outside", "g1", "write", `{"filePath":"` + proj + `/main.go","content":"x"}`, true},
		{"pi edit outside (path key)", "g1", "edit", `{"path":"/etc/hosts","oldText":"a","newText":"b"}`, true},
		{"pi edit inside", "g1", "edit", `{"path":"` + wt + `/b.go","oldText":"a","newText":"b"}`, false},
		{"write with no target path", "g1", "write", `{"content":"x"}`, true},
		{"patch tools name files inside the patch", "g1", "patch", `{"patchText":"*** Update File: /etc/passwd"}`, true},
		{"bash redirect outside", "g1", "bash", `{"command":"echo x > ` + proj + `/main.go"}`, true},
		{"bash inside", "g1", "bash", `{"command":"go test ./..."}`, false},
		{"read-only role cannot write anywhere", "boss", "write", `{"filePath":"` + wt + `/a.txt"}`, true},
		{"reads are never blocked", "g1", "read", `{"filePath":"/etc/hosts"}`, false},
		{"capitalised names still work", "g1", "Write", `{"file_path":"/etc/passwd"}`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, p, neutral, err := Adapt("tool", Payload{Event: "tool", Tool: c.tool, ToolInput: json.RawMessage(c.input), Cwd: wt})
			if err != nil || !neutral {
				t.Fatalf("Adapt: %v", err)
			}
			res, err := Handle(st, c.agent, "pretooluse", p, Options{ProjectDir: proj})
			if err != nil {
				t.Fatal(err)
			}
			if got := res.Decision != nil && res.Decision.Deny; got != c.deny {
				t.Fatalf("deny = %v, want %v (%+v)", got, c.deny, res.Decision)
			}
			if c.deny {
				evs, _ := st.EventsByKind(c.agent, "lane_denied", 1)
				if len(evs) != 1 || !strings.Contains(evs[0].Data, `"tool"`) {
					t.Fatalf("no lane_denied event: %+v", evs)
				}
			}
		})
	}
}
