package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/oorbt/slai/internal/service"
)

// parseSince reads a --since value (see service.ParseSince).
func parseSince(v string, now time.Time) (time.Time, error) { return service.ParseSince(v, now) }

// writeStats prints the stats report: JSON when asJSON, else two aligned tables (by
// agent, then by harness+model) that stay within 100 columns.
func writeStats(w io.Writer, rep service.StatsReport, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	if rep.Since > 0 {
		fmt.Fprintf(w, "tasks assigned since %s\n", time.Unix(rep.Since, 0).Format("2006-01-02 15:04"))
	} else {
		fmt.Fprintln(w, "all time")
	}
	fmt.Fprintln(w, "\nby agent:")
	statsTable(w, rep.Agents, func(r service.StatsRow) string { return r.Agent + " " + r.Harness + "/" + r.Model })
	fmt.Fprintln(w, "\nby harness/model:")
	statsTable(w, rep.Groups, func(r service.StatsRow) string { return r.Harness + "/" + r.Model })
	fmt.Fprintln(w, "\nrounds = request_review calls per merged task; rework = merged tasks that got request_changes;")
	fmt.Fprintln(w, "refused = merge_refused events; time = median assigned to merged; spend is from recorded usage.")
	return nil
}

const statsNameWidth = 30

func statsTable(w io.Writer, rows []service.StatsRow, name func(service.StatsRow) string) {
	const f = "%-*s %6s %7s %6s %6s %7s %8s %8s %7s\n"
	fmt.Fprintf(w, f, statsNameWidth, "", "merged", "can/rev", "rounds", "rework", "refused", "spend", "$/merged", "time")
	for _, r := range rows {
		rounds, rework, per, med := "-", "-", "-", "-"
		if r.Merged > 0 {
			rounds = fmt.Sprintf("%.1f", r.RoundsPerMerged)
			rework = fmt.Sprintf("%.0f%%", r.ReworkRate*100)
			per = fmt.Sprintf("$%.2f", r.SpendPerMerged)
			med = shortDuration(r.MedianSeconds)
		}
		fmt.Fprintf(w, f, statsNameWidth, clip(name(r), statsNameWidth), strconv.Itoa(r.Merged),
			fmt.Sprintf("%d/%d", r.Cancelled, r.Reverted), rounds, rework, strconv.Itoa(r.MergeRefused),
			fmt.Sprintf("$%.2f", r.SpendUSD), per, med)
	}
}

// clip shortens s to n runes, ending in "…" when it was cut.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

func shortDuration(sec int64) string {
	d := time.Duration(sec) * time.Second
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd%02dh", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%ds", sec)
}
