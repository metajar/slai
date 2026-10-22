package supervisor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/oorbt/slai/internal/config"
)

// Adding the harness field must not change the recorded hash of any existing
// (claude) agent, or every agent would restart on upgrade.
func TestConfigHashUnchangedForClaudeAgents(t *testing.T) {
	tm := team(5)
	a := tm.Agents[1]
	b, _ := json.Marshal([]string{a.Role, tm.ResolveModel(a.Model), a.Focus, a.Persona, tm.PermissionModeFor(a), ""})
	sum := sha256.Sum256(b)
	if want := hex.EncodeToString(sum[:8]); ConfigHash(tm, a, t.TempDir()) != want {
		t.Fatal("claude agent hash changed")
	}
	a.Harness = config.HarnessClaude
	if ConfigHash(tm, a, t.TempDir()) != hex.EncodeToString(sum[:8]) {
		t.Fatal("explicit harness: claude must hash like the default")
	}
}

func TestConfigHashChangesWithHarnessAndModel(t *testing.T) {
	tm := team(5)
	dir := t.TempDir()
	base := config.Agent{ID: "g1", Role: config.RoleEngineer, Model: "zai/glm-5.3", Harness: "pi"}
	h0 := ConfigHash(tm, base, dir)
	oc := base
	oc.Harness = "opencode"
	if ConfigHash(tm, oc, dir) == h0 {
		t.Error("changing harness: must restart the agent")
	}
	m := base
	m.Model = "zai/glm-5.3-flash"
	if ConfigHash(tm, m, dir) == h0 {
		t.Error("changing the model must restart the agent")
	}
	cl := base
	cl.Harness, cl.Model = "", "sonnet"
	if ConfigHash(tm, cl, dir) == h0 {
		t.Error("claude and pi hash the same")
	}
}
