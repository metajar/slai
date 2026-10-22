package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/orphans"
)

type fakeLister struct {
	teams []orphans.Team
	err   error
	calls int
}

func (f *fakeLister) list(context.Context) ([]orphans.Team, error) {
	f.calls++
	return f.teams, f.err
}

func orphanTeam(prefix string, orphan bool) orphans.Team {
	return orphans.Team{Prefix: prefix, Project: "/p/" + prefix, Orphan: orphan}
}

// poll runs one scheduled scan and feeds its result to the model.
func pollOrphans(t *testing.T, m *Model) bool {
	t.Helper()
	cmd := m.orphanPollCmd()
	if cmd == nil {
		return false
	}
	m.Update(cmd())
	return true
}

func orphanModel(t *testing.T, f *fakeLister) (*Model, *time.Time) {
	t.Helper()
	m, _ := newModel(t)
	now := time.Unix(5_000_000, 0)
	m.opts.Now = func() time.Time { return now }
	m.opts.OtherTeams = f.list
	return m, &now
}

// AC 5 (item 3): a banner when orphans exist; healthy other teams do not count.
func TestOrphanBannerShowsOnlyForOrphans(t *testing.T) {
	f := &fakeLister{teams: []orphans.Team{orphanTeam("slai_aaa_", true), orphanTeam("slai_bbb_", false), orphanTeam("slai_ccc_", true)}}
	m, _ := orphanModel(t, f)
	if strings.Contains(screen(m), "orphaned") {
		t.Fatal("banner before any scan")
	}
	if !pollOrphans(t, m) {
		t.Fatal("the first tick must scan")
	}
	if v := screen(m); !strings.Contains(v, "2 orphaned slai teams: run `slai down --orphans`") {
		t.Fatalf("banner missing:\n%s", v)
	}
	f.teams = []orphans.Team{orphanTeam("slai_aaa_", true)}
	m.orphans.polledAt = time.Time{}
	pollOrphans(t, m)
	if v := screen(m); !strings.Contains(v, "1 orphaned slai team: run") {
		t.Fatalf("singular banner:\n%s", v)
	}
	f.teams = []orphans.Team{orphanTeam("slai_bbb_", false)}
	m.orphans.polledAt = time.Time{}
	pollOrphans(t, m)
	if strings.Contains(screen(m), "orphaned") {
		t.Fatal("no orphans, no banner")
	}
}

// Polling is cheap: not on every refresh, and never two at once.
func TestOrphanPollIsRateLimited(t *testing.T) {
	f := &fakeLister{teams: []orphans.Team{orphanTeam("slai_aaa_", true)}}
	m, now := orphanModel(t, f)
	cmd := m.orphanPollCmd()
	if cmd == nil || m.orphanPollCmd() != nil {
		t.Fatal("a scan in flight must block the next one")
	}
	m.Update(cmd())
	for i := 0; i < 20; i++ { // tick-driven polling inside the interval
		*now = now.Add(2 * time.Second)
		if m.orphanPollCmd() != nil {
			t.Fatalf("polled again after only %v", 2*time.Duration(i+1)*time.Second)
		}
	}
	*now = now.Add(OrphanPollEvery)
	if m.orphanPollCmd() == nil {
		t.Fatal("no scan after the interval")
	}
	if f.calls != 1 {
		t.Fatalf("lister called %d times by the rate-limit checks", f.calls)
	}
	// the tick handler schedules it
	m2, _ := orphanModel(t, &fakeLister{})
	if _, c := m2.Update(tickMsg(time.Now())); c == nil || !m2.orphans.busy {
		t.Error("the first tick must start a scan")
	}
}

// x dismisses; the same set is not shown again; a different set is.
func TestOrphanBannerDismissAndReshow(t *testing.T) {
	f := &fakeLister{teams: []orphans.Team{orphanTeam("slai_aaa_", true), orphanTeam("slai_ccc_", true)}}
	m, now := orphanModel(t, f)
	pollOrphans(t, m)
	if !strings.Contains(screen(m), "orphaned") {
		t.Fatal("banner expected")
	}
	m.Update(key("x"))
	if strings.Contains(screen(m), "orphaned") {
		t.Fatal("x must hide the banner")
	}
	// same set, listed in another order, on a later scan: still hidden
	f.teams = []orphans.Team{orphanTeam("slai_ccc_", true), orphanTeam("slai_aaa_", true), orphanTeam("slai_bbb_", false)}
	*now = now.Add(OrphanPollEvery + time.Second)
	pollOrphans(t, m)
	if strings.Contains(screen(m), "orphaned") {
		t.Fatal("the same set must not come back")
	}
	// a new orphan changes the set
	f.teams = append(f.teams, orphanTeam("slai_ddd_", true))
	*now = now.Add(OrphanPollEvery + time.Second)
	pollOrphans(t, m)
	if !strings.Contains(screen(m), "3 orphaned slai teams") {
		t.Fatalf("a different set must show again:\n%s", screen(m))
	}
	// dismissing with nothing to dismiss reports false (esc falls through)
	m.Update(key("x"))
	if m.dismissOrphans() {
		t.Error("nothing left to dismiss")
	}
}

// A failed scan keeps the last answer; no lister, no banner.
func TestOrphanScanErrorKeepsLastAnswerAndNilListerIsOff(t *testing.T) {
	f := &fakeLister{teams: []orphans.Team{orphanTeam("slai_aaa_", true)}}
	m, now := orphanModel(t, f)
	pollOrphans(t, m)
	f.teams, f.err = nil, errors.New("tmux: no server")
	*now = now.Add(OrphanPollEvery + time.Second)
	pollOrphans(t, m)
	if !strings.Contains(screen(m), "1 orphaned slai team") {
		t.Fatalf("a failed scan flashed the banner away:\n%s", screen(m))
	}
	off, _ := newModel(t)
	if off.orphanPollCmd() != nil {
		t.Error("without a lister nothing is scanned")
	}
}
