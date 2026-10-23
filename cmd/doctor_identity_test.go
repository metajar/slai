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
	// C6 signal (iv): the mismatch also records an identity_correction for the pinned agent
	if evs, _ := st.EventsByKind("orch", "identity_correction", 3); len(evs) != 1 ||
		!strings.Contains(evs[0].Data, `"signal":"env_mismatch"`) || !strings.Contains(evs[0].Data, "named pm") {
		t.Errorf("identity_correction events = %+v", evs)
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

func TestDoctorIdentityRunChecks(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	run := func(id string) string { return filepath.Join(dir, ".slai", "run", id) }
	good := func(id string) {
		if err := (claude.Spec{AgentID: id, ProjectDir: dir, WorkDir: dir, SystemPrompt: "You are **" + id + "**."}).WriteRunFiles(); err != nil {
			t.Fatal(err)
		}
	}
	good("orch")
	good("eng-1")
	good("eng-2")
	live := map[string]bool{"orch": true, "eng-1": true, "eng-2": true, "pm": true}
	isLive := func(id string) bool { return live[id] }
	agents := []string{"orch", "eng-1", "eng-2", "pm", "never-started"}
	// pm runs with the pre-pinning format: no --agent, no env pin
	os.MkdirAll(run("pm"), 0o755)
	os.WriteFile(filepath.Join(run("pm"), "mcp.json"), []byte(`{"mcpServers":{"slai":{"command":"slai","args":["mcp"]}}}`), 0o644)
	os.WriteFile(filepath.Join(run("pm"), "settings.json"), []byte(`{"hooks":{}}`), 0o644)
	os.WriteFile(filepath.Join(run("pm"), "system.md"), []byte("You are pm."), 0o644)
	// eng-1's system.md is empty; eng-2's names someone else
	os.WriteFile(filepath.Join(run("eng-1"), "system.md"), []byte("  \n"), 0o644)
	os.WriteFile(filepath.Join(run("eng-2"), "system.md"), []byte("You are eng-9."), 0o644)

	var buf bytes.Buffer
	bad := identityRunChecks(&buf, st, dir, agents, isLive)
	out := buf.String()
	for _, want := range []string{
		"✗ pm is running with run files that do not pin its identity",
		"✗ eng-1's system.md is empty",
		"✗ eng-2's system.md does not name eng-2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if bad != 3 || strings.Contains(out, "orch") || strings.Contains(out, "never-started") {
		t.Errorf("bad = %d, want 3 (orch is fine, never-started has no run dir):\n%s", bad, out)
	}
	// the same unpinned files for an agent that is not running are only the existing warning
	live["pm"] = false
	buf.Reset()
	identityRunChecks(&buf, st, dir, []string{"pm"}, isLive)
	if strings.Contains(buf.String(), "is running with run files") {
		t.Errorf("a stopped agent flagged as running:\n%s", buf.String())
	}
	// a missing system.md for an agent that has run files
	os.Remove(filepath.Join(run("eng-1"), "system.md"))
	buf.Reset()
	if n := identityRunChecks(&buf, st, dir, []string{"eng-1"}, isLive); n != 1 || !strings.Contains(buf.String(), "✗ eng-1 has no system.md") {
		t.Errorf("missing system.md: %d\n%s", n, buf.String())
	}
}

func TestDoctorFlagsAgentsSharingASessionID(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, a := range []string{"orch", "eng-1", "eng-2"} {
		os.MkdirAll(filepath.Join(dir, ".slai", "run", a), 0o755)
	}
	os.WriteFile(filepath.Join(dir, ".slai", "run", "orch", "session-id"), []byte("aaaaaaaa-1111\n"), 0o644)
	os.WriteFile(filepath.Join(dir, ".slai", "run", "eng-1", "session-id"), []byte("bbbbbbbb-2222\n"), 0o644)
	var buf bytes.Buffer
	if n := sharedSessionChecks(&buf, st, dir, []string{"orch", "eng-1", "eng-2"}); n != 0 || buf.Len() != 0 {
		t.Fatalf("distinct ids flagged: %d %q", n, buf.String())
	}
	// two agents' SessionStart hooks report the same session id (how /tmp/minion's orch lost its identity)
	_ = st.AddEvent("orch", "hook:sessionstart", `{"session":"cccccccc-3333","source":"resume"}`)
	_ = st.AddEvent("eng-2", "hook:sessionstart", `{"session":"cccccccc-3333","source":"resume"}`)
	_ = st.AddEvent("eng-2", "hook:sessionstart", `{"session":"dddddddd-4444","source":"startup"}`)
	buf.Reset()
	n := sharedSessionChecks(&buf, st, dir, []string{"orch", "eng-1", "eng-2"})
	if n != 1 || !strings.Contains(buf.String(), "✗ eng-2 and orch share session id cccccccc…") {
		t.Fatalf("events: %d\n%s", n, buf.String())
	}
	// the same id in two session-id files
	os.WriteFile(filepath.Join(dir, ".slai", "run", "eng-1", "session-id"), []byte("aaaaaaaa-1111\n"), 0o644)
	buf.Reset()
	if n := sharedSessionChecks(&buf, st, dir, []string{"orch", "eng-1", "eng-2"}); n != 2 || !strings.Contains(buf.String(), "eng-1 and orch share session id aaaaaaaa…") {
		t.Fatalf("files: %d\n%s", n, buf.String())
	}
}
