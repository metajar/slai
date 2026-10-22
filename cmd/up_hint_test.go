package cmd

import (
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
