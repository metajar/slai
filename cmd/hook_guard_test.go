package cmd

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/hooks"
	"github.com/oorbt/slai/internal/store"
)

func decisionOf(t *testing.T, out string) (decision, reason string) {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &m); err != nil {
		t.Fatalf("not an explicit decision: %q (%v)", out, err)
	}
	return m["decision"], m["reason"]
}

// AC 10 (fail closed): every neutral `tool` event is answered explicitly, and any
// internal failure is a deny, never silence.
func TestNeutralToolEventIsAlwaysAnsweredExplicitly(t *testing.T) {
	proj, _ := neutralProject(t, "engineer")
	t.Setenv("SLAI_AGENT_ID", "")
	t.Setenv("SLAI_PROJECT", "")
	args := func(extra ...string) []string {
		return append([]string{"tool", "--agent", "g1", "--project", proj}, extra...)
	}

	t.Run("success is an explicit allow", func(t *testing.T) {
		d, _ := decisionOf(t, runHookCmd(t, `{"event":"tool","tool":"Read"}`, args()...))
		if d != "allow" {
			t.Fatalf("decision = %q", d)
		}
	})
	t.Run("a lane violation is a deny with the reason", func(t *testing.T) {
		d, r := decisionOf(t, runHookCmd(t, `{"event":"tool","tool":"Write","tool_input":{"file_path":"/etc/passwd"},"cwd":"/etc"}`, args()...))
		if d != "deny" || r == "" || strings.HasPrefix(r, "guard error") {
			t.Fatalf("decision %q reason %q", d, r)
		}
	})

	t.Run("a store that cannot be opened denies", func(t *testing.T) {
		d, r := decisionOf(t, runHookCmd(t, `{"event":"tool","tool":"Read"}`, "tool", "--agent", "g1", "--project", filepath.Join(proj, "no", "such", "dir")))
		if d != "deny" || !strings.HasPrefix(r, "guard error: ") {
			t.Fatalf("decision %q reason %q", d, r)
		}
	})
	t.Run("a panic denies", func(t *testing.T) {
		old := handleHook
		handleHook = func(*store.Store, string, string, hooks.Payload, hooks.Options) (hooks.Result, error) {
			panic("boom")
		}
		t.Cleanup(func() { handleHook = old })
		d, r := decisionOf(t, runHookCmd(t, `{"event":"tool","tool":"Read"}`, args()...))
		if d != "deny" || !strings.Contains(r, "panic: boom") {
			t.Fatalf("decision %q reason %q", d, r)
		}
	})
	t.Run("an error while handling denies", func(t *testing.T) {
		old := handleHook
		handleHook = func(*store.Store, string, string, hooks.Payload, hooks.Options) (hooks.Result, error) {
			return hooks.Result{}, errors.New("disk full")
		}
		t.Cleanup(func() { handleHook = old })
		d, r := decisionOf(t, runHookCmd(t, `{"event":"tool","tool":"Read"}`, args()...))
		if d != "deny" || !strings.Contains(r, "disk full") {
			t.Fatalf("decision %q reason %q", d, r)
		}
	})
	t.Run("an unreadable tool call denies", func(t *testing.T) {
		d, _ := decisionOf(t, runHookCmd(t, `not json`, args()...))
		if d != "deny" {
			t.Fatalf("decision = %q", d)
		}
	})
	t.Run("no identity denies", func(t *testing.T) {
		d, _ := decisionOf(t, runHookCmd(t, `{"event":"tool"}`, "tool"))
		if d != "deny" {
			t.Fatalf("decision = %q", d)
		}
	})
	t.Run("a hung hook denies", func(t *testing.T) {
		old, oldH := handleHook, hookTimeout
		handleHook = func(*store.Store, string, string, hooks.Payload, hooks.Options) (hooks.Result, error) {
			time.Sleep(2 * time.Second)
			return hooks.Result{}, nil
		}
		hookTimeout = 100 * time.Millisecond
		t.Cleanup(func() { handleHook, hookTimeout = old, oldH })
		d, r := decisionOf(t, runHookCmd(t, `{"event":"tool","tool":"Read"}`, args()...))
		if d != "deny" || !strings.Contains(r, "timed out") {
			t.Fatalf("decision %q reason %q", d, r)
		}
	})
}

// The other events, and Claude's own PreToolUse, keep their old contract: failures
// are silent and never block the agent.
func TestOtherHookEventsStayQuietOnFailure(t *testing.T) {
	proj, _ := neutralProject(t, "engineer")
	t.Setenv("SLAI_AGENT_ID", "")
	t.Setenv("SLAI_PROJECT", "")
	bad := filepath.Join(proj, "no", "such", "dir")
	for _, ev := range []string{"prompt", "stop", "pretooluse", "userpromptsubmit"} {
		if out := runHookCmd(t, `{}`, ev, "--agent", "g1", "--project", bad); out != "" {
			t.Errorf("%s printed %q on a failure", ev, out)
		}
	}
}
