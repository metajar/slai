package harness

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/roles"
)

const identitySID = "11111111-2222-4333-8444-555555555555"

// identitySpec is a launch for eng-1 with the real rendered role prompt.
func identitySpec(t *testing.T) LaunchSpec {
	t.Helper()
	dir := t.TempDir()
	team := &config.Team{Agents: []config.Agent{
		{ID: "orch", Role: config.RoleOrchestrator}, {ID: "eng-1", Role: config.RoleEngineer, Focus: "backend"}}}
	prompt, err := roles.Render(team, team.Agents[1], dir)
	if err != nil {
		t.Fatal(err)
	}
	return LaunchSpec{AgentID: "eng-1", Model: "sonnet", PermissionMode: "manual", ProjectDir: dir,
		WorkDir: filepath.Join(dir, ".slai", "worktrees", "eng-1"), SystemPrompt: prompt, SessionID: identitySID}
}

// chain splits a command into its `||` alternatives (claude revive and opencode fall back).
func chain(cmd string) []string { return strings.Split(cmd, " || ") }

// C4: for every harness the role prompt, which names the agent and its role, is passed on the
// system-prompt channel on a fresh launch and on a resume, in every fallback alternative.
func TestIdentityIsOnTheSystemPromptChannelForEveryHarness(t *testing.T) {
	for _, k := range Kinds() {
		for _, resume := range []bool{false, true} {
			t.Run(string(k)+map[bool]string{false: "/fresh", true: "/resume"}[resume], func(t *testing.T) {
				h, _ := For(k)
				s := identitySpec(t)
				s.Resume = resume
				if !strings.Contains(s.SystemPrompt, "**eng-1**") || !strings.Contains(s.SystemPrompt, "role: Engineer") {
					t.Fatalf("the rendered prompt does not carry id and role:\n%.200s", s.SystemPrompt)
				}
				if err := h.WriteRunFiles(s); err != nil {
					t.Fatal(err)
				}
				file := filepath.Join(s.ProjectDir, ".slai", "run", "eng-1", "system.md")
				if b, err := os.ReadFile(file); err != nil || string(b) != s.SystemPrompt {
					t.Fatalf("system.md = %q, %v", b, err)
				}
				cmds := chain(h.Command(s))
				switch k {
				case Claude, Pi:
					want := `--append-system-prompt "$(cat '` + file + `')"`
					for _, c := range cmds {
						if !strings.Contains(c, want) {
							t.Errorf("alternative lacks %s:\n%s", want, c)
						}
					}
				case OpenCode:
					cfg := readJSON(t, filepath.Join(s.ProjectDir, ".slai", "run", "eng-1", "opencode.json"))
					name := openCodeAgentName("eng-1")
					agents := cfg["agent"].(map[string]any)
					if got := agents[name].(map[string]any)["prompt"]; got != s.SystemPrompt {
						t.Errorf("opencode agent prompt = %v", got)
					}
					if cfg["default_agent"] != name {
						t.Errorf("default_agent = %v, want %s", cfg["default_agent"], name)
					}
					for _, c := range cmds {
						if !strings.Contains(c, "--agent '"+name+"'") {
							t.Errorf("alternative does not select the agent: %s", c)
						}
					}
				}
			})
		}
	}
}

// C4 / (e): a missing, empty or foreign role prompt refuses the launch with an error naming
// the agent and the file, and leaves no system.md behind.
func TestEmptyOrForeignPromptRefusesLaunch(t *testing.T) {
	for _, k := range Kinds() {
		for name, prompt := range map[string]string{"empty": "", "blank": " \n\t\n", "foreign": "You are **orch**, role: Orchestrator."} {
			t.Run(string(k)+"/"+name, func(t *testing.T) {
				h, _ := For(k)
				s := identitySpec(t)
				s.SystemPrompt = prompt
				err := h.WriteRunFiles(s)
				if !errors.Is(err, ErrNoIdentity) {
					t.Fatalf("err = %v, want ErrNoIdentity", err)
				}
				file := filepath.Join(s.ProjectDir, ".slai", "run", "eng-1", "system.md")
				if !strings.Contains(err.Error(), "eng-1") || !strings.Contains(err.Error(), file) {
					t.Errorf("error should name the agent and file: %v", err)
				}
				if _, statErr := os.Stat(file); statErr == nil {
					t.Error("system.md was written for a refused launch")
				}
			})
		}
	}
}

// No launch or revive path may resume "the latest" conversation of the directory: that is how an
// orchestrator became another agent. Sessions are only ever selected by the agent's own id.
func TestNoLaunchPathResumesTheLatestConversation(t *testing.T) {
	bad := []string{"--continue", " -c ", "--last", " --fork"}
	for _, k := range Kinds() {
		h, _ := For(k)
		for _, sid := range []string{"", identitySID} {
			for _, resume := range []bool{false, true} {
				s := identitySpec(t)
				s.SessionID, s.Resume = sid, resume
				cmd := h.Command(s)
				for _, b := range bad {
					if strings.Contains(cmd+" ", b) {
						t.Errorf("%s (session %q, resume %v) contains %q:\n%s", k, sid, resume, b, cmd)
					}
				}
				// every resume/session flag is followed by an explicit quoted id
				for _, m := range regexp.MustCompile(`(--resume|--session-id| -s)( \S*)?`).FindAllStringSubmatch(cmd, -1) {
					if !strings.HasPrefix(m[2], " '") {
						t.Errorf("%s: %s without an explicit id in %s", k, m[1], cmd)
					}
				}
			}
		}
	}
}

// Regression at the source level: the string "--continue" must not appear in any non-test Go
// file that builds a harness command (comments excluded).
func TestNoContinueFlagInLaunchSources(t *testing.T) {
	for _, dir := range []string{".", "../claude"} {
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			code := stripGoComments(string(src))
			for _, lit := range []string{`"--continue"`, `"-c"`, `"--last"`, "--continue "} {
				if strings.Contains(code, lit) {
					t.Errorf("%s builds a %s flag", f, lit)
				}
			}
		}
	}
}

func stripGoComments(s string) string {
	s = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(s, "")
	return regexp.MustCompile(`(?m)//.*$`).ReplaceAllString(s, "")
}
