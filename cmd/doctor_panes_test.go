package cmd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/tmux"
)

// AC 3: `slai doctor --panes` prints, per live agent, the detected status and the
// matched rule. The windows are real tmux panes showing real captured fixtures.
func TestDoctorPanesReportsDetectedStatusAndRule(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	team := config.Preset("squad", "p", "", "main")
	for i := range team.Agents {
		switch team.Agents[i].ID {
		case "eng-1":
			team.Agents[i].Harness, team.Agents[i].Model = config.HarnessPi, "zai/glm-5.3"
		case "eng-2":
			team.Agents[i].Harness, team.Agents[i].Model = config.HarnessOpenCode, "zhipuai-coding-plan/glm-5.3"
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, ".slai"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := config.Write(filepath.Join(dir, ".slai", "team.yaml"), team); err != nil {
		t.Fatal(err)
	}
	sock := fmt.Sprintf("slai-doctor-panes-%d", time.Now().UnixNano()%1e9)
	tm := tmux.New(sock)
	t.Cleanup(func() { _ = tm.KillServer(context.Background()); tmux.RemoveSocket(sock) })
	panes := filepath.Join("..", "internal", "hooks", "testdata", "panes")
	show := map[string]string{ // agent -> fixture its window displays
		"orch":      filepath.Join(panes, "claude", "permission-dialog.txt"),
		"pm":        filepath.Join(panes, "claude", "idle-prompt.txt"),
		"eng-1":     filepath.Join(panes, "pi", "working.txt"),
		"eng-2":     filepath.Join(panes, "opencode", "permission-dialog.txt"),
		"principal": filepath.Join(panes, "claude", "quoted-dialog-in-scrollback.txt"),
		// fg-* agents are left not running
	}
	for id, file := range show {
		abs, _ := filepath.Abs(file)
		if err := tm.NewSession(context.Background(), tmux.SessionName(dir, id), dir, nil, "cat '"+abs+"'; sleep 120"); err != nil {
			t.Fatal(err)
		}
	}
	// wait for the fixtures to be drawn
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ready := true
		for id := range show {
			p, _ := tm.CapturePane(context.Background(), tmux.SessionName(dir, id), false, 0)
			ready = ready && strings.TrimSpace(p) != ""
		}
		if ready {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	var out bytes.Buffer
	if err := doctorPanes(context.Background(), dir, tm, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	line := func(id string) string {
		for _, l := range strings.Split(got, "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), id+" ") {
				return l
			}
		}
		t.Fatalf("no line for %s in:\n%s", id, got)
		return ""
	}
	for id, want := range map[string][]string{
		"orch":      {"claude", "pane: waiting", "rule: claude.modal"},
		"pm":        {"claude", "pane: idle", "rule: claude.idle"},
		"eng-1":     {"pi", "pane: working", "rule: pi.working"},
		"eng-2":     {"opencode", "pane: waiting", "rule: opencode.modal"},
		"principal": {"claude", "pane: idle", "rule: claude.idle"}, // a quoted dialog is not a prompt
	} {
		l := line(id)
		for _, w := range want {
			if !strings.Contains(l, w) {
				t.Errorf("%s: %q lacks %q", id, l, w)
			}
		}
	}
	if l := line("fg-newbie"); !strings.Contains(l, "not running") {
		t.Errorf("fg-newbie: %q", l)
	}
	if !strings.Contains(got, `matched: "`) {
		t.Errorf("no matched excerpt printed:\n%s", got)
	}
	// the dialog agents show an excerpt of the prompt itself
	if !strings.Contains(got, "Do you want to proceed?") || !strings.Contains(got, "Permission required") || strings.Contains(got, "╌") {
		t.Errorf("modal excerpts missing:\n%s", got)
	}
}

func TestDoctorPanesFlagIsRegistered(t *testing.T) {
	if f := doctorCmd().Flags().Lookup("panes"); f == nil {
		t.Fatal("slai doctor has no --panes flag")
	}
}
