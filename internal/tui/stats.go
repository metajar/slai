package tui

import (
	"fmt"
	"strings"

	"github.com/oorbt/slai/internal/service"
)

// statsMedianMinW is the narrowest terminal that still shows the median-time column.
const statsMedianMinW = 100

// statsFixedW is the width of the five numeric columns plus their gaps (no median).
const statsFixedW = 1 + 6 + 1 + 6 + 1 + 6 + 1 + 7 + 1 + 8

// statsEmpty is true when nothing has merged yet, so the table would be all zeros.
func statsEmpty(rep service.StatsReport) bool {
	for _, r := range rep.Agents {
		if r.Merged > 0 {
			return false
		}
	}
	return true
}

// renderStats lays the quality report out in w columns: one row per agent, then the
// harness/model groups. The median-time column is dropped below statsMedianMinW.
func renderStats(rep service.StatsReport, w int) []string {
	if statsEmpty(rep) {
		return []string{styleDim.Render("no finished tasks yet"),
			styleDim.Render("numbers appear once an engineer's task has merged")}
	}
	median := w >= statsMedianMinW
	nameW := w - statsFixedW
	if median {
		nameW -= 8
	}
	nameW = max(min(nameW, 30), 8)
	row := func(name string, merged, rounds, rework, refused, per, med string) string {
		s := fmt.Sprintf("%-*s %6s %6s %6s %7s %8s", nameW, name, merged, rounds, rework, refused, per)
		if median {
			s += fmt.Sprintf(" %7s", med)
		}
		return s
	}
	block := func(title string, rows []service.StatsRow, name func(service.StatsRow) string) []string {
		out := []string{styleBold.Render(row(title, "merged", "rounds", "rework", "refused", "$/merged", "time"))}
		for _, r := range rows {
			rounds, rework, per, med := "-", "-", "-", "-"
			if r.Merged > 0 {
				rounds = fmt.Sprintf("%.1f", r.RoundsPerMerged)
				rework = fmt.Sprintf("%.0f%%", r.ReworkRate*100)
				per = fmt.Sprintf("$%.2f", r.SpendPerMerged)
				med = statsDuration(r.MedianSeconds)
			}
			out = append(out, row(statsClip(name(r), nameW), fmt.Sprint(r.Merged), rounds, rework, fmt.Sprint(r.MergeRefused), per, med))
		}
		return out
	}
	lines := block("by agent", rep.Agents, func(r service.StatsRow) string { return r.Agent })
	lines = append(lines, "")
	lines = append(lines, block("by harness/model", rep.Groups, func(r service.StatsRow) string { return r.Harness + "/" + r.Model })...)
	lines = append(lines, "", styleDim.Render("rounds = review requests per merged task · rework = merged tasks that got request_changes"))
	return lines
}

func statsClip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func statsDuration(sec int64) string {
	switch {
	case sec >= 48*3600:
		return fmt.Sprintf("%dd%02dh", sec/86400, sec%86400/3600)
	case sec >= 3600:
		return fmt.Sprintf("%dh%02dm", sec/3600, sec%3600/60)
	case sec >= 60:
		return fmt.Sprintf("%dm", sec/60)
	}
	return fmt.Sprintf("%ds", sec)
}

func (m *Model) statsView() string {
	title := styleTitle.Render("Stats") + styleDim.Render("  all time, from tasks, review events and recorded spend")
	var lines []string
	switch {
	case m.statsErr != "":
		lines = []string{styleErr.Render("stats: " + m.statsErr)}
	case !m.statsOK:
		lines = []string{styleDim.Render("loading…")}
	default:
		lines = renderStats(m.stats, m.w)
	}
	return fit(title+"\n\n"+strings.Join(lines, "\n"), m.w, m.h-1) + "\n" + m.footerOverlay()
}
