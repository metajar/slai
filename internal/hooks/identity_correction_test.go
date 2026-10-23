package hooks

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/store"
)

// C6 signal (a): a lane denial records an identity_correction event and queues the agent's
// identity line plus a whoami instruction as an inbox item from slai.
func TestLaneDenialQueuesIdentityCorrection(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	proj := t.TempDir()
	wt := filepath.Join(proj, ".slai", "worktrees", "eng-1")
	_ = st.UpsertAgent(store.Agent{ID: "eng-1", Role: "engineer", Worktree: wt, Status: "idle"})
	opt := Options{ProjectDir: proj, IdentityLine: func(id string) string { return "You are " + id + ", the engineer of this team." }}
	deny := func() {
		t.Helper()
		p := Payload{ToolName: "Write", ToolInput: json.RawMessage(`{"file_path":"/etc/passwd","content":"x"}`), Cwd: wt}
		if res, err := Handle(st, "eng-1", "pretooluse", p, opt); err != nil || res.Decision == nil || !res.Decision.Deny {
			t.Fatalf("not denied: %v %+v", err, res.Decision)
		}
	}
	deny()
	evs, _ := st.EventsByKind("eng-1", "identity_correction", 5)
	if len(evs) != 1 || !strings.Contains(evs[0].Data, `"signal":"lane_denial"`) || !strings.Contains(evs[0].Data, `"queued":true`) {
		t.Fatalf("events = %+v", evs)
	}
	ms, _ := st.UndeliveredFor("eng-1")
	if len(ms) != 1 || ms[0].From != "slai" || ms[0].Kind != store.IdentityMessageKind ||
		!strings.HasPrefix(ms[0].Body, "You are eng-1, the engineer of this team.") || !strings.Contains(ms[0].Body, "whoami") {
		t.Fatalf("queued = %+v", ms)
	}
	// a second denial moments later is recorded but does not queue a second line
	deny()
	evs, _ = st.EventsByKind("eng-1", "identity_correction", 5)
	if len(evs) != 2 || !strings.Contains(evs[0].Data, `"queued":false`) {
		t.Fatalf("second denial events = %+v", evs)
	}
	if ms, _ := st.UndeliveredFor("eng-1"); len(ms) != 1 {
		t.Fatalf("queued %d identity items, want 1", len(ms))
	}
}

// C7: the lane guard tells a blocked orchestrator who it is and how to delegate.
func TestOrchestratorLaneDenialSaysWhoItIs(t *testing.T) {
	d := CheckLane(Lane{Role: "orchestrator", ProjectDir: "/p"}, "Write", json.RawMessage(`{"file_path":"/p/main.go"}`))
	if d == nil || !d.Deny || !strings.Contains(d.Reason, "you are the orchestrator") || !strings.Contains(d.Reason, "create_task") {
		t.Fatalf("decision = %+v", d)
	}
}
