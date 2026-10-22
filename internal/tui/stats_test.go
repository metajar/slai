package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/oorbt/slai/internal/service"
)

func seededStats() service.StatsReport {
	return service.StatsReport{
		Agents: []service.StatsRow{
			{Agent: "eng-1", Harness: "claude", Model: "sonnet", Merged: 8, RoundsPerMerged: 1.5, ReworkRate: 0.25, MergeRefused: 1, SpendPerMerged: 2.345, MedianSeconds: 5400},
			{Agent: "eng-2", Harness: "claude", Model: "sonnet", Merged: 4, RoundsPerMerged: 1.0, ReworkRate: 0, SpendPerMerged: 1.5, MedianSeconds: 600},
			{Agent: "eng-3", Harness: "pi", Model: "qwen", Merged: 0},
		},
		Groups: []service.StatsRow{
			{Harness: "claude", Model: "sonnet", Merged: 12, RoundsPerMerged: 1.33, ReworkRate: 0.17, MergeRefused: 1, SpendPerMerged: 2.06, MedianSeconds: 3000},
			{Harness: "pi", Model: "qwen"},
		},
	}
}

func plain(lines []string) string {
	var out []string
	for _, l := range lines {
		out = append(out, strings.TrimRight(ansi.Strip(l), " "))
	}
	return strings.Join(out, "\n")
}

// AC 1
func TestStatsViewGolden80(t *testing.T) {
	want := `by agent                       merged rounds rework refused $/merged
eng-1                               8    1.5    25%       1    $2.35
eng-2                               4    1.0     0%       0    $1.50
eng-3                               0      -      -       0        -

by harness/model               merged rounds rework refused $/merged
claude/sonnet                      12    1.3    17%       1    $2.06
pi/qwen                             0      -      -       0        -

rounds = review requests per merged task · rework = merged tasks that got request_changes`
	if got := plain(renderStats(seededStats(), 80)); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	for _, l := range strings.Split(plain(renderStats(seededStats(), 80)), "\n") {
		if ansi.StringWidth(l) > 90 && !strings.HasPrefix(l, "rounds =") {
			t.Errorf("row too wide: %q", l)
		}
	}
}

func TestStatsViewGolden120ShowsMedian(t *testing.T) {
	want := `by agent                       merged rounds rework refused $/merged    time
eng-1                               8    1.5    25%       1    $2.35   1h30m
eng-2                               4    1.0     0%       0    $1.50     10m
eng-3                               0      -      -       0        -       -

by harness/model               merged rounds rework refused $/merged    time
claude/sonnet                      12    1.3    17%       1    $2.06     50m
pi/qwen                             0      -      -       0        -       -

rounds = review requests per merged task · rework = merged tasks that got request_changes`
	if got := plain(renderStats(seededStats(), 120)); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	for _, l := range strings.Split(plain(renderStats(seededStats(), 80)), "\n") {
		if strings.Contains(l, "time") {
			t.Errorf("median column should be dropped at 80 cols: %q", l)
		}
	}
}

// AC 2
func TestStatsKeyOpensAndEscCloses(t *testing.T) {
	m := New(Options{})
	m.w, m.h = 100, 30
	m.handleKey(key("s"))
	if m.ov != ovStats {
		t.Fatalf("s did not open the stats view (ov=%v)", m.ov)
	}
	m.stats, m.statsOK = seededStats(), true
	if v := ansi.Strip(m.View()); !strings.Contains(v, "Stats") || !strings.Contains(v, "eng-1") {
		t.Errorf("view:\n%s", v)
	}
	m.handleKey(key("esc"))
	if m.ov != ovNone {
		t.Fatal("esc did not close it")
	}
	if !strings.Contains(helpText, "  s            Stats view") {
		t.Error("help does not list the key")
	}
	found := false
	for _, h := range listHints {
		found = found || h.key == "s"
	}
	if !found {
		t.Error("footer hints do not list the key")
	}
}

// AC 3
func TestStatsEmptyTeamSaysNoFinishedTasks(t *testing.T) {
	rep := service.StatsReport{Agents: []service.StatsRow{{Agent: "eng-1", Harness: "claude", Model: "sonnet"}}}
	got := plain(renderStats(rep, 80))
	if !strings.Contains(got, "no finished tasks yet") || strings.Contains(got, "rounds") {
		t.Fatalf("got:\n%s", got)
	}
	if got := plain(renderStats(service.StatsReport{}, 80)); !strings.Contains(got, "no finished tasks yet") {
		t.Fatalf("zero report: %s", got)
	}
}

// The numbers come from the board refresh while the view is open.
func TestStatsRefreshFillsTheViewOnlyWhenAsked(t *testing.T) {
	m := New(Options{})
	m.ov = ovStats
	m.Update(refreshMsg{statsAsked: true, stats: seededStats()})
	if !m.statsOK || len(m.stats.Agents) != 3 {
		t.Fatalf("stats not stored: ok=%v %+v", m.statsOK, m.stats)
	}
	m.Update(refreshMsg{}) // a refresh that did not ask keeps the last numbers
	if len(m.stats.Agents) != 3 {
		t.Error("an unrelated refresh wiped the stats")
	}
}
