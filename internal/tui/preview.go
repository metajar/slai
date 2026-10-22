package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/oorbt/slai/internal/store"
)

// previewHeader: "eng-1 · sonnet · working · T-0004 Add Stripe webhook".
func previewHeader(a store.Agent, taskTitle string) string {
	parts := []string{a.ID}
	if a.Model != "" {
		parts = append(parts, a.Model)
	}
	parts = append(parts, a.Status)
	if a.CurrentTask != "" {
		t := a.CurrentTask
		if taskTitle != "" {
			t += " " + taskTitle
		}
		parts = append(parts, t)
	}
	return strings.Join(parts, " · ")
}

// fitPreview keeps ANSI intact, drops trailing blank lines, keeps the last h
// lines and truncates each to w cells.
func fitPreview(body string, w, h int) []string {
	if w <= 0 || h <= 0 {
		return nil
	}
	body = strings.ReplaceAll(body, "\r", "")
	lines := strings.Split(body, "\n")
	for len(lines) > 0 && strings.TrimSpace(ansi.Strip(lines[len(lines)-1])) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > h {
		lines = lines[len(lines)-h:]
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = ansi.Truncate(l, w, "") + "\x1b[0m"
	}
	return out
}

func taskTitleFor(tasks []store.Task, id string) string {
	for _, t := range tasks {
		if t.ID == id {
			return t.Title
		}
	}
	return ""
}

var _ = fmt.Sprintf
