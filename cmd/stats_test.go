package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/oorbt/slai/internal/service"
)

func sampleStats() service.StatsReport {
	e1 := service.StatsRow{Agent: "eng-1", Role: "engineer", Harness: "claude", Model: "claude-sonnet-5-5",
		Tasks: 4, Merged: 2, Cancelled: 1, Reverted: 1, ReviewRounds: 3, Reworked: 1, MergeRefused: 1,
		MedianSeconds: 5400, SpendUSD: 3, RoundsPerMerged: 1.5, ReworkRate: 0.5, SpendPerMerged: 1.5}
	e2 := service.StatsRow{Agent: "eng-2", Role: "engineer", Harness: "opencode", Model: "zai-coding-plan/glm-4.6-long-model-name",
		Tasks: 2, Merged: 1, ReviewRounds: 3, Reworked: 1, MedianSeconds: 600, SpendUSD: 0.5,
		RoundsPerMerged: 3, ReworkRate: 1, SpendPerMerged: 0.5}
	e3 := service.StatsRow{Agent: "eng-3", Role: "engineer", Harness: "claude", Model: "claude-sonnet-5-5"}
	g1 := e1
	g1.Agent, g1.Role, g1.Agents = "", "", []string{"eng-1", "eng-3"}
	g2 := e2
	g2.Agent, g2.Role, g2.Agents = "", "", []string{"eng-2"}
	return service.StatsReport{Since: 1000000, Agents: []service.StatsRow{e1, e2, e3}, Groups: []service.StatsRow{g1, g2}}
}

// AC3: the JSON shape is a contract (the TUI reads it); change it only by adding fields.
func TestStatsJSONGolden(t *testing.T) {
	rep := sampleStats()
	rep.Agents = rep.Agents[1:2]
	rep.Groups = rep.Groups[1:]
	var b bytes.Buffer
	if err := writeStats(&b, rep, true); err != nil {
		t.Fatal(err)
	}
	const want = `{
  "since": 1000000,
  "agents": [
    {
      "agent": "eng-2",
      "role": "engineer",
      "harness": "opencode",
      "model": "zai-coding-plan/glm-4.6-long-model-name",
      "tasks": 2,
      "merged": 1,
      "cancelled": 0,
      "reverted": 0,
      "review_rounds": 3,
      "reworked": 1,
      "merge_refused": 0,
      "median_assigned_to_merged_seconds": 600,
      "spend_usd": 0.5,
      "rounds_per_merged": 3,
      "rework_rate": 1,
      "spend_per_merged_usd": 0.5
    }
  ],
  "groups": [
    {
      "agents": [
        "eng-2"
      ],
      "harness": "opencode",
      "model": "zai-coding-plan/glm-4.6-long-model-name",
      "tasks": 2,
      "merged": 1,
      "cancelled": 0,
      "reverted": 0,
      "review_rounds": 3,
      "reworked": 1,
      "merge_refused": 0,
      "median_assigned_to_merged_seconds": 600,
      "spend_usd": 0.5,
      "rounds_per_merged": 3,
      "rework_rate": 1,
      "spend_per_merged_usd": 0.5
    }
  ]
}
`
	if b.String() != want {
		t.Fatalf("JSON changed:\n%s", b.String())
	}
}

// AC3: the text tables are compact and aligned, within 100 columns even for long model names.
func TestStatsTextWithin100Columns(t *testing.T) {
	var b bytes.Buffer
	if err := writeStats(&b, sampleStats(), false); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, l := range strings.Split(out, "\n") {
		if n := utf8.RuneCountInString(l); n > 100 {
			t.Errorf("%d columns: %q", n, l)
		}
	}
	for _, want := range []string{"by agent:", "by harness/model:", "eng-3", "claude/claude-sonnet-5-5",
		"1/1", "1.5", "50%", "$1.50", "1h30m", "10m"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// the zero-task agent is a row of zeros, with dashes for the ratios
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "eng-3") && !strings.Contains(l, "$0.00") {
			t.Errorf("eng-3 row: %q", l)
		}
	}
}

func TestParseSince(t *testing.T) {
	now := time.Unix(10_000_000, 0)
	for in, want := range map[string]time.Duration{"7d": 7 * 24 * time.Hour, "2w": 14 * 24 * time.Hour, "36h": 36 * time.Hour, "90m": 90 * time.Minute} {
		got, err := parseSince(in, now)
		if err != nil || !got.Equal(now.Add(-want)) {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	if got, err := parseSince("", now); err != nil || !got.IsZero() {
		t.Errorf("empty is all time: %v %v", got, err)
	}
	for _, bad := range []string{"x", "7", "-1d", "d", "1.5d"} {
		if _, err := parseSince(bad, now); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}
