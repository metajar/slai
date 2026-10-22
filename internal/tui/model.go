package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/git"
	"github.com/oorbt/slai/internal/memory"
	"github.com/oorbt/slai/internal/orphans"
	"github.com/oorbt/slai/internal/ratelimit"
	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/tmux"
)

// Options configures the TUI.
type Options struct {
	Project string
	Dir     string
	Team    *config.Team
	Store   *store.Store
	Tmux    *tmux.Client // the agents' (private) tmux server
	Git     *git.Repo
	// Memory is the team memory (the .slai/memory files and their index); nil hides the Memory
	// view. Editor builds the command that edits a file (default: $VISUAL, $EDITOR, vi).
	Memory      *memory.Memory
	Editor      func(path string) *exec.Cmd
	SessionName func(agentID string) string

	// OtherTeams lists the slai teams other than this project's (orphans flagged);
	// nil disables the orphan banner.
	OtherTeams func(ctx context.Context) ([]orphans.Team, error)

	ResumeAgent func(agentID string) error                      // clear a slai hold (loop guard) on one agent
	Revive      func(ctx context.Context, agentID string) error // start a dead agent (claude --continue)
	Stop        func(ctx context.Context, agentID string) error // kill the agent's session
	Restart     func(ctx context.Context, agentID string) error // stop + start a fresh conversation
	// SetPaused / IsPaused pause and resume wake-up delivery.
	SetPaused func(bool) error
	IsPaused  func() bool
	// CurrentHold reports the team hold (`slai hold`); HoldTeam starts one with an optional
	// name and ReleaseHold lifts it. Same supervisor calls as the CLI.
	CurrentHold func() (name string, since int64, held bool)
	HoldTeam    func(name string) error
	ReleaseHold func() error
	// SetPriority changes a task's priority (number, up, down or top) and returns the new value.
	SetPriority func(taskID, spec string) (int, error)
	// StartLoop, if set, runs in a goroutine for the life of the TUI; ctx is cancelled on exit.
	StartLoop func(ctx context.Context)
	// Supervisor describes who runs the supervisor loop: "tui" or "daemon".
	Supervisor string
	// Warn, if set, returns a persistent warning banner ("" = none); polled with the store.
	Warn func() string
	Now  func() time.Time
	// TeamChanged, if set, hands a team.yaml change to the in-process supervisor
	// and reconciles it; nil (or an error) means nothing started the new agent.
	TeamChanged func(*config.Team) error

	// Pane overrides Tmux for pane I/O (tests).
	Pane paneIO
}

type mode int

const (
	mList     mode = iota // arrows move the agent list
	mFocus                // every key goes to the selected agent's pane
	mQuick                // quick-prompt popup
	mConfirm              // y/n question
	mAddAgent             // New Agent form
)

type overlay int

const (
	ovNone overlay = iota
	ovHelp
	ovDetail
	ovDiff
	ovTasks
	ovLog
	ovUsage
	ovStats
	ovMemory
)

type confirmState struct {
	what  string // "stop" | "restart"
	agent string
}

// Model is the bubbletea model.
type Model struct {
	opts Options
	io   paneIO
	snd  *sender

	w, h int

	agents   []store.Agent
	tasks    []store.Task
	events   []store.Event
	spend    float64
	month    float64
	usage    *ratelimit.Snapshot
	usageOff bool // U toggles the header usage strip
	warn     string

	items   []item
	cur     int    // cursor: index into items
	sel     string // selected agent id (drives the preview)
	folded  map[string]bool
	listTop int // first visible list line

	mode    mode
	ov      overlay
	ovFrom  overlay
	ovTitle string
	vp      viewport.Model

	tasksCur    int
	tasksFollow string // keep the Tasks cursor on this task after the next refresh
	showHooks   bool

	form     *addForm
	orphans  orphanState
	quick    textinput.Model
	mem      memState            // the Memory view
	stats    service.StatsReport // last report, refreshed while the Stats view is open
	statsOK  bool
	statsErr string
	comp     completion // Tab-completion state of the quick box
	quickTo  string
	// quickRedirect makes the quick prompt a course correction (interrupt, then deliver).
	quickRedirect bool
	// quickHold makes the quick prompt the optional name of a team hold.
	quickHold bool
	// holdName / holdSince / held mirror the team hold for the banner.
	holdName  string
	holdSince int64
	held      bool
	confirm   confirmState

	notify     map[string]bool
	paused     bool
	heldAgents map[string]bool
	health     healthState
	agentSpend map[string]float64
	dismissed  map[string]bool // escalation banner entries hidden locally
	runDone    string          // the orchestrator's final summary once it called finish
	status     string
	statusE    bool
	statAt     time.Time

	snap        tmux.Snapshot
	snapFor     string
	snapErr     string
	previewBusy bool
	refreshBusy bool
	fastPending bool
	tickN       int
	lastKeyAt   time.Time // last keystroke forwarded while focused
	resized     map[string]resizeInfo

	// preview scrollback / selection / mouse
	scroll  map[string]int // agent -> lines back from the bottom of its history
	hist    map[string]int // agent -> last seen #{history_size}
	ms      mouseState
	hl      selection
	clip    *Clipboard
	mouseOn bool // false: mouse capture released so the terminal's own selection works
}

// selection is the highlighted range inside the preview (inner cell coordinates).
type selection struct {
	active bool
	a, b   cell
}

type resizeInfo struct {
	w, h int
	at   time.Time
}

// Run starts the TUI and blocks until quit.
func Run(opts Options) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if opts.StartLoop != nil {
		go opts.StartLoop(ctx)
	}
	p := tea.NewProgram(New(opts), tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err := p.Run()
	return err
}

// New builds a Model (exported for tests / embedding).
func New(opts Options) *Model {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	ti := textinput.New()
	ti.Prompt = ""
	ti.CharLimit = 4000
	m := &Model{opts: opts, notify: map[string]bool{}, quick: ti, w: 100, h: 30,
		folded: map[string]bool{}, resized: map[string]resizeInfo{},
		scroll: map[string]int{}, hist: map[string]int{}, clip: NewClipboard(), mouseOn: true}
	switch {
	case opts.Pane != nil:
		m.io = opts.Pane
	case opts.Tmux != nil:
		m.io = opts.Tmux
	}
	if m.io != nil {
		m.snd = newSender(m.io)
	}
	m.vp = viewport.New(100, 28)
	m.agents = orderAgents(opts.Team, nil)
	m.rebuild()
	m.setSel(defaultTarget(m.agents))
	if opts.IsPaused != nil {
		m.paused = opts.IsPaused()
	}
	return m
}

func (m *Model) session(agent string) string {
	if m.opts.SessionName != nil {
		return m.opts.SessionName(agent)
	}
	for _, a := range m.agents {
		if a.ID == agent && a.TmuxSession != "" {
			return a.TmuxSession
		}
	}
	return tmux.SessionName(m.opts.Dir, agent)
}

func (m *Model) Init() tea.Cmd {
	m.refreshBusy = true
	return tea.Batch(m.refreshCmd(), m.previewKick(), m.resizeSel(true), tick())
}

func (m *Model) previewKick() tea.Cmd {
	if m.previewBusy || m.sel == "" {
		return nil
	}
	c := m.previewCmd(m.sel)
	if c != nil {
		m.previewBusy = true
	}
	return c
}

// ---- selection / list ----

func (m *Model) rebuild() {
	m.items = buildItems(m.agents, m.folded)
	for i := range m.items {
		m.items[i].paused = m.items[i].agent != "" && m.heldAgents[m.items[i].agent]
	}
	if m.cur >= len(m.items) {
		m.cur = max(len(m.items)-1, 0)
	}
	// keep the cursor on the selected agent when the order changed
	if m.cur < len(m.items) && !m.items[m.cur].isHeader() && m.items[m.cur].agent != m.sel {
		m.cursorTo(m.sel)
	}
}

// attachCurrent attaches the full tmux view of the agent under the cursor.
func (m *Model) attachCurrent() tea.Cmd {
	a, ok := m.curAgent()
	switch {
	case !ok:
		m.setStatus("select an agent first", true)
	case a.Status == "dead" || a.Status == "errored":
		m.setStatus(a.ID+" is not running — press v to revive it", true)
	case m.opts.Tmux == nil:
		m.setStatus("no tmux client", true)
	default:
		return m.bindDetachCmd(m.session(a.ID))
	}
	return nil
}

// jumpToNth handles a digit key: the nth visible agent (the digit shown at the
// start of its row) is selected; pressing the digit of the agent that is already
// selected attaches to it.
func (m *Model) jumpToNth(n int) tea.Cmd {
	i := 0
	for idx, it := range m.items {
		if it.isHeader() {
			continue
		}
		if i++; i != n {
			continue
		}
		if it.agent == m.sel && m.cur == idx {
			return m.attachCurrent()
		}
		m.cur = idx
		cmd := m.selectAgent(it.agent)
		m.ensureVisible()
		return cmd
	}
	m.setStatus(fmt.Sprintf("no agent %d", n), false)
	return nil
}

// quickToOrchestrator opens the quick prompt addressed to the orchestrator.
func (m *Model) quickToOrchestrator() tea.Cmd {
	orch := m.orchestratorID()
	if orch == "" {
		return nil
	}
	for _, a := range m.agents {
		if a.ID == orch && (a.Status == "dead" || a.Status == "errored") {
			m.setStatus(orch+" is not running — press v to revive it", true)
			return nil
		}
	}
	m.cursorTo(orch)
	sel := m.selectAgent(orch)
	m.ensureVisible()
	m.mode, m.quickTo = mQuick, orch
	m.quick.SetValue("")
	m.quick.Focus()
	return tea.Batch(sel, textinput.Blink)
}

func (m *Model) cursorTo(agent string) {
	for i, it := range m.items {
		if it.agent == agent {
			m.cur = i
			return
		}
	}
}

func (m *Model) setSel(id string) {
	m.sel = id
	m.cursorTo(id)
	m.ensureVisible()
}

func (m *Model) selAgent() (store.Agent, bool) {
	for _, a := range m.agents {
		if a.ID == m.sel {
			return a, true
		}
	}
	return store.Agent{}, false
}

// curAgent is the agent under the cursor (false when the cursor is on a group header).
func (m *Model) curAgent() (store.Agent, bool) {
	if m.cur < 0 || m.cur >= len(m.items) || m.items[m.cur].isHeader() {
		return store.Agent{}, false
	}
	for _, a := range m.agents {
		if a.ID == m.items[m.cur].agent {
			return a, true
		}
	}
	return store.Agent{}, false
}

func (m *Model) setStatus(s string, isErr bool) {
	m.status, m.statusE, m.statAt = s, isErr, m.opts.Now()
}

func (m *Model) lineOf(i int) int {
	n := 0
	for j := 0; j < i && j < len(m.items); j++ {
		if m.items[j].isHeader() {
			n++
		} else {
			n += 2
		}
	}
	return n
}

func (m *Model) listViewH() int { return max(m.layout().bodyH-2, 1) }

func (m *Model) ensureVisible() {
	if m.cur < 0 || m.cur >= len(m.items) {
		return
	}
	first := m.lineOf(m.cur)
	last := first
	if !m.items[m.cur].isHeader() {
		last++
	}
	vh := m.listViewH()
	if first < m.listTop {
		m.listTop = first
	}
	if last >= m.listTop+vh {
		m.listTop = last - vh + 1
	}
	total := m.lineOf(len(m.items))
	if m.listTop > max(total-vh, 0) {
		m.listTop = max(total-vh, 0)
	}
	if m.listTop < 0 {
		m.listTop = 0
	}
}

// moveCursor moves the list cursor; landing on an agent selects it.
func (m *Model) moveCursor(d int) tea.Cmd {
	if len(m.items) == 0 {
		return nil
	}
	m.cur = clamp(m.cur+d, 0, len(m.items)-1)
	return m.cursorMoved()
}

func (m *Model) cursorMoved() tea.Cmd {
	m.ensureVisible()
	it := m.items[m.cur]
	if it.isHeader() {
		return nil
	}
	return m.selectAgent(it.agent)
}

func (m *Model) selectAgent(id string) tea.Cmd {
	if id == m.sel {
		return nil
	}
	m.sel = id
	m.snap, m.snapFor, m.snapErr = tmux.Snapshot{}, "", ""
	cmds := []tea.Cmd{}
	if a, ok := m.selAgent(); ok && a.Status == "finished" {
		for i := range m.agents {
			if m.agents[i].ID == id {
				m.agents[i].Status = "idle"
			}
		}
		cmds = append(cmds, m.clearFinishedCmd(id))
	}
	cmds = append(cmds, m.previewKick(), m.resizeSel(true))
	return tea.Batch(cmds...)
}

// resizeSel sizes the selected agent's tmux window to the preview pane.
func (m *Model) resizeSel(force bool) tea.Cmd {
	if m.sel == "" || m.io == nil {
		return nil
	}
	pw, ph := m.previewSize()
	r := m.resized[m.sel]
	if !force && r.w == pw && r.h == ph {
		return nil
	}
	if !force && m.opts.Now().Sub(r.at) < 700*time.Millisecond {
		return nil
	}
	m.resized[m.sel] = resizeInfo{pw, ph, m.opts.Now()}
	return m.resizeCmd(m.sel, pw, ph)
}

// nextWaiting returns the next agent (after the selection, wrapping) that needs the user.
func (m *Model) nextWaiting() (string, bool) {
	order := buildItemsAll(m.agents)
	at := m.sel
	if a, ok := m.curAgent(); ok {
		at = a.ID
	}
	start := 0
	for i, id := range order {
		if id == at {
			start = i + 1
		}
	}
	for k := 0; k < len(order); k++ {
		id := order[(start+k)%len(order)]
		for _, a := range m.agents {
			if a.ID == id && isWaiting(a.Status) && !(id == at && k == len(order)-1 && !m.cursorOnHeader()) {
				return id, true
			}
		}
	}
	return "", false
}

func (m *Model) cursorOnHeader() bool {
	return m.cur >= 0 && m.cur < len(m.items) && m.items[m.cur].isHeader()
}

// buildItemsAll lists agent ids in display order, ignoring folds.
func buildItemsAll(agents []store.Agent) []string {
	var out []string
	for _, it := range buildItems(agents, nil) {
		if !it.isHeader() {
			out = append(out, it.agent)
		}
	}
	return out
}

// currentTask returns the task in context: the Tasks overlay cursor, else the
// selected agent's current task.
func (m *Model) currentTask() (store.Task, bool) {
	if m.ov == ovTasks {
		rows := boardRows(m.tasks)
		if m.tasksCur >= 0 && m.tasksCur < len(rows) {
			return rows[m.tasksCur], true
		}
		return store.Task{}, false
	}
	if a, ok := m.selAgent(); ok && a.CurrentTask != "" {
		for _, t := range m.tasks {
			if t.ID == a.CurrentTask {
				return t, true
			}
		}
	}
	return store.Task{}, false
}

func bell() tea.Msg {
	fmt.Fprint(os.Stderr, "\a")
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func clamp(v, lo, hi int) int { return max(lo, min(v, hi)) }

// ---- update ----

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.vp.Width, m.vp.Height = m.w, max(m.h-2, 1)
		m.quick.Width = max(min(m.w-20, 80), 10)
		m.ensureVisible()
		return m, m.resizeSel(true)

	case orphansMsg:
		m.onOrphans(msg)
		return m, nil

	case tickMsg:
		m.tickN++
		cmds := []tea.Cmd{tick()}
		if c := m.orphanPollCmd(); c != nil {
			cmds = append(cmds, c)
		}
		if m.tickN%10 == 1 && !m.refreshBusy {
			if c := m.refreshCmd(); c != nil {
				m.refreshBusy = true
				cmds = append(cmds, c)
			}
		}
		// The supervisor holds wake-ups only while the user is actually typing; an
		// idle focused agent must still receive its work.
		if m.mode == mFocus && m.tickN%10 == 0 && m.opts.Now().Sub(m.lastKeyAt) < focusTypingWindow {
			if c := m.focusBeatCmd(m.sel); c != nil {
				cmds = append(cmds, c)
			}
		}
		if m.mode == mFocus || m.tickN%5 == 0 {
			if c := m.previewKick(); c != nil {
				cmds = append(cmds, c)
			}
		}
		return m, tea.Batch(cmds...)

	case fastMsg:
		m.fastPending = false
		return m, m.previewKick()

	case refreshMsg:
		m.refreshBusy = false
		if msg.err != nil {
			m.setStatus("store: "+msg.err.Error(), true)
			return m, nil
		}
		next, ring := newlyWaiting(m.notify, msg.agents)
		m.notify = next
		m.agents = orderAgents(m.opts.Team, msg.agents)
		m.tasks, m.events, m.spend, m.warn = msg.tasks, msg.events, msg.spend, msg.warn
		m.month, m.usage = msg.month, msg.usage
		m.heldAgents = msg.paused
		m.health = msg.health
		m.agentSpend = msg.agentSpend
		m.runDone = msg.runDone
		if msg.statsAsked {
			m.stats, m.statsOK, m.statsErr = msg.stats, msg.statsErr == nil, ""
			if msg.statsErr != nil {
				m.statsErr = msg.statsErr.Error()
			}
		}
		if m.opts.IsPaused != nil {
			m.paused = m.opts.IsPaused()
		}
		m.holdName, m.holdSince, m.held = "", 0, false
		if m.opts.CurrentHold != nil {
			m.holdName, m.holdSince, m.held = m.opts.CurrentHold()
		}
		if _, ok := m.selAgent(); !ok {
			m.sel = defaultTarget(m.agents)
		}
		m.rebuild()
		var clear tea.Cmd
		if a, ok := m.selAgent(); ok && a.Status == "finished" && m.mode == mFocus {
			// you are looking at it: it is not "done, unread" any more
			for i := range m.agents {
				if m.agents[i].ID == a.ID {
					m.agents[i].Status = "idle"
				}
			}
			m.rebuild()
			clear = m.clearFinishedCmd(a.ID)
		}
		rows := boardRows(m.tasks)
		if m.tasksFollow != "" {
			for i, t := range rows {
				if t.ID == m.tasksFollow {
					m.tasksCur = i
				}
			}
			m.tasksFollow = ""
		}
		if n := len(rows); m.tasksCur >= n {
			m.tasksCur = max(n-1, 0)
		}
		m.ensureVisible()
		if len(ring) > 0 {
			return m, tea.Batch(bell, clear)
		}
		return m, clear

	case previewMsg:
		m.previewBusy = false
		if msg.agent != m.sel {
			return m, m.previewKick()
		}
		if msg.err != nil {
			m.snapErr = firstLine(msg.err.Error())
			m.snap, m.snapFor = tmux.Snapshot{}, msg.agent
			return m, nil
		}
		m.snapErr = ""
		m.snap, m.snapFor = msg.snap, msg.agent
		if m.syncScroll(msg.agent, msg.snap) {
			return m, m.previewKick() // the view we asked for is stale: fetch the right window
		}
		// self-heal: the window must match the preview pane exactly
		pw, ph := m.previewSize()
		if msg.snap.W != pw || msg.snap.H != ph {
			return m, m.resizeSel(false)
		}
		return m, nil

	case resizedMsg:
		if msg.err == nil && msg.agent == m.sel {
			return m, m.previewKick()
		}
		return m, nil

	case copiedMsg:
		if msg.err != nil {
			m.setStatus(msg.err.Error(), true)
		} else {
			m.setStatus(fmt.Sprintf("copied %d %s", msg.lines, plural(msg.lines, "line", "lines")), false)
		}
		return m, nil

	case actionMsg:
		if msg.err != nil {
			m.setStatus(msg.err.Error(), true)
		} else {
			m.setStatus(msg.text, false)
		}
		if msg.followTask != "" {
			m.tasksFollow = msg.followTask
		}
		return m, m.refreshSoon()

	case attachReadyMsg:
		c := m.opts.Tmux.AttachCmd(msg.session)
		return m, tea.ExecProcess(c, func(err error) tea.Msg { return attachDoneMsg{err} })

	case attachDoneMsg:
		if msg.err != nil {
			m.setStatus("attach: "+msg.err.Error(), true)
		}
		return m, tea.Batch(m.previewKick(), m.resizeSel(true), m.refreshSoon())

	case memLoadedMsg:
		m.onMemLoaded(msg)
		return m, nil

	case memDetailMsg:
		m.onMemDetail(msg)
		return m, nil

	case memDoneMsg:
		return m, m.onMemDone(msg)

	case detailMsg:
		if m.ov != ovDetail && m.ov != ovDiff {
			m.ovFrom = m.ov
		}
		m.ov = ovDetail
		m.ovTitle = "task " + msg.task.ID
		m.vp.SetContent(renderDetail(msg.task, msg.spec, msg.history, msg.memory))
		m.vp.GotoTop()
		return m, nil

	case diffMsg:
		if msg.err != nil {
			m.setStatus("diff: "+msg.err.Error(), true)
			return m, nil
		}
		if m.ov != ovDetail && m.ov != ovDiff {
			m.ovFrom = m.ov
		}
		m.ov = ovDiff
		m.ovTitle = fmt.Sprintf("diff %s (%s) · %d files", msg.task.ID, msg.task.Branch, msg.files)
		if strings.TrimSpace(msg.patch) == "" {
			m.vp.SetContent(styleDim.Render("(empty diff)"))
		} else {
			m.vp.SetContent(colorizeDiff(msg.patch))
		}
		m.vp.GotoTop()
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		return m.handleMouse(msg)
	}
	return m, nil
}

func (m *Model) refreshSoon() tea.Cmd {
	if m.refreshBusy {
		return nil
	}
	c := m.refreshCmd()
	if c != nil {
		m.refreshBusy = true
	}
	return c
}

// focusTypingWindow is how long after the last forwarded keystroke the focus
// heartbeat keeps the supervisor from typing a wake-up line into the pane.
const focusTypingWindow = 4 * time.Second

// ---- keys ----

func (m *Model) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	hadSel := m.hl.active
	m.hl.active = false // any key clears the selection
	m.ms.pressed = false
	if hadSel && k.Type == tea.KeyEsc && !k.Alt && (m.mode == mFocus || m.mode == mList) && m.ov == ovNone {
		return m, nil // a plain Esc that clears a selection is consumed
	}
	if m.mode == mFocus {
		m.lastKeyAt = m.opts.Now()
		return m.focusKey(k)
	}
	key := k.String()
	if m.mode == mConfirm {
		switch key {
		case "y", "Y", "enter":
			c := m.confirm
			m.mode = mList
			m.setStatus(c.what+"ing "+c.agent+"…", false)
			return m, m.stopCmd(c.agent, c.what == "restart")
		default:
			m.mode = mList
			m.setStatus("cancelled", false)
		}
		return m, nil
	}
	if m.mode == mAddAgent {
		return m.addAgentKey(k)
	}
	if m.mode == mQuick {
		if m.completeKey(k) {
			return m, nil
		}
		switch key {
		case "esc":
			m.mode, m.quickRedirect, m.quickHold = mList, false, false
			m.quick.Blur()
			m.comp = completion{}
			return m, nil
		case "enter":
			text := strings.TrimSpace(m.quick.Value())
			redirect, hold := m.quickRedirect, m.quickHold
			m.mode, m.quickRedirect, m.quickHold = mList, false, false
			m.quick.Blur()
			if hold { // the name is optional
				return m, m.holdCmd(text)
			}
			if text == "" {
				return m, nil
			}
			if redirect {
				for _, a := range m.agents {
					if a.ID != m.quickTo {
						continue
					}
					if why := m.redirectBlock(a); why != "" { // it changed while the box was open
						m.setStatus("can't redirect: "+why, true)
						return m, nil
					}
					return m, m.redirectCmd(a, text)
				}
				return m, nil
			}
			wou := false
			for _, a := range m.agents {
				if a.ID == m.quickTo && a.Status == "waiting_on_user" {
					wou = true
				}
			}
			return m, m.sendCmd(m.quickTo, text, wou)
		case "ctrl+c":
			return m, tea.Quit
		}
		var cmd tea.Cmd
		m.quick, cmd = m.quick.Update(k)
		return m, cmd
	}
	if key == "ctrl+c" {
		return m, tea.Quit
	}
	if m.ov != ovNone {
		return m.overlayKey(k, key)
	}
	return m.listKey(k, key)
}

func (m *Model) enterFocus() tea.Cmd {
	a, ok := m.selAgent()
	if !ok {
		return nil
	}
	if a.Status == "dead" || a.Status == "errored" {
		m.setStatus(a.ID+" is not running — press v to revive it", true)
		return nil
	}
	if m.io == nil {
		m.setStatus("no tmux client", true)
		return nil
	}
	m.mode = mFocus
	m.status = ""
	m.lastKeyAt = m.opts.Now()
	return tea.Batch(m.resizeSel(true), m.previewKick(), m.focusBeatCmd(m.sel))
}

func (m *Model) focusKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if returnKey(k) {
		m.mode = mList
		m.setStatus("back to the agent list", false)
		return m, m.focusClearCmd(m.sel)
	}
	if m.scroll[m.sel] > 0 {
		// scrolled back: paging keys move the view instead of reaching the agent
		_, ph := m.previewSize()
		switch k.Type {
		case tea.KeyPgUp:
			return m, m.scrollTo(m.scroll[m.sel] + max(ph/2, 1))
		case tea.KeyPgDown:
			return m, m.scrollTo(m.scroll[m.sel] - max(ph/2, 1))
		case tea.KeyHome:
			return m, m.scrollTo(m.hist[m.sel])
		case tea.KeyEnd:
			return m, m.scrollTo(0)
		}
	}
	keys, paste, ok := translateKey(k)
	if !ok || m.snd == nil || m.sel == "" {
		return m, nil
	}
	m.scroll[m.sel] = 0 // typing returns to the live view
	m.snd.enqueue(sendItem{session: m.session(m.sel), keys: keys, paste: paste})
	if m.fastPending {
		return m, nil
	}
	m.fastPending = true
	return m, tea.Tick(35*time.Millisecond, func(time.Time) tea.Msg { return fastMsg{} })
}

func (m *Model) closeOverlay() {
	switch {
	case (m.ov == ovDetail || m.ov == ovDiff) && m.ovFrom != ovNone:
		m.ov, m.ovFrom = m.ovFrom, ovNone
	default:
		m.ov, m.ovFrom = ovNone, ovNone
	}
}

func (m *Model) overlayKey(k tea.KeyMsg, key string) (tea.Model, tea.Cmd) {
	switch m.ov {
	case ovHelp, ovUsage:
		m.ov = ovNone
		return m, nil
	case ovDetail, ovDiff:
		if key == "esc" || key == "q" {
			m.closeOverlay()
			return m, nil
		}
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(k)
		return m, cmd
	case ovStats:
		if key == "esc" || key == "q" || key == "s" {
			m.ov = ovNone
		}
		return m, nil
	case ovMemory:
		return m.memoryKey(k, key)
	case ovLog:
		switch key {
		case "esc", "q", "l":
			m.ov = ovNone
		case "h":
			m.showHooks = !m.showHooks
		}
		return m, nil
	case ovTasks:
		n := len(boardRows(m.tasks))
		switch key {
		case "esc", "q", "b":
			m.ov = ovNone
		case "up", "k":
			m.tasksCur = clamp(m.tasksCur-1, 0, max(n-1, 0))
		case "down", "j":
			m.tasksCur = clamp(m.tasksCur+1, 0, max(n-1, 0))
		case "pgup":
			m.tasksCur = clamp(m.tasksCur-10, 0, max(n-1, 0))
		case "pgdown":
			m.tasksCur = clamp(m.tasksCur+10, 0, max(n-1, 0))
		case "home", "g":
			m.tasksCur = 0
		case "end", "G":
			m.tasksCur = max(n-1, 0)
		case "enter", "t":
			if t, ok := m.currentTask(); ok {
				return m, m.detailCmd(t)
			}
		case "d":
			return m, m.diffKey()
		case "+", "=":
			return m, m.priorityCmd("up")
		case "-", "_":
			return m, m.priorityCmd("down")
		case "T":
			return m, m.priorityCmd("top")
		}
	}
	return m, nil
}

func (m *Model) diffKey() tea.Cmd {
	t, ok := m.currentTask()
	switch {
	case !ok:
		m.setStatus("no task selected", true)
	case t.Status != store.StatusInReview:
		m.setStatus(t.ID+" is not in review", true)
	case t.Branch == "":
		m.setStatus(t.ID+" has no branch", true)
	default:
		return m.diffCmd(t)
	}
	return nil
}

func (m *Model) listKey(k tea.KeyMsg, key string) (tea.Model, tea.Cmd) {
	if (key == "esc" || key == "x") && m.dismissBanner() {
		return m, nil
	}
	switch key {
	case "q":
		return m, tea.Quit
	case "?":
		m.ov = ovHelp
	case "esc":
		m.status = ""
	case "i":
		m.ov = ovUsage
	case "I": // agent info: harness, model, degraded features
		if a, ok := m.curAgent(); ok {
			if m.ov != ovDetail && m.ov != ovDiff {
				m.ovFrom = m.ov
			}
			m.ov = ovDetail
			m.ovTitle = "agent " + a.ID
			m.vp.SetContent(m.agentInfo(a))
			m.vp.GotoTop()
		}
	case "U":
		m.usageOff = !m.usageOff
		if m.usageOff {
			m.setStatus("usage strip hidden (U shows it)", false)
		} else {
			m.setStatus("usage strip shown", false)
		}
	case "m":
		m.mouseOn = !m.mouseOn
		m.hl.active = false
		if m.mouseOn {
			m.setStatus("mouse capture on: drag selects in the preview, wheel scrolls", false)
			return m, func() tea.Msg { return tea.EnableMouseCellMotion() }
		}
		m.setStatus("mouse capture off: use the terminal's own selection / scroll", false)
		return m, func() tea.Msg { return tea.DisableMouse() }
	case "up", "k":
		return m, m.moveCursor(-1)
	case "down", "j":
		return m, m.moveCursor(1)
	case "home":
		return m, m.moveCursor(-len(m.items))
	case "end":
		return m, m.moveCursor(len(m.items))
	case "left", "h":
		if len(m.items) == 0 {
			return m, nil
		}
		it := m.items[m.cur]
		if it.isHeader() {
			if !it.folded {
				m.toggleFold(it.group)
			}
			return m, nil
		}
		for i := m.cur; i >= 0; i-- { // jump to the group header
			if m.items[i].isHeader() {
				m.cur = i
				break
			}
		}
		m.ensureVisible()
	case "right", "enter":
		if len(m.items) == 0 {
			return m, nil
		}
		if it := m.items[m.cur]; it.isHeader() {
			m.toggleFold(it.group)
			return m, nil
		}
		return m, m.enterFocus()
	case " ", "space":
		if len(m.items) == 0 {
			return m, nil
		}
		a, ok := m.curAgent()
		if !ok { // no agent under the cursor (a group header): the prompt goes to the orchestrator
			return m, m.quickToOrchestrator()
		}
		if a.Status == "dead" || a.Status == "errored" {
			m.setStatus(a.ID+" is not running — press v to revive it", true)
			return m, nil
		}
		m.mode, m.quickTo = mQuick, a.ID
		m.quick.SetValue("")
		m.quick.Focus()
		return m, textinput.Blink
	case "o":
		return m, m.quickToOrchestrator()
	case "c":
		return m, m.openRedirect()
	case "n":
		return m, m.openAddAgent()
	case "g":
		if id, ok := m.nextWaiting(); ok {
			for _, a := range m.agents {
				if a.ID == id {
					m.folded[groupKeyFor(a.Role)] = false
				}
			}
			m.rebuild()
			m.cursorTo(id)
			return m, m.cursorMoved()
		}
		m.setStatus("nobody else needs you", false)
	case "b":
		m.ov = ovTasks
	case "l":
		m.ov = ovLog
	case "M":
		return m, m.openMemory()
	case "s":
		m.ov = ovStats
		return m, m.refreshCmd() // fetch the numbers now, not at the next tick
	case "u":
		a, ok := m.curAgent()
		switch {
		case !ok:
			m.setStatus("select an agent first", true)
		case !m.heldAgents[a.ID]:
			m.setStatus(a.ID+" is not paused", false)
		default:
			m.setStatus("resuming "+a.ID+"…", false)
			return m, m.resumeCmd(a.ID)
		}
	case "v":
		a, ok := m.curAgent()
		switch {
		case !ok:
			m.setStatus("select an agent first", true)
		case a.Status == "dead" || a.Status == "errored":
			m.setStatus("reviving "+a.ID+"…", false)
			return m, m.reviveCmd(a.ID)
		default:
			m.setStatus(a.ID+" is already running", true)
		}
	case "x", "R":
		a, ok := m.curAgent()
		if !ok {
			m.setStatus("select an agent first", true)
			return m, nil
		}
		what := "stop"
		if key == "R" {
			what = "restart"
		}
		if (what == "stop") && (a.Status == "dead" || a.Status == "errored") {
			m.setStatus(a.ID+" is not running", true)
			return m, nil
		}
		m.mode, m.confirm = mConfirm, confirmState{what, a.ID}
	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		return m, m.jumpToNth(int(key[0] - '0'))
	case "a":
		return m, m.attachCurrent()
	case "p":
		return m, m.pauseCmd()
	case "H":
		return m, m.holdKey()
	case "t":
		if t, ok := m.currentTask(); ok {
			return m, m.detailCmd(t)
		}
		m.setStatus("no task selected", true)
	case "d":
		return m, m.diffKey()
	}
	return m, nil
}

func (m *Model) toggleFold(group string) {
	m.folded[group] = !m.folded[group]
	m.rebuild()
	for i, it := range m.items {
		if it.isHeader() && it.group == group {
			m.cur = i
		}
	}
	m.ensureVisible()
}

// ---- mouse ----

func (m *Model) geom() mouseGeom {
	l := m.layout()
	g := mouseGeom{ListOn: l.showList, ListW: l.lw, PrevOn: l.showPreview, Top: l.top, BodyH: l.bodyH, PrevW: l.rw}
	if l.showList && l.showPreview {
		g.PrevX0 = l.lw
	}
	g.Focused = m.mode == mFocus
	if m.paneKnown() {
		g.AltScreen, g.MouseAny = m.snap.AltScreen, m.snap.MouseAny
	}
	return g
}

// paneKnown reports whether m.snap describes the selected agent's pane.
func (m *Model) paneKnown() bool { return m.snapFor != "" && m.snapFor == m.sel && m.snapErr == "" }

// wheelReportsPerNotch is how many wheel reports one wheel notch sends to an
// app that handles the mouse itself.
const wheelReportsPerNotch = 3

// forward types literal bytes / named keys into the selected agent's pane.
func (m *Model) forward(keys ...tmux.Key) tea.Cmd {
	if m.snd == nil || m.sel == "" || len(keys) == 0 {
		return nil
	}
	m.snd.enqueue(sendItem{session: m.session(m.sel), keys: keys})
	if m.fastPending {
		return nil
	}
	m.fastPending = true
	return tea.Tick(35*time.Millisecond, func(time.Time) tea.Msg { return fastMsg{} })
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func (m *Model) handleMouse(ev tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.ov == ovTasks {
		switch ev.Button {
		case tea.MouseButtonWheelUp:
			m.tasksCur = max(m.tasksCur-1, 0)
		case tea.MouseButtonWheelDown:
			m.tasksCur = clamp(m.tasksCur+1, 0, max(len(m.tasks)-1, 0))
		}
		return m, nil
	}
	if m.ov == ovDetail || m.ov == ovDiff {
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(ev)
		return m, cmd
	}
	if m.ov != ovNone || m.mode == mQuick || m.mode == mConfirm || m.mode == mAddAgent {
		return m, nil
	}
	var act mouseAct
	m.ms, act = mouseDispatch(ev, m.geom(), m.ms, m.opts.Now())
	l := m.layout()
	pw, _ := m.previewSize()
	switch act.Kind {
	case mkListWheel:
		total := m.lineOf(len(m.items))
		m.listTop = clamp(m.listTop+act.Delta, 0, max(total-m.listViewH(), 0))
	case mkPreviewWheel:
		m.hl.active = false
		return m, m.scrollTo(m.scroll[m.sel] - act.Delta) // wheel up (negative) = further back
	case mkFwdWheel:
		m.hl.active = false
		raw := wheelReports(act.Delta < 0, act.A, m.snap.MouseSGR, wheelReportsPerNotch)
		if raw == "" {
			return m, nil
		}
		return m, m.forward(tmux.Lit(raw))
	case mkFwdClick:
		raw := clickReports(act.Btn, act.A, m.snap.MouseSGR)
		m.hl.active = false
		if raw == "" {
			return m, nil
		}
		return m, m.forward(tmux.Lit(raw))
	case mkScrollKeys:
		m.hl.active = false
		name := "Down"
		if act.Delta < 0 {
			name = "Up"
		}
		return m, m.forward(tmux.Named(name), tmux.Named(name), tmux.Named(name))
	case mkListClick:
		if m.mode == mFocus {
			m.mode = mList
		}
		idx := m.listTop + (act.Y - l.top - 1)
		if act.Y-l.top-1 < 0 {
			return m, nil
		}
		ls := renderListH(m.items, -1, 10, m.agents, m.tasks, m.events, m.rowExtras(), false)
		if idx >= 0 && idx < len(ls) {
			i := ls[idx].item
			m.cur = i
			if m.items[i].isHeader() {
				m.toggleFold(m.items[i].group)
				return m, nil
			}
			return m, m.cursorMoved()
		}
	case mkPreviewFocus:
		m.hl.active = false
		if m.mode == mList {
			return m, m.enterFocus()
		}
	case mkSelBegin:
		m.hl.active = false
	case mkSelUpdate:
		m.hl = selection{true, act.A, act.B}
	case mkSelCopy:
		m.hl = selection{true, act.A, act.B}
		return m, m.copySelection(act.A, act.B, pw)
	case mkSelWord, mkSelLine:
		rows := m.plainRows()
		if act.A.Y < 0 || act.A.Y >= len(rows) {
			return m, nil
		}
		a, b := cell{0, act.A.Y}, cell{pw - 1, act.A.Y}
		if act.Kind == mkSelWord {
			c0, c1, ok := wordBounds(rows[act.A.Y], act.A.X)
			if !ok {
				return m, nil
			}
			a, b = cell{c0, act.A.Y}, cell{c1, act.A.Y}
		}
		m.hl = selection{true, a, b}
		return m, m.copySelection(a, b, pw)
	}
	return m, nil
}

// displayRows is what the preview shows, row by row (ANSI, padded to the pane
// width, no cursor / selection). nil while there is nothing to show.
func (m *Model) displayRows() []string {
	if m.snapFor == "" || m.snapFor != m.sel || m.snapErr != "" {
		return nil
	}
	pw, ph := m.previewSize()
	return previewRows(m.snap.Body, pw, ph)
}

func (m *Model) plainRows() []string {
	rows := m.displayRows()
	for i := range rows {
		rows[i] = ansi.Strip(rows[i])
	}
	return rows
}

// copySelection copies the text of a..b (from the displayed rows) to the clipboard.
func (m *Model) copySelection(a, b cell, pw int) tea.Cmd {
	text := extractSelection(m.plainRows(), a, b, pw)
	if strings.TrimSpace(text) == "" || m.clip == nil {
		return nil
	}
	n := strings.Count(text, "\n") + 1
	clip := m.clip
	return func() tea.Msg {
		_, err := clip.Copy(text)
		return copiedMsg{lines: n, err: err}
	}
}

type copiedMsg struct {
	lines int
	err   error
}

// ---- scrollback ----

// scrollTo sets the selected agent's offset (clamped to its known history) and
// refreshes the preview.
func (m *Model) scrollTo(off int) tea.Cmd {
	if m.sel == "" {
		return nil
	}
	m.scroll[m.sel] = scrollBy(0, off, m.hist[m.sel])
	m.hl.active = false
	return m.previewKick()
}

// syncScroll reconciles the scroll offset with a fresh snapshot. It returns
// true when the snapshot does not show the window we want (re-fetch needed).
func (m *Model) syncScroll(agent string, snap tmux.Snapshot) bool {
	prev, seen := m.hist[agent]
	m.hist[agent] = snap.HistorySize
	off := m.scroll[agent]
	if off > 0 && seen {
		off = followHistory(off, snap.HistorySize-prev, snap.HistorySize)
	}
	off = clamp(off, 0, snap.HistorySize)
	m.scroll[agent] = off
	return off != snap.Offset
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
