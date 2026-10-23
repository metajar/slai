package harness

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/claude"
)

func TestForCoversEveryKindAndRejectsUnknown(t *testing.T) {
	if got := Kinds(); len(got) != 3 || got[0] != Claude {
		t.Fatalf("Kinds = %v", got)
	}
	for _, k := range Kinds() {
		h, err := For(k)
		if err != nil || h.Kind() != k {
			t.Fatalf("For(%s) = %v, %v", k, h, err)
		}
	}
	if h, err := For(""); err != nil || h.Kind() != Claude {
		t.Fatalf("empty kind should be claude: %v %v", h, err)
	}
	_, err := For("codex")
	if err == nil || !strings.Contains(err.Error(), `unknown harness "codex"`) || !strings.Contains(err.Error(), "claude, opencode, pi") {
		t.Fatalf("unknown kind error = %v", err)
	}
}

// The Claude adapter must produce exactly the command line package claude always
// has: these goldens were captured before the harness package existed.
func TestClaudeCommandIsByteIdentical(t *testing.T) {
	h, _ := For(Claude)
	ls := LaunchSpec{AgentID: "eng-1", Model: "sonnet", PermissionMode: "manual", ProjectDir: "/p", WorkDir: "/p/.slai/worktrees/eng-1", SystemPrompt: "You are **eng-1**."}
	const tail = ` --append-system-prompt "$(cat '/p/.slai/run/eng-1/system.md')" --settings '/p/.slai/run/eng-1/settings.json' --permission-mode 'manual' --mcp-config '/p/.slai/run/eng-1/mcp.json'`
	const sid = "11111111-2222-4333-8444-555555555555"
	fresh := func(flag, id string) string {
		s := "claude --model 'sonnet'"
		if id != "" {
			s += " " + flag + " '" + id + "'"
		}
		return s + tail
	}
	if got, want := h.Command(ls), fresh("", ""); got != want {
		t.Errorf("no session:\n got %s\nwant %s", got, want)
	}
	ls.SessionID = sid
	if got, want := h.Command(ls), fresh("--session-id", sid); got != want {
		t.Errorf("first launch:\n got %s\nwant %s", got, want)
	}
	ls.Resume = true
	got := h.Command(ls)
	parts := strings.Split(got, " || ")
	if len(parts) != 3 || parts[0] != fresh("--resume", sid) || parts[1] != fresh("--session-id", sid) ||
		!strings.HasPrefix(parts[2], "claude --model 'sonnet' --session-id '") || !strings.HasSuffix(parts[2], tail) {
		t.Errorf("resume:\n%s", got)
	}
	// and it is the same function as before
	old := claude.Spec{AgentID: "eng-1", Model: "sonnet", PermissionMode: "manual", ProjectDir: "/p", WorkDir: "/p/.slai/worktrees/eng-1", SystemPrompt: "You are **eng-1**.", SessionID: sid}
	ls.Resume = false
	if h.Command(ls) != old.Command() {
		t.Error("adapter differs from claude.Spec.Command")
	}
}

func TestClaudeRunFilesAreUnchanged(t *testing.T) {
	d := t.TempDir()
	h, _ := For(Claude)
	ls := LaunchSpec{AgentID: "orch", Model: "opus", PermissionMode: "auto", ProjectDir: d, WorkDir: d, SlaiBin: "slai", SystemPrompt: "You are **orch**.", Deny: []string{"Bash(git push:*)"}}
	if err := h.WriteRunFiles(ls); err != nil {
		t.Fatal(err)
	}
	viaHarness := map[string]string{}
	for _, f := range []string{"mcp.json", "settings.json", "system.md"} {
		b, err := os.ReadFile(filepath.Join(claude.RunDir(d, "orch"), f))
		if err != nil {
			t.Fatal(err)
		}
		viaHarness[f] = string(b)
	}
	d2 := t.TempDir()
	old := claude.Spec{AgentID: "orch", Model: "opus", PermissionMode: "auto", ProjectDir: d2, WorkDir: d2, SlaiBin: "slai", SystemPrompt: "You are **orch**.", Deny: []string{"Bash(git push:*)"}}
	if err := old.WriteRunFiles(); err != nil {
		t.Fatal(err)
	}
	for f, got := range viaHarness {
		b, _ := os.ReadFile(filepath.Join(claude.RunDir(d2, "orch"), f))
		if got != strings.ReplaceAll(string(b), d2, d) {
			t.Errorf("%s differs through the harness", f)
		}
	}
}

func TestFeaturesAndDegraded(t *testing.T) {
	want := map[Kind][]string{
		Claude:   nil,
		OpenCode: {"statusline", "resume_by_id"},
		Pi:       {"statusline"},
	}
	for k, w := range want {
		h, _ := For(k)
		if got := h.Features().Degraded(); strings.Join(got, ",") != strings.Join(w, ",") {
			t.Errorf("%s degraded = %v, want %v", k, got, w)
		}
	}
	pi, _ := For(Pi)
	if pi.Features().PermissionPrompts {
		t.Error("pi never asks for permission")
	}
	if pi.Features().TrustGate {
		t.Error("pi launches with --no-approve: there is no trust dialog to gate on")
	}
	cl, _ := For(Claude)
	if f := cl.Features(); !f.PermissionPrompts || !f.PromptSubmitEvent || !f.TrustGate || !f.Statusline {
		t.Errorf("claude features = %+v", f)
	}
	if cl.PaneRules().Modal == nil || cl.Keys().EnterDelay == 0 || len(cl.Keys().ClearSeq) != 2 {
		t.Errorf("claude rules/keys incomplete")
	}
}

func TestSessionIDPersistenceIsHarnessNeutral(t *testing.T) {
	d := t.TempDir()
	cl, _ := For(Claude)
	a, existed, err := LoadOrCreateSessionID(cl, d, "orch")
	if err != nil || existed || len(a) != 36 {
		t.Fatalf("first: %q %v %v", a, existed, err)
	}
	b, existed, _ := LoadOrCreateSessionID(cl, d, "orch")
	if b != a || !existed {
		t.Fatalf("not stable: %q %q %v", a, b, existed)
	}
	// a harness that assigns its own ids stores nothing until one is captured
	oc, _ := For(OpenCode)
	id, existed, err := LoadOrCreateSessionID(oc, d, "oc-1")
	if err != nil || id != "" || existed {
		t.Fatalf("opencode: %q %v %v", id, existed, err)
	}
	if _, err := os.Stat(SessionIDPath(d, "oc-1")); err == nil {
		t.Fatal("wrote an empty session id")
	}
	_ = os.MkdirAll(filepath.Dir(SessionIDPath(d, "oc-1")), 0o755)
	_ = os.WriteFile(SessionIDPath(d, "oc-1"), []byte("ses_abc\n"), 0o644)
	if id, existed, _ := LoadOrCreateSessionID(oc, d, "oc-1"); id != "ses_abc" || !existed {
		t.Fatalf("captured id not returned: %q %v", id, existed)
	}
}

// AC 7: a wrong ambient SLAI_AGENT_ID never leaks into an agent's run files or
// command line. Adapters that have not landed yet are skipped; B3/B4 switch this on.
func TestIdentityIsPinnedDespiteAmbientEnv(t *testing.T) {
	t.Setenv("SLAI_AGENT_ID", "intruder")
	t.Setenv("SLAI_PROJECT", "/somewhere/else")
	for _, k := range Kinds() {
		t.Run(string(k), func(t *testing.T) {
			h, _ := For(k)
			d := t.TempDir()
			model := "sonnet"
			if k != Claude {
				model = "zai/glm-5.3"
			}
			ls := LaunchSpec{AgentID: "eng-1", Model: model, PermissionMode: "manual", ProjectDir: d, WorkDir: d, SlaiBin: "slai", SystemPrompt: "You are **eng-1**.", SessionID: "11111111-2222-4333-8444-555555555555"}
			if err := h.WriteRunFiles(ls); err != nil {
				if errors.Is(err, ErrNotImplemented) {
					t.Skip("adapter not implemented yet")
				}
				t.Fatal(err)
			}
			if strings.Contains(h.Command(ls), "intruder") {
				t.Errorf("command line carries the ambient id: %s", h.Command(ls))
			}
			err := filepath.Walk(filepath.Join(d, ".slai", "run", "eng-1"), func(p string, fi os.FileInfo, err error) error {
				if err != nil || fi.IsDir() {
					return err
				}
				b, _ := os.ReadFile(p)
				if strings.Contains(string(b), "intruder") || strings.Contains(string(b), "/somewhere/else") {
					t.Errorf("%s carries the ambient identity", p)
				}
				if filepath.Base(p) == "mcp.json" && (!strings.Contains(string(b), `"--project"`) || !strings.Contains(string(b), d)) {
					t.Errorf("%s does not pin --project: %s", p, b)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
