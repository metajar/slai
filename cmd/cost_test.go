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
	if want := "Starting 3 Claude sessions. Spend caps: daily $25, per-agent $5"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if got := costNotice(costTeam(2.5, 0, "")); got != "Starting 1 Claude session. Spend caps: per-agent $2.5" {
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

func TestCostNoticeCapTable(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps config.SpendCaps
		want string
	}{
		{"none", config.SpendCaps{}, "Spend caps: OFF (set defaults.spend_caps in .slai/team.yaml)"},
		{"per-run only", config.SpendCaps{PerRunUSD: 3}, "Spend caps: per-run $3"},
		{"per-task only", config.SpendCaps{PerTaskUSD: 1.5}, "Spend caps: per-task $1.5"},
		{"all four", config.SpendCaps{DailyUSD: 120, PerAgentUSD: 40, PerTaskUSD: 10, PerRunUSD: 3}, "Spend caps: daily $120, per-agent $40, per-task $10, per-run $3"},
	} {
		team := costTeam(0, 0, "")
		team.Defaults.Caps = tc.caps
		for name, got := range map[string]string{"up": costNotice(team), "init": initCostNotice(team)} {
			if !strings.HasSuffix(got, tc.want) {
				t.Errorf("%s/%s: got %q, want suffix %q", tc.name, name, got, tc.want)
			}
		}
	}
}
