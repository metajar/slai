package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/ratelimit"
	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/tmux"
)

type (
	tickMsg    time.Time
	refreshMsg struct {
		agents     []store.Agent
		tasks      []store.Task
		events     []store.Event
		spend      float64
		month      float64
		usage      *ratelimit.Snapshot
		paused     map[string]bool
		health     healthState
		agentSpend map[string]float64
		runDone    string
		statsAsked bool // the Stats view was open when this refresh started
		stats      service.StatsReport
		statsErr   error
		warn       string
		err        error
	}
	previewMsg struct {
		agent string
		snap  tmux.Snapshot
		err   error
	}
	resizedMsg struct {
		agent string
		w, h  int
		err   error
	}
	fastMsg   struct{}
	actionMsg struct {
		text string
		err  error
		// followTask is the task whose row the Tasks view cursor should stay on once
		// the next refresh has re-sorted the board.
		followTask string
	}
	attachReadyMsg struct{ session string }
	attachDoneMsg  struct{ err error }
	detailMsg      struct {
		task    store.Task
		spec    string
		history []string
		memory  string // the Memory section: facts delivered, with a stats line
		err     error
	}
	diffMsg struct {
		task  store.Task
		patch string
		files int
		err   error
	}
)

func parseData(s string) map[string]any {
	var m map[string]any
	if s != "" {
		_ = json.Unmarshal([]byte(s), &m)
	}
	return m
}

const tickEvery = 100 * time.Millisecond

func tick() tea.Cmd {
	return tea.Tick(tickEvery, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func ctx3() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

func (m *Model) refreshCmd() tea.Cmd {
	st := m.opts.Store
	if st == nil {
		return nil
	}
	warn := m.opts.Warn
	dir := m.opts.Dir
	wantStats := m.ov == ovStats
	team := m.opts.Team
	return func() tea.Msg {
		var r refreshMsg
		var err error
		if r.agents, err = st.GetAgents(); err != nil {
			r.err = err
			return r
		}
		if r.tasks, err = st.ListTasks(""); err != nil {
			r.err = err
			return r
		}
		if r.events, err = st.RecentEvents(500); err != nil {
			r.err = err
			return r
		}
		r.spend, _ = st.TodaySpend()
		r.month, _ = st.MonthSpend()
		r.usage = loadUsageSnap(st)
		r.paused, _ = st.PausedAgents()
		r.agentSpend, _ = st.SpendByAgentToday()
		r.health = loadHealth(st, dir, service.GateConfigOf(m.opts.Team), r.agents, time.Now())
		r.runDone, _, _ = st.GetKV("run.done")
		if warn != nil {
			r.warn = warn()
		}
		if wantStats {
			r.statsAsked = true
			r.stats, r.statsErr = (&service.Service{Store: st, Team: team, Dir: dir}).Stats(time.Time{})
		}
		return r
	}
}

// previewCmd captures agent's pane (with cursor) off the UI goroutine.
func (m *Model) previewCmd(agent string) tea.Cmd {
	io := m.io
	if io == nil || agent == "" {
		return nil
	}
	sess := m.session(agent)
	off := m.scroll[agent]
	return func() tea.Msg {
		ctx, cancel := ctx3()
		defer cancel()
		var snap tmux.Snapshot
		var err error
		if sio, ok := io.(scrollIO); ok {
			snap, err = sio.SnapshotAt(ctx, sess, off)
		} else {
			snap, err = io.Snapshot(ctx, sess)
		}
		return previewMsg{agent: agent, snap: snap, err: err}
	}
}

// resizeCmd sizes agent's tmux window to exactly w x h cells.
func (m *Model) resizeCmd(agent string, w, h int) tea.Cmd {
	io := m.io
	if io == nil || agent == "" || w < 1 || h < 1 {
		return nil
	}
	sess := m.session(agent)
	return func() tea.Msg {
		ctx, cancel := ctx3()
		defer cancel()
		return resizedMsg{agent: agent, w: w, h: h, err: io.ResizeWindow(ctx, sess, w, h)}
	}
}

// sendCmd is the quick prompt: text + Enter into the pane, recorded as a user
// message (already delivered, so no wake-up is injected on top of it).
func (m *Model) sendCmd(target, text string, waitingOnUser bool) tea.Cmd {
	o := m.opts
	io := m.io
	sess := m.session(target)
	return func() tea.Msg {
		ctx, cancel := ctx3()
		defer cancel()
		if io != nil {
			if err := io.SendKeys(ctx, sess, text, true); err != nil {
				return actionMsg{err: err}
			}
		}
		if o.Store != nil {
			id, err := o.Store.AddMessage(store.Message{From: "user", To: target, Kind: "user", Body: text})
			if err != nil {
				return actionMsg{err: err}
			}
			_ = o.Store.MarkDelivered(id)
		}
		if waitingOnUser {
			return actionMsg{text: "answered " + target}
		}
		return actionMsg{text: "sent to " + target}
	}
}

// bindDetachCmd binds ctrl+q to detach-client on the agents' tmux server and
// lets tmux size the window to the real terminal while attached.
func (m *Model) bindDetachCmd(session string) tea.Cmd {
	io := m.io
	return func() tea.Msg {
		if io != nil {
			ctx, cancel := ctx3()
			defer cancel()
			_ = io.BindDetachKey(ctx, "C-q")
			_ = io.WindowSizeLatest(ctx, session)
		}
		return attachReadyMsg{session: session}
	}
}

func (m *Model) stopCmd(agent string, restart bool) tea.Cmd {
	o := m.opts
	f := o.Stop
	if restart {
		f = o.Restart
	}
	if f == nil {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := f(ctx, agent); err != nil {
			return actionMsg{err: err}
		}
		if restart {
			return actionMsg{text: "restarted " + agent + " (fresh session)"}
		}
		return actionMsg{text: "stopped " + agent + " (v to revive)"}
	}
}

func (m *Model) resumeCmd(agent string) tea.Cmd {
	f := m.opts.ResumeAgent
	if f == nil {
		return nil
	}
	return func() tea.Msg {
		if err := f(agent); err != nil {
			return actionMsg{err: err}
		}
		return actionMsg{text: "resumed " + agent}
	}
}

func (m *Model) reviveCmd(agent string) tea.Cmd {
	f := m.opts.Revive
	if f == nil {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := f(ctx, agent); err != nil {
			return actionMsg{err: err}
		}
		return actionMsg{text: "revived " + agent}
	}
}

func (m *Model) pauseCmd() tea.Cmd {
	o := m.opts
	if o.SetPaused == nil {
		return nil
	}
	cur := o.IsPaused != nil && o.IsPaused()
	return func() tea.Msg {
		if err := o.SetPaused(!cur); err != nil {
			return actionMsg{err: err}
		}
		if cur {
			return actionMsg{text: "resumed"}
		}
		return actionMsg{text: "paused"}
	}
}

func (m *Model) clearFinishedCmd(agent string) tea.Cmd {
	st := m.opts.Store
	if st == nil {
		return nil
	}
	return func() tea.Msg {
		_ = st.SetAgentStatus(agent, "idle")
		return nil
	}
}

func (m *Model) detailCmd(t store.Task) tea.Cmd {
	o := m.opts
	return func() tea.Msg {
		r := detailMsg{task: t}
		if o.Store != nil {
			if fresh, err := o.Store.GetTask(t.ID); err == nil {
				r.task = fresh
			}
			ev, _ := o.Store.RecentEvents(1000)
			msgs, _ := o.Store.MessagesForTask(t.ID)
			tasks, _ := o.Store.ListTasks("")
			r.history = buildHistory(ev, msgs, logCtx{team: o.Team, tasks: tasks}, t.ID)
			r.memory = taskMemory(o, t.ID)
		}
		if r.task.SpecPath != "" {
			p := r.task.SpecPath
			if !filepath.IsAbs(p) {
				p = filepath.Join(o.Dir, p)
			}
			if b, err := os.ReadFile(p); err == nil {
				lines := strings.Split(string(b), "\n")
				if len(lines) > 60 {
					lines = append(lines[:60], "… (truncated)")
				}
				r.spec = strings.Join(lines, "\n")
			}
		}
		return r
	}
}

func (m *Model) diffCmd(t store.Task) tea.Cmd {
	o := m.opts
	base := config.DefaultIntegrationBranch
	if o.Team != nil && o.Team.Project.IntegrationBranch != "" {
		base = o.Team.Project.IntegrationBranch
	}
	return func() tea.Msg {
		if o.Git == nil {
			return diffMsg{task: t, err: fmt.Errorf("no git repo configured")}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		res, err := o.Git.Diff(ctx, base, t.Branch, 2000)
		if err != nil {
			return diffMsg{task: t, err: err}
		}
		return diffMsg{task: t, patch: res.Patch, files: len(res.Files)}
	}
}

// focusBeatCmd tells the supervisor the user is typing into agent's pane, so
// it holds back wake-up lines that would interleave with the user's keys.
func (m *Model) focusBeatCmd(agent string) tea.Cmd {
	st, now := m.opts.Store, m.opts.Now
	if st == nil || agent == "" {
		return nil
	}
	return func() tea.Msg {
		_ = st.TouchFocus(agent, now())
		return nil
	}
}

func (m *Model) focusClearCmd(agent string) tea.Cmd {
	st := m.opts.Store
	if st == nil || agent == "" {
		return nil
	}
	return func() tea.Msg {
		_ = st.ClearFocus(agent)
		return nil
	}
}
