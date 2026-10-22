package tui

import (
	"github.com/oorbt/slai/internal/ratelimit"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

var usageNow = time.Unix(1_800_000_000, 0)

func snapAt(fh, sd float64, age time.Duration) *ratelimit.Snapshot {
	return &ratelimit.Snapshot{
		FiveHour: &ratelimit.Window{UsedPercent: fh, ResetsAt: usageNow.Add(108 * time.Minute).Unix()},
		SevenDay: &ratelimit.Window{UsedPercent: sd, ResetsAt: usageNow.Add(72 * time.Hour).Unix()},
		At:       usageNow.Add(-age).Unix(),
	}
}

func plainTiers(s *ratelimit.Snapshot, month float64, w int) []string {
	var out []string
	for _, t := range usageTiers(s, month, usageNow, w) {
		out = append(out, ansi.Strip(t))
	}
	return out
}

func TestFmtReset(t *testing.T) {
	for d, want := range map[time.Duration]string{
		10 * time.Second: "<1m", 48 * time.Minute: "48m", 108 * time.Minute: "1h48m",
		72 * time.Hour: "3d", 76 * time.Hour: "3d4h",
	} {
		if got := fmtReset(d); got != want {
			t.Errorf("fmtReset(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestUsageWidthTiers(t *testing.T) {
	s := snapAt(62, 31, 0)
	cases := map[int]string{
		140: "5h 62% ↻1h48m · wk 31% ↻3d · ~$61.20 mo",
		100: "5h 62% ↻1h48m · wk 31%",
		70:  "5h 62% · wk 31%",
		40:  "5h 62%",
	}
	for w, want := range cases {
		if got := plainTiers(s, 61.2, w)[0]; got != want {
			t.Errorf("w=%d got %q, want %q", w, got, want)
		}
	}
}

func TestUsageThresholdColours(t *testing.T) {
	for pct, want := range map[float64]string{74: "", 75: styleAmber.Render("5h 75%"), 89: styleAmber.Render("5h 89%"), 90: styleErr.Render("5h 90%")} {
		got := usageGauge("5h", &ratelimit.Window{UsedPercent: pct, ResetsAt: usageNow.Add(time.Hour).Unix()}, usageNow, false, false, false)
		if pct == 74 {
			if got != "5h 74%" {
				t.Errorf("74 = %q", got)
			}
			continue
		}
		if got != want {
			t.Errorf("pct %v = %q, want %q", pct, got, want)
		}
	}
	// stale data is never amber/red
	got := usageGauge("5h", &ratelimit.Window{UsedPercent: 95, ResetsAt: usageNow.Add(time.Hour).Unix()}, usageNow, false, true, false)
	if got != "5h 95%" {
		t.Errorf("stale gauge coloured: %q", got)
	}
}

func TestUsageNilAndSpendOnly(t *testing.T) {
	if got := plainTiers(nil, 5, 140)[0]; got != "5h – · wk – · ~$5.00 mo" {
		t.Errorf("no data = %q", got)
	}
	s := snapAt(10, 10, 0)
	s.SevenDay = nil
	if got := plainTiers(s, 5, 100)[0]; got != "5h 10% ↻1h48m · wk –" {
		t.Errorf("nil window = %q", got)
	}
	// reset already passed: show "–" not the old percent
	s = snapAt(80, 10, 0)
	s.FiveHour.ResetsAt = usageNow.Add(-time.Minute).Unix()
	if got := plainTiers(s, 5, 70)[0]; !strings.HasPrefix(got, "5h –") {
		t.Errorf("reset window = %q", got)
	}
	// API key: no windows at all -> spend only
	api := &ratelimit.Snapshot{Auth: "api_key", At: usageNow.Unix()}
	if got := plainTiers(api, 61.2, 140); len(got) != 1 || got[0] != "~$61.20 mo" {
		t.Errorf("spend only = %v", got)
	}
	if got := plainTiers(api, 61.2, 100); len(got) != 0 {
		t.Errorf("spend only narrow = %v", got)
	}
	// a snapshot with no windows but not api_key is still "–" gauges
	if got := plainTiers(&ratelimit.Snapshot{At: usageNow.Unix()}, 5, 70)[0]; got != "5h – · wk –" {
		t.Errorf("empty plan snapshot = %q", got)
	}
}

func TestUsageStaleShowsAge(t *testing.T) {
	if got := plainTiers(snapAt(95, 50, 12*time.Minute), 1, 100)[0]; got != "5h 95% ↻1h48m (12m ago) · wk 50%" {
		t.Errorf("stale age = %q", got)
	}
	if got := plainTiers(snapAt(95, 50, 12*time.Minute), 1, 70)[0]; got != "5h 95% · wk 50%" {
		t.Errorf("narrow stale = %q", got)
	}
}

func TestUsagePopup(t *testing.T) {
	m, _ := newModel(t)
	m.opts.Now = func() time.Time { return usageNow }
	m.usage, m.month = snapAt(62, 31, 3*time.Minute), 61.2
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'i'}})
	if m.ov != ovUsage {
		t.Fatal("i did not open the usage popup")
	}
	v := ansi.Strip(m.View())
	for _, want := range []string{"62% used", "31% used", "~$61.20 (estimate)", "3m ago", "statusLine", "account-wide", "CLAUDE_CONFIG_DIR", "no monthly limit"} {
		if !strings.Contains(v, want) {
			t.Errorf("popup missing %q:\n%s", want, v)
		}
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	if m.ov != ovNone {
		t.Fatal("popup did not close")
	}
	m.usage = &ratelimit.Snapshot{Auth: "api_key", At: usageNow.Unix()}
	if d := usageDetails(m.usage, 1, 2, usageNow, "cfg"); !strings.Contains(d, "API key — no plan limits") {
		t.Errorf("api key popup: %s", d)
	}
}

func TestUsageStaleDimAndExpired(t *testing.T) {
	if got := usageTiers(snapAt(95, 50, 11*time.Minute), 1, usageNow, 100)[0]; got != styleDim.Render("5h 95% ↻1h48m (11m ago) · wk 50%") {
		t.Errorf("stale not dimmed: %q", got)
	}
	if got := plainTiers(snapAt(95, 50, 31*time.Minute), 1, 100)[0]; got != "5h ? · wk ?" {
		t.Errorf("expired = %q", got)
	}
}

func TestHeaderUsageWidthsAndToggle(t *testing.T) {
	m, _ := newModel(t)
	m.opts.Now = func() time.Time { return usageNow }
	m.usage, m.month = snapAt(62, 31, 0), 61.2
	for _, w := range []int{40, 60, 80, 100, 140} {
		m.w = w
		h := m.headerView()
		if lw := ansi.StringWidth(h); lw > w {
			t.Errorf("w=%d header width %d: %q", w, lw, ansi.Strip(h))
		}
		if strings.Contains(h, "\n") {
			t.Errorf("w=%d wrapped", w)
		}
	}
	m.w = 140
	if h := ansi.Strip(m.headerView()); !strings.Contains(h, "5h 62% ↻1h48m · wk 31% ↻3d · ~$61.20 mo") {
		t.Errorf("wide header: %q", h)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'U'}})
	if h := ansi.Strip(m.headerView()); strings.Contains(h, "5h") {
		t.Errorf("strip still shown after U: %q", h)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'U'}})
	if h := ansi.Strip(m.headerView()); !strings.Contains(h, "5h 62%") {
		t.Errorf("strip not restored: %q", h)
	}
}

func TestRefreshLoadsUsageSnapshot(t *testing.T) {
	m, s := newModel(t)
	want := ratelimit.Snapshot{Auth: "oauth", At: 1800000000, FiveHour: &ratelimit.Window{UsedPercent: 42, ResetsAt: 1800005000}}
	if err := ratelimit.Save(s, want); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUsage("eng-1", 1, 1, 2.5); err != nil {
		t.Fatal(err)
	}
	m.Update(m.refreshCmd()())
	if m.usage == nil || m.usage.FiveHour == nil || m.usage.FiveHour.UsedPercent != 42 || m.usage.Auth != "oauth" {
		t.Fatalf("usage not loaded: %+v", m.usage)
	}
	if m.month != 2.5 {
		t.Fatalf("month spend = %v, want 2.5", m.month)
	}
}
