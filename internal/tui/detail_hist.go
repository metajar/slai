package tui

import (
	"fmt"
	"sort"
	"time"

	"github.com/oorbt/slai/internal/store"
)

type histItem struct {
	at   int64
	seq  int
	text string
}

// buildHistory merges task-related events and real messages (assignment,
// reviews, request_changes comments, replies) into one chronological list.
func buildHistory(events []store.Event, msgs []store.Message, c logCtx, taskID string) []string {
	var items []histItem
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if isNoise(e) || !eventMentions(e, taskID) {
			continue
		}
		items = append(items, histItem{e.At, 0, styleDim.Render("• ") + c.format(e)})
	}
	for _, m := range msgs {
		head := fmt.Sprintf("%s → %s [%s]", orDefault(m.From, "system"), m.To, m.Kind)
		if m.ReplyTo != 0 {
			head += fmt.Sprintf(" (reply to #%d)", m.ReplyTo)
		}
		body := "\n    " + indent(m.Body)
		items = append(items, histItem{m.CreatedAt, 1, styleBold.Render("✉ "+head) + body})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].at != items[j].at {
			return items[i].at < items[j].at
		}
		return items[i].seq < items[j].seq
	})
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = styleDim.Render(time.Unix(it.at, 0).Format("15:04:05")) + " " + it.text
	}
	return out
}

func eventMentions(e store.Event, taskID string) bool {
	d := parseData(e.Data)
	return firstString(d, "task_id", "task") == taskID
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func indent(s string) string {
	out := ""
	for i, l := range splitLines(s) {
		if i > 0 {
			out += "\n    "
		}
		out += l
	}
	return out
}
