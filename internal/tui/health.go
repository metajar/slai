package tui

import (
	"fmt"
	"github.com/oorbt/slai/internal/agentrows"
	"os"
	"sort"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/supervisor"
)

// heartbeatStale is how old the supervisor heartbeat may get before
// the TUI reports the supervisor as down.
const heartbeatStale = service.SupervisorDownAfter

// maxEscBanner caps how many escalation lines the banner uses.
const maxEscBanner = 3

// healthState is the team-health data polled with the store.
type healthState struct {
	byAgent map[string]service.Health
	esc     []store.Escalation
	hb      int64 // supervisor heartbeat (unix); 0 = never
	noLock  bool  // nobody holds the supervisor lock
	builds  service.BuildState
}

// loadHealth reads liveness for every agent, the open escalations and the
// supervisor heartbeat. A dead tmux session shows as status "dead".
func loadHealth(st *store.Store, dir string, cfg service.GateConfig, agents []store.Agent, now time.Time) healthState {
	h := healthState{byAgent: map[string]service.Health{}}
	for _, a := range agents {
		alive := a.Status != "dead" && a.Status != ""
		h.byAgent[a.ID] = service.AgentHealthGated(st, a.ID, alive, now, cfg)
	}
	h.esc, _ = st.OpenEscalations()
	h.hb, _, _ = st.SupervisorHeartbeat()
	h.builds = service.ReadBuildState(st, buildID(), agents)
	if dir != "" {
		// A leftover pidfile nobody holds means the supervisor died; no pidfile
		// at all means it was never started or was stopped on purpose.
		if _, err := os.Stat(supervisor.PidPath(dir)); err == nil {
			_, running := supervisor.Running(dir)
			h.noLock = !running
		}
	}
	return h
}

// supervisorDown reports the heartbeat time when it is older than heartbeatStale.
// A missing lock holder also counts as down; the zero time means "since unknown".
func supervisorDown(h healthState, now time.Time) (time.Time, bool) {
	if h.hb == 0 {
		return time.Time{}, h.noLock
	}
	t := time.Unix(h.hb, 0)
	return t, h.noLock || now.Sub(t) > heartbeatStale
}

// healthChip is the row chip for an agent whose liveness the status alone does
// not explain ("" = no chip): stale, rate_limited, or dead with a reason.
func healthChip(h service.Health, status string) (label, detail string) {
	return agentrows.Chip(h, status)
}

// escKindLabel gives escalation kinds a readable name (AC 19).
func escKindLabel(kind string) string {
	switch kind {
	case "rate_limited":
		return "rate limited"
	case "rate_limited_all":
		return "all agents rate limited"
	case "modal_stuck":
		return "stuck on a prompt"
	case "revive_gave_up":
		return "revive gave up"
	case "orch_down":
		return "orchestrator down"
	case "supervisor_down":
		return "supervisor down"
	case "unread_stuck":
		return "messages unread"
	case "stuck_gave_up":
		return "stuck, restart failed"
	case "stalled":
		return "stalled"
	}
	return kind
}

func isEscKind(kind string) bool {
	return escKindLabel(kind) != kind
}

func escKey(e store.Escalation) string { return fmt.Sprintf("%s|%s|%d", e.Kind, e.Agent, e.Since) }

// escBannerLines renders one red line per undismissed user-level escalation
// plus the supervisor-down notice. dismissed is keyed by escKey / supKey.
func escBannerLines(h healthState, dismissed map[string]bool, now time.Time, w int) []string {
	var out []string
	if t, down := supervisorDown(h, now); down && !dismissed[supKey(h.hb)] {
		msg := "supervisor: down"
		if !t.IsZero() {
			msg += " since " + t.Format("15:04")
		}
		out = append(out, msg)
	}
	var esc []store.Escalation
	for _, e := range h.esc {
		if e.User && !dismissed[escKey(e)] {
			esc = append(esc, e)
		}
	}
	sort.SliceStable(esc, func(i, j int) bool { return esc[i].Since < esc[j].Since })
	for i, e := range esc {
		if i == maxEscBanner-len(out) && len(esc)-i > 1 {
			out = append(out, fmt.Sprintf("+%d more (see log)", len(esc)-i))
			break
		}
		s := e.Agent
		if s == "" {
			s = "team"
		}
		s += ": " + escKindLabel(e.Kind)
		if e.Text != "" {
			s += " — " + trunc(e.Text, 100)
		}
		out = append(out, s)
	}
	for i, l := range out {
		out[i] = ansi.Truncate(styleBanner.Render("✖ "+l+"  (esc/x hides)"), w, "…")
	}
	return out
}

func supKey(hb int64) string { return fmt.Sprintf("supervisor|%d", hb) }

var styleBanner = lipgloss.NewStyle().Foreground(colRed).Bold(true)

// dismissBanner hides every currently shown escalation locally; the store is
// untouched, and a new escalation (different Since) shows again.
func (m *Model) dismissBanner() bool {
	if len(escBannerLines(m.health, m.dismissed, m.opts.Now(), m.w)) == 0 {
		return m.dismissOrphans()
	}
	m.dismissOrphans()
	if m.dismissed == nil {
		m.dismissed = map[string]bool{}
	}
	for _, e := range m.health.esc {
		if e.User {
			m.dismissed[escKey(e)] = true
		}
	}
	if _, down := supervisorDown(m.health, m.opts.Now()); down {
		m.dismissed[supKey(m.health.hb)] = true
	}
	return true
}

// queuedCount is how many agents are waiting for a concurrency slot.
func queuedCount(h map[string]service.Health) int {
	n := 0
	for _, v := range h {
		if v.Liveness == "gated" && v.Reason == "queued for slot" {
			n++
		}
	}
	return n
}
