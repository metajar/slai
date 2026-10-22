package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/config"
)

func teamWith(principalModel, engineerModel, engineer2Model string) *config.Team {
	t := config.Preset("", "app", "d", "main")
	t.Agents = []config.Agent{
		{ID: "orch", Role: config.RoleOrchestrator, Model: "opus"},
		{ID: "principal", Role: config.RolePrincipal, Model: principalModel},
		{ID: "eng-1", Role: config.RoleEngineer, Model: engineerModel},
		{ID: "eng-2", Role: config.RoleEngineer, Model: engineer2Model},
	}
	return t
}

// AC 1 (init and doctor share the helper) and AC 2.
func TestSameReviewerLine(t *testing.T) {
	same := teamWith("sonnet", "sonnet", "sonnet")
	if l := sameReviewerLine(same); !strings.Contains(l, "same model and harness as every engineer") || !strings.HasPrefix(l, "- info:") {
		t.Fatalf("line = %q", l)
	}
	if sameReviewerLine(teamWith("opus", "sonnet", "sonnet")) != "" {
		t.Fatal("a different principal model must suppress the line")
	}
	if sameReviewerLine(teamWith("sonnet", "sonnet", "haiku")) != "" {
		t.Fatal("any engineer differing must suppress the line")
	}
	harness := teamWith("sonnet", "sonnet", "sonnet")
	harness.Agents[3].Harness = config.HarnessOpenCode
	if sameReviewerLine(harness) != "" {
		t.Fatal("a differing harness must suppress the line")
	}
	noEng := teamWith("sonnet", "sonnet", "sonnet")
	noEng.Agents = noEng.Agents[:2]
	if sameReviewerLine(noEng) != "" {
		t.Fatal("no engineers: nothing to compare")
	}
}

// AC 1 and 3: doctor prints the line, and printing it never changes the failure count.
func TestDoctorPrintsSameReviewerLineWithoutChangingTheOutcome(t *testing.T) {
	run := func(tm *config.Team) (string, int) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, ".slai"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := config.Write(filepath.Join(dir, ".slai", "team.yaml"), tm); err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		bad := projectChecks(context.Background(), dir, false, &b)
		return b.String(), bad
	}
	withOut, withBad := run(teamWith("sonnet", "sonnet", "sonnet"))
	withoutOut, withoutBad := run(teamWith("opus", "sonnet", "sonnet"))
	if !strings.Contains(withOut, "- info: the principal runs the same model") {
		t.Fatalf("doctor output lacks the line:\n%s", withOut)
	}
	if strings.Contains(withoutOut, "same model and harness") {
		t.Fatalf("doctor printed the line for a differing principal:\n%s", withoutOut)
	}
	if withBad != withoutBad {
		t.Fatalf("the info line changed doctor's failure count: %d vs %d", withBad, withoutBad)
	}
	if strings.Contains(withOut, "! warn: the principal") || strings.Contains(withOut, "✗ the principal") {
		t.Fatal("the line must be informational, not a warning or failure")
	}
}
