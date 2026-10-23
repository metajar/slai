package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/claude"
	"github.com/oorbt/slai/internal/store"
)

func TestResolveAgentFlagWinsOverEnv(t *testing.T) {
	for _, c := range []struct{ flag, env, id, mismatch string }{
		{"orch", "pm", "orch", "pm"},
		{"orch", "orch", "orch", ""},
		{"orch", "", "orch", ""},
		{"", "pm", "pm", ""},
		{"", "", "", ""},
	} {
		if id, mm := resolveAgent(c.flag, c.env); id != c.id || mm != c.mismatch {
			t.Errorf("resolveAgent(%q,%q) = %q,%q want %q,%q", c.flag, c.env, id, mm, c.id, c.mismatch)
		}
	}
	g := pinnedGetenv("orch", func(k string) string { return map[string]string{"SLAI_AGENT_ID": "pm", "SLAI_PROJECT": "/p"}[k] })
	if g("SLAI_AGENT_ID") != "orch" || g("SLAI_PROJECT") != "/p" {
		t.Error("pinnedGetenv must override only SLAI_AGENT_ID")
	}
}

func TestNoteMismatchLogsAndRecords(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, ".slai", "state.db"))
	if err != nil {
		os.MkdirAll(filepath.Join(dir, ".slai"), 0o755)
		if st, err = store.Open(filepath.Join(dir, ".slai", "state.db")); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()
	noteMismatch(dir, "orch", "pm")
	b, _ := os.ReadFile(filepath.Join(dir, ".slai", "run", "orch", "mcp.log"))
	if !strings.Contains(string(b), "inherited SLAI_AGENT_ID=pm") {
		t.Errorf("mcp.log = %q", b)
	}
	st, _ = store.Open(filepath.Join(dir, ".slai", "state.db"))
	defer st.Close()
	if v, ok, _ := st.GetKV("identity.mismatch.orch"); !ok || !strings.HasPrefix(v, "pm|") {
		t.Errorf("KV = %q %v", v, ok)
	}
}

func TestDoctorIdentityCheck(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	run := func(id string) string { return filepath.Join(dir, ".slai", "run", id) }
	// current format
	if err := (claude.Spec{AgentID: "orch", ProjectDir: dir, WorkDir: dir, SystemPrompt: "You are **orch**."}).WriteRunFiles(); err != nil {
		t.Fatal(err)
	}
	// old format: no --agent, no env
	os.MkdirAll(run("pm"), 0o755)
	os.WriteFile(filepath.Join(run("pm"), "mcp.json"), []byte(`{"mcpServers":{"slai":{"command":"slai","args":["mcp"]}}}`), 0o644)
	os.WriteFile(filepath.Join(run("pm"), "settings.json"), []byte(`{"hooks":{}}`), 0o644)
	// pinned to the wrong agent: orch's files copied into eng-1's directory
	os.MkdirAll(run("eng-1"), 0o755)
	for _, f := range []string{"mcp.json", "settings.json"} {
		b, _ := os.ReadFile(filepath.Join(run("orch"), f))
		os.WriteFile(filepath.Join(run("eng-1"), f), b, 0o644)
	}
	var buf bytes.Buffer
	bad := identityCheck(&buf, st, dir, []string{"orch", "pm", "eng-1", "never-started"})
	out := buf.String()
	if bad != 1 {
		t.Errorf("bad = %d (only the wrongly pinned agent fails)\n%s", bad, out)
	}
	for _, want := range []string{"! warn: pm run files predate identity pinning", "✗ eng-1 run files are pinned to another agent"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, not := range []string{"orch run files", "never-started"} {
		if strings.Contains(out, not) {
			t.Errorf("unexpected %q in:\n%s", not, out)
		}
	}
	_ = st.SetKV("identity.mismatch.orch", "pm|1700000000")
	buf.Reset()
	identityCheck(&buf, st, dir, []string{"orch"})
	if !strings.Contains(buf.String(), "inherited SLAI_AGENT_ID=pm and ignored it") {
		t.Errorf("mismatch KV not reported: %s", buf.String())
	}
}
