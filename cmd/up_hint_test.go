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
