package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/oorbt/slai/internal/ratelimit"
	"github.com/oorbt/slai/internal/store"
)

// The usage strip shows the Claude 5h / weekly rate-limit windows (written by
// `slai statusline` into KV key "ratelimit") plus month-to-date spend.

const (
	usageStale   = 10 * time.Minute // dim the strip
	usageExpired = 30 * time.Minute // show "?" instead of the numbers
	usageAmber   = 75.0
	usageRed     = 90.0
)

// loadUsageSnap reads the snapshot; nil means "no data yet" (or unreadable).
func loadUsageSnap(st *store.Store) *ratelimit.Snapshot {
	snap, ok, err := ratelimit.Load(st)
	if err != nil || !ok {
		return nil
	}
	return &snap
}

// fmtReset renders a countdown: "<1m", "48m", "1h48m", "3d", "3d4h".
func fmtReset(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	days, h := int(d.Hours())/24, int(d.Hours())%24
	if h == 0 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd%dh", days, h)
}

// usageColor picks the style for a used-percent value.
func usageColor(pct float64) lipgloss.Style {
	switch {
	case pct >= usageRed:
		return styleErr
	case pct >= usageAmber:
		return styleAmber
	}
	return lipgloss.NewStyle()
}

// usageGauge renders one window ("5h 62% ↻1h48m"). A nil or already-reset
// window is "–". Stale data is dimmed by the caller and never coloured.
func usageGauge(label string, w *ratelimit.Window, now time.Time, withReset, stale, expired bool) string {
	switch {
	case expired:
		return label + " ?"
	case w == nil || w.ResetsAt != 0 && w.ResetsAt <= now.Unix():
		return label + " –"
	}
	s := fmt.Sprintf("%s %.0f%%", label, w.UsedPercent)
	if !stale {
		s = usageColor(w.UsedPercent).Render(s)
	}
	if withReset && w.ResetsAt > 0 {
		s += " ↻" + fmtReset(time.Unix(w.ResetsAt, 0).Sub(now))
	}
	return s
}

// usageTiers returns the strip variants for terminal width w, richest first
// (spec: wide ≥120, medium 90–119, narrow 60–89, very narrow <60). The caller
// takes the first that fits next to the rest of the header.
func usageTiers(snap *ratelimit.Snapshot, month float64, now time.Time, w int) []string {
	var tiers []string
	add := func(parts ...string) { tiers = append(tiers, strings.Join(parts, " · ")) }
	var stale, expired bool
	if snap != nil {
		age := now.Sub(time.Unix(snap.At, 0))
		stale, expired = age > usageStale, age > usageExpired
	}
	if snap != nil && snap.Auth == "api_key" {
		// No plan rate limits (API-key auth): spend only, no gauges.
		if w >= 120 {
			add(fmt.Sprintf("~$%.2f mo", month))
		}
		return tiers
	}
	g := func(label string, win *ratelimit.Window, reset bool) string {
		if snap == nil {
			return label + " –"
		}
		return usageGauge(label, win, now, reset, stale, expired)
	}
	var fh, sd *ratelimit.Window
	if snap != nil {
		fh, sd = snap.FiveHour, snap.SevenDay
	}
	// Stale-but-not-expired values show how old they are (wide/medium only).
	ago := ""
	if snap != nil && stale && !expired {
		ago = " (" + fmtReset(now.Sub(time.Unix(snap.At, 0))) + " ago)"
	}
	if w >= 120 {
		add(g("5h", fh, true)+ago, g("wk", sd, true), fmt.Sprintf("~$%.2f mo", month))
	}
	if w >= 90 {
		add(g("5h", fh, true)+ago, g("wk", sd, false))
	}
	if w >= 60 {
		add(g("5h", fh, false), g("wk", sd, false))
	}
	add(g("5h", fh, false))
	if snap == nil || stale {
		for i, t := range tiers {
			tiers[i] = styleDim.Render(ansi.Strip(t))
		}
	}
	return tiers
}

// usageDetails is the body of the `i` popup.
func usageDetails(snap *ratelimit.Snapshot, today, month float64, now time.Time, cfgDir string, extra ...string) string {
	var b strings.Builder
	line := func(k, v string) { fmt.Fprintf(&b, "  %-14s %s\n", k, v) }
	b.WriteString(styleBold.Render("Claude usage") + "\n\n")
	win := func(label string, w *ratelimit.Window) {
		switch {
		case snap == nil || w == nil:
			line(label, "–")
		case w.ResetsAt != 0 && w.ResetsAt <= now.Unix():
			line(label, "– (window has reset; waiting for a refresh)")
		default:
			line(label, fmt.Sprintf("%.0f%% used · resets %s (in %s)", w.UsedPercent,
				time.Unix(w.ResetsAt, 0).Local().Format("Mon Jan 2 15:04"), fmtReset(time.Unix(w.ResetsAt, 0).Sub(now))))
		}
	}
	switch {
	case snap != nil && snap.Auth == "api_key":
		line("limits", "API key — no plan limits")
	default:
		win("5-hour window", snapWin(snap, true))
		win("weekly", snapWin(snap, false))
	}
	line("spend today", fmt.Sprintf("~$%.2f (estimate)", today))
	line("spend month", fmt.Sprintf("~$%.2f (estimate)", month))
	b.WriteString("\n")
	switch {
	case snap == nil:
		line("last update", "none yet")
	default:
		line("last update", fmtReset(now.Sub(time.Unix(snap.At, 0)))+" ago")
	}
	line("source", "Claude Code statusLine rate_limits (the real figures, not estimated)")
	line("account", cfgDir)
	for _, e := range extra {
		b.WriteString(e)
	}
	b.WriteString("\n" + styleDim.Render("  Plan windows are account-wide and may include usage outside slai.\n  Spend is estimated by slai from agent transcripts and its price table.\n  There is no monthly limit; month is slai's own spend.\n\n  press any key to close"))
	return b.String()
}

func snapWin(s *ratelimit.Snapshot, five bool) *ratelimit.Window {
	switch {
	case s == nil:
		return nil
	case five:
		return s.FiveHour
	}
	return s.SevenDay
}
