package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/oorbt/slai/internal/orphans"
)

// OrphanPollEvery is how often the TUI looks for orphaned slai teams. Looking
// means listing tmux sessions and scanning processes, so it is far slower than the
// 1 s refresh.
const OrphanPollEvery = 45 * time.Second

// orphansMsg carries one scan result.
type orphansMsg struct {
	teams []orphans.Team
	err   error
}

// orphanState is what the banner shows: the orphaned teams' prefixes (sorted) and
// the set the user dismissed.
type orphanState struct {
	prefixes  []string
	dismissed string // key of the dismissed set
	busy      bool
	polledAt  time.Time
}

// key identifies a set of orphans, so a dismissed set stays dismissed and a
// different one shows again.
func (o orphanState) key() string { return strings.Join(o.prefixes, ",") }

func (o orphanState) shown() bool { return len(o.prefixes) > 0 && o.key() != o.dismissed }

// orphanLine is the banner text.
func orphanLine(n int) string {
	return fmt.Sprintf("%d orphaned slai %s: run `slai down --orphans`", n, plural(n, "team", "teams"))
}

// orphanPollCmd starts a scan when one is due (the first tick, then every
// OrphanPollEvery) and none is running. It returns nil when there is nothing to do.
func (m *Model) orphanPollCmd() tea.Cmd {
	if m.opts.OtherTeams == nil || m.orphans.busy {
		return nil
	}
	now := m.opts.Now()
	if !m.orphans.polledAt.IsZero() && now.Sub(m.orphans.polledAt) < OrphanPollEvery {
		return nil
	}
	m.orphans.busy, m.orphans.polledAt = true, now
	list := m.opts.OtherTeams
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		teams, err := list(ctx)
		return orphansMsg{teams, err}
	}
}

// onOrphans records a scan result. A failed scan keeps the previous answer: a
// flaky tmux call must not flash the banner away.
func (m *Model) onOrphans(msg orphansMsg) {
	m.orphans.busy = false
	if msg.err != nil {
		return
	}
	var ps []string
	for _, t := range msg.teams {
		if t.Orphan {
			ps = append(ps, t.Prefix)
		}
	}
	sort.Strings(ps)
	m.orphans.prefixes = ps
}

// orphanBannerLine is the banner for the current set ("" when none or dismissed).
func (m *Model) orphanBannerLine() string {
	if !m.orphans.shown() {
		return ""
	}
	return styleAmber.Render("⚠ " + orphanLine(len(m.orphans.prefixes)))
}

// dismissOrphans hides the current set; it shows again only for a different set.
func (m *Model) dismissOrphans() bool {
	if !m.orphans.shown() {
		return false
	}
	m.orphans.dismissed = m.orphans.key()
	return true
}
