package cmd

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/config"
)

func TestGoalHintNamesConfiguredOrchestrator(t *testing.T) {
	tm := &config.Team{Agents: []config.Agent{{ID: "eng-1", Role: config.RoleEngineer}, {ID: "boss", Role: config.RoleOrchestrator}}}
	got := goalHint(tm)
	if !strings.Contains(got, `slai send boss "<your goal>"`) || strings.Contains(got, "orch ") {
		t.Fatalf("hint = %q", got)
	}
}

func TestReadmeQuickstartIsShort(t *testing.T) {
	b, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)\n## Quickstart\n(.*?)\n## `).FindStringSubmatch(string(b))
	if m == nil {
		t.Fatal("README has no Quickstart section")
	}
	n := 0
	for _, l := range strings.Split(strings.TrimSpace(m[1]), "\n") {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	if n >= 10 {
		t.Fatalf("quickstart has %d non-blank lines, want < 10", n)
	}
}

func TestRootHelpPointsAtCommandTable(t *testing.T) {
	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), rootHelpFooter) || !strings.Contains(rootHelpFooter, "docs/commands.md") {
		t.Fatalf("root help lacks footer:\n%s", out.String())
	}
	var sub bytes.Buffer
	root = newRootCmd()
	root.SetOut(&sub)
	root.SetArgs([]string{"send", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sub.String(), rootHelpFooter) {
		t.Fatalf("subcommand help should not carry the root footer:\n%s", sub.String())
	}
}

// `slai up` ends with exactly one next step: the goal command. It must not also
// point at the dashboard.
func TestUpPrintsOneNextStep(t *testing.T) {
	b, err := os.ReadFile("daemon.go")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)func upCmd\(\).*?\nfunc goalHint`).Find(b)
	if m == nil {
		t.Fatal("upCmd not found")
	}
	if n := strings.Count(string(m), "goalHint(s.Team)"); n != 2 { // --foreground path and the detached path, one each
		t.Fatalf("upCmd prints the next step %d times, want once per path (2)", n)
	}
	if strings.Contains(string(m), "open the dashboard") {
		t.Fatal("slai up must not print a second next step pointing at the dashboard")
	}
	tm := &config.Team{Agents: []config.Agent{{ID: "orch", Role: config.RoleOrchestrator}}}
	if n := strings.Count(goalHint(tm), "next:"); n != 1 {
		t.Fatalf("goalHint has %d next lines", n)
	}
}
