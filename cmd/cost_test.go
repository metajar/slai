package cmd

import (
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/config"
)

func costTeam(per, day float64, harnesses ...string) *config.Team {
	t := &config.Team{}
	for i, h := range harnesses {
		t.Agents = append(t.Agents, config.Agent{ID: string(rune('a' + i)), Harness: h})
	}
	t.Defaults.Caps = config.SpendCaps{PerAgentUSD: per, DailyUSD: day}
	return t
}

func TestCostNoticeCapsOn(t *testing.T) {
	got := costNotice(costTeam(5, 25, "", "claude", "claude"))
	if want := "Starting 3 Claude sessions. Spend caps: per-agent $5 / daily $25"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if got := costNotice(costTeam(2.5, 0, "")); got != "Starting 1 Claude session. Spend caps: per-agent $2.5 / daily off" {
		t.Fatalf("got %q", got)
	}
}

func TestCostNoticeCapsOff(t *testing.T) {
	got := costNotice(costTeam(0, 0, "", "", "", "", "", "", ""))
	if want := "Starting 7 Claude sessions. Spend caps: OFF (set defaults.spend_caps in .slai/team.yaml)"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestCostNoticeMentionsOtherHarnesses(t *testing.T) {
	got := costNotice(costTeam(0, 0, "", "pi", "opencode"))
	if !strings.Contains(got, "1 Claude session + 2 on other harnesses") {
		t.Fatalf("got %q", got)
	}
}
