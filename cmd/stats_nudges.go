package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
)

// Manual nudges (spec B6): times the user had to `slai send` to a silent, idle agent that owed
// work. The wake-up, reminder and board-check machinery should make that unnecessary, so the
// target is zero a week. `slai status --stats` reports the last few weeks.

const nudgeWeeks = 4

type nudgeWeek struct {
	WeekStart string `json:"week_start"` // Monday, UTC, YYYY-MM-DD
	Count     int    `json:"count"`
}

// manualNudgeWeeks counts manual_nudge events per week for the current and the previous
// nudgeWeeks-1 weeks, newest first. Weeks start on Monday, UTC.
func manualNudgeWeeks(st *store.Store, now time.Time) ([]nudgeWeek, error) {
	evs, err := st.EventsAfter(0, "manual_nudge", 100000)
	if err != nil {
		return nil, err
	}
	now = now.UTC()
	monday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -((int(now.Weekday()) + 6) % 7))
	out := make([]nudgeWeek, nudgeWeeks)
	for i := range out {
		out[i].WeekStart = monday.AddDate(0, 0, -7*i).Format("2006-01-02")
	}
	for _, e := range evs {
		at := time.Unix(e.At, 0).UTC()
		for i := range out {
			start := monday.AddDate(0, 0, -7*i)
			if !at.Before(start) && at.Before(start.AddDate(0, 0, 7)) {
				out[i].Count++
				break
			}
		}
	}
	return out, nil
}

func nudgeLine(weeks []nudgeWeek) string {
	labels := []string{"this week", "last week"}
	s := "manual nudges (target 0):"
	for i, w := range weeks {
		l := fmt.Sprintf("%d weeks ago", i)
		if i < len(labels) {
			l = labels[i]
		}
		sep := ","
		if i == 0 {
			sep = ""
		}
		s += fmt.Sprintf("%s %s %d", sep, l, w.Count)
	}
	return s
}

// writeStatsWithNudges is writeStats plus the manual-nudge counts: a last line in the text
// report, a manual_nudges key in the JSON (every other key unchanged).
func writeStatsWithNudges(w io.Writer, rep service.StatsReport, st *store.Store, now time.Time, asJSON bool) error {
	weeks, err := manualNudgeWeeks(st, now)
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(struct {
			service.StatsReport
			ManualNudges []nudgeWeek `json:"manual_nudges"`
		}{rep, weeks})
	}
	if err := writeStats(w, rep, false); err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, "\n"+nudgeLine(weeks))
	return err
}
