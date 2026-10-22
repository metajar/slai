package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The OpenCode and Pi pane fixtures were captured from the real TUIs (T-0025,
// opencode 1.18.16, pi 1.3.0, 200x50 tmux panes). The pane rules built on them
// (harness spec AC 12) are only as good as these files, so pin what each one is
// supposed to show.
func TestHarnessPaneFixturesShowWhatTheyClaim(t *testing.T) {
	cases := []struct {
		file string
		want []string // all must appear
		not  []string // none may appear
	}{
		{"opencode/idle-home.txt", []string{"Ask anything", "tab agents"}, []string{"esc interrupt", "Permission required"}},
		{"opencode/idle-after-turn.txt", []string{"Build · GLM-5.3", "ctrl+p commands"}, []string{"esc interrupt", "Permission required"}},
		{"opencode/working.txt", []string{"esc interrupt"}, []string{"Permission required"}},
		{"opencode/permission-dialog.txt", []string{"△ Permission required", "# Shell command", "Allow once", "Allow always", "Reject", "enter confirm"}, nil},
		{"opencode/permission-dialog-external-dir.txt", []string{"△ Permission required", "Access external directory", "Allow once", "Reject"}, nil},
		{"opencode/provider-error-retrying.txt", []string{"Insufficient balance or no resource package", "retrying in", "esc interrupt"}, nil},
		{"opencode/quoted-dialog-in-scrollback.txt", []string{"Permission required", "Allow once"}, []string{"esc interrupt"}},
		{"pi/idle-startup-extkeys-off.txt", []string{"extended-keys is off", "(auto)"}, []string{"Working"}},
		{"pi/idle-startup-extkeys-on.txt", []string{"(auto)"}, []string{"extended-keys is off", "Working"}},
		{"pi/working.txt", []string{"Working"}, nil},
		{"pi/idle-after-turn.txt", []string{"(auto)"}, []string{"Working"}},
		{"pi/trust-prompt.txt", []string{"Trust project folder?", "Do not trust", "escape/ctrl+c cancel"}, nil},
		{"pi/provider-error-badkey.txt", []string{"Error: 401"}, nil},
		{"pi/quoted-dialog-in-scrollback.txt", []string{"Permission required", "Allow once"}, []string{"Trust project folder?\n"}},
	}
	for _, c := range cases {
		b, err := os.ReadFile(filepath.Join("testdata", "panes", c.file))
		if err != nil {
			t.Errorf("%s: %v", c.file, err)
			continue
		}
		s := string(b)
		for _, w := range c.want {
			if !strings.Contains(s, w) {
				t.Errorf("%s lacks %q", c.file, w)
			}
		}
		for _, n := range c.not {
			if strings.Contains(s, n) {
				t.Errorf("%s must not contain %q", c.file, n)
			}
		}
	}
}

// The negative case: in the quoted OpenCode pane the dialog words sit above the
// input box (history), whereas in the real dialog they replace the input box, so
// a detector must look at the live bottom region only (T-0013).
func TestOpenCodeQuotedDialogIsAboveTheInputBox(t *testing.T) {
	line := func(file, marker string) int {
		b, err := os.ReadFile(filepath.Join("testdata", "panes", "opencode", file))
		if err != nil {
			t.Fatal(err)
		}
		last := -1
		for i, l := range strings.Split(string(b), "\n") {
			if strings.Contains(l, marker) {
				last = i
			}
		}
		return last
	}
	quoted, box := line("quoted-dialog-in-scrollback.txt", "Permission required"), line("quoted-dialog-in-scrollback.txt", "Build · GLM-5.3 Zhipu")
	if quoted < 0 || box < 0 || quoted >= box {
		t.Fatalf("quoted dialog at line %d must be above the input box footer at line %d", quoted, box)
	}
	real, realBox := line("permission-dialog.txt", "Allow once"), line("permission-dialog.txt", "Build · GLM-5.3 Zhipu")
	if real < 0 || realBox >= 0 {
		t.Fatalf("a real dialog replaces the input box: dialog line %d, input footer line %d", real, realBox)
	}
}
