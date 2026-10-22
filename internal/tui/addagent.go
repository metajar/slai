package tui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/oorbt/slai/internal/config"
)

// The New Agent form (`n` in the agents list): fills in one agent and appends it to team.yaml.

const (
	fRole = iota
	fHarness
	fID
	fFocus
	fModel
	fPersona
	fPerm
)

const teamDefaultPerm = "(team default)"

type addForm struct {
	team *config.Team // team.yaml as read when the form opened (live validation, suggestions)

	role    int // index into config.Roles
	harn    int // index into config.Harnesses
	perm    int // 0 = team default, else config.PermissionModes[perm-1]
	text    [fPerm + 1]textinput.Model
	cur     int // index into visible()
	idDirty bool
	mdlDirty,
	permDirty bool
	err string
}

// lookPath finds a harness binary (a var so tests can fake the machine).
var lookPath = exec.LookPath

func teamPath(dir string) string { return filepath.Join(dir, ".slai", "team.yaml") }

func newAddForm(t *config.Team) *addForm {
	f := &addForm{team: t}
	for i := range f.text {
		ti := textinput.New()
		ti.Prompt = ""
		ti.Width = 32
		f.text[i] = ti
	}
	f.role = indexOf(config.Roles, config.RoleEngineer)
	f.harn = indexOf(config.Harnesses, config.HarnessClaude)
	f.applyRole()
	f.focusCur()
	f.revalidate()
	return f
}

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return 0
}

func (f *addForm) roleName() string    { return config.Roles[f.role] }
func (f *addForm) harnessName() string { return config.Harnesses[f.harn] }

// applyRole re-derives the defaults that follow the role until the user edits them.
func (f *addForm) applyRole() {
	r := f.roleName()
	if !f.idDirty {
		f.text[fID].SetValue(config.SuggestID(f.team, r))
	}
	if !f.mdlDirty {
		f.text[fModel].SetValue("")
		if f.harnessName() == config.HarnessClaude {
			f.text[fModel].SetValue(config.DefaultModelFor(r))
		}
	}
	f.text[fModel].Placeholder = config.ModelHint(f.harnessName())
	if !f.permDirty {
		f.perm = 0
		if config.IsWorkingRole(r) {
			f.perm = 1 + indexOf(config.PermissionModes, config.DefaultWorkingPermissionMode)
		}
	}
}

// visible lists the fields shown for the current role (Persona only for focus groups).
func (f *addForm) visible() []int {
	v := []int{fRole, fHarness, fID, fFocus, fModel}
	if f.roleName() == config.RoleFocusGroup {
		v = append(v, fPersona)
	}
	if f.harnessName() != config.HarnessPi { // pi has no permission layer to configure
		v = append(v, fPerm)
	}
	return v
}

func (f *addForm) field() int { return f.visible()[f.cur] }

func isSelect(field int) bool { return field == fRole || field == fHarness || field == fPerm }

func (f *addForm) focusCur() {
	for i := range f.text {
		f.text[i].Blur()
	}
	if fl := f.field(); !isSelect(fl) {
		f.text[fl].Focus()
	}
}

func (f *addForm) move(d int) {
	n := len(f.visible())
	f.cur = (f.cur + d + n) % n
	f.focusCur()
}

func (f *addForm) permName() string {
	if f.perm == 0 {
		return ""
	}
	return config.PermissionModes[f.perm-1]
}

func (f *addForm) agent() config.Agent {
	a := config.Agent{
		ID:             strings.TrimSpace(f.text[fID].Value()),
		Role:           f.roleName(),
		Model:          strings.TrimSpace(f.text[fModel].Value()),
		Focus:          strings.TrimSpace(f.text[fFocus].Value()),
		PermissionMode: f.permName(),
	}
	if h := f.harnessName(); h != config.HarnessClaude {
		a.Harness = h
	}
	if f.harnessName() == config.HarnessPi {
		a.PermissionMode = ""
	}
	if a.Role == config.RoleFocusGroup {
		a.Persona = strings.TrimSpace(f.text[fPersona].Value())
	}
	return a
}

// binaryProblem is set when the chosen non-claude harness is not installed
// (claude itself is checked by `slai up` / doctor preflight).
func (f *addForm) binaryProblem() error {
	if f.harnessName() == config.HarnessClaude {
		return nil
	}
	return config.CheckHarnessBinary(f.harnessName(), lookPath)
}

// revalidate checks the form against the team it was opened on.
func (f *addForm) revalidate() {
	f.err = ""
	if err := f.binaryProblem(); err != nil {
		f.err = err.Error() // the more fundamental problem: nothing else matters until it is installed
		return
	}
	probe := *f.team
	probe.Agents = append([]config.Agent(nil), f.team.Agents...)
	if err := config.AddAgent(&probe, f.agent()); err != nil {
		f.err = errLine(err)
	}
}

func errLine(err error) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(err.Error(), "\n", "; ")), " ")
}

// key handles one key; done reports the form should close (saved or cancelled).
func (f *addForm) key(k tea.KeyMsg) (cmd tea.Cmd, save, cancel bool) {
	switch s := k.String(); s {
	case "esc":
		return nil, false, true
	case "ctrl+s":
		return nil, true, false
	case "tab", "down":
		f.move(1)
		return nil, false, false
	case "shift+tab", "up":
		f.move(-1)
		return nil, false, false
	case "enter":
		if f.cur == len(f.visible())-1 {
			return nil, true, false
		}
		f.move(1)
		return nil, false, false
	}
	fl := f.field()
	if isSelect(fl) {
		d := 0
		switch k.String() {
		case "right", "l", " ":
			d = 1
		case "left", "h":
			d = -1
		}
		if d != 0 {
			switch fl {
			case fRole:
				f.role = (f.role + d + len(config.Roles)) % len(config.Roles)
				f.applyRole()
			case fHarness:
				f.harn = (f.harn + d + len(config.Harnesses)) % len(config.Harnesses)
				f.applyRole()
			default:
				n := len(config.PermissionModes) + 1
				f.perm = (f.perm + d + n) % n
				f.permDirty = true
			}
			if f.cur >= len(f.visible()) {
				f.cur = len(f.visible()) - 1
			}
			f.revalidate()
		}
		return nil, false, false
	}
	before := f.text[fl].Value()
	f.text[fl], cmd = f.text[fl].Update(k)
	if f.text[fl].Value() != before {
		switch fl {
		case fID:
			f.idDirty = true
		case fModel:
			f.mdlDirty = true
		}
		f.revalidate()
	}
	return cmd, false, false
}

var (
	styleLabel = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	styleMark  = lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true)
)

func (f *addForm) view() string {
	var rows []string
	row := func(fl int, label, val string) {
		mark := "  "
		if f.field() == fl {
			mark = styleMark.Render("▸ ")
		}
		rows = append(rows, mark+styleLabel.Render(fmt.Sprintf("%-11s", label))+val)
	}
	sel := func(s string) string { return "‹ " + s + " ›" }
	rows = append(rows, styleBold.Render("New agent"), "")
	row(fRole, "Role", sel(f.roleName()))
	row(fHarness, "Harness", sel(f.harnessName()))
	row(fID, "ID", f.text[fID].View())
	row(fFocus, "Focus", f.text[fFocus].View())
	row(fModel, "Model", f.text[fModel].View())
	if f.roleName() == config.RoleFocusGroup {
		row(fPersona, "Persona", f.text[fPersona].View())
	}
	if f.harnessName() != config.HarnessPi {
		perm := teamDefaultPerm
		if f.perm > 0 {
			perm = f.permName()
		}
		row(fPerm, "Permission", sel(perm))
	}
	rows = append(rows, styleDim.Render("  model: "+config.ModelHint(f.harnessName())), "")
	if f.err != "" {
		rows = append(rows, styleErr.Render(ansiTrunc(f.err, 64)))
	} else {
		rows = append(rows, styleDim.Render("ok — ctrl+s saves to team.yaml"))
	}
	rows = append(rows, styleDim.Render("tab/↑↓ move · ←→/space change · enter next/save · esc cancel"))
	return paneBorderFocus.Padding(0, 2).Render(strings.Join(rows, "\n"))
}

func ansiTrunc(s string, w int) string {
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	return string(r[:w-1]) + "…"
}

// ---- model wiring ----

// openAddAgent starts the form on the current team.yaml (falling back to the loaded team).
func (m *Model) openAddAgent() tea.Cmd {
	t, err := m.loadTeam()
	if err != nil {
		m.setStatus("cannot add an agent: "+errLine(err), true)
		return nil
	}
	m.form = newAddForm(t)
	m.mode = mAddAgent
	return textinput.Blink
}

// loadTeam reads team.yaml afresh so edits made meanwhile are kept.
func (m *Model) loadTeam() (*config.Team, error) {
	if m.opts.Dir != "" {
		if _, err := os.Stat(teamPath(m.opts.Dir)); err == nil {
			return config.Load(teamPath(m.opts.Dir))
		}
	}
	if m.opts.Team != nil {
		c := *m.opts.Team
		c.Agents = append([]config.Agent(nil), m.opts.Team.Agents...)
		return &c, nil
	}
	return nil, fmt.Errorf("no team.yaml")
}

func (m *Model) addAgentKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if k.String() == "ctrl+c" {
		return m, tea.Quit
	}
	cmd, save, cancel := m.form.key(k)
	switch {
	case cancel:
		m.form, m.mode = nil, mList
	case save:
		m.saveAddAgent()
	}
	return m, cmd
}

// saveAddAgent re-reads team.yaml, appends the agent and rewrites the file; on
// any problem the form stays open with the error and nothing is written.
func (m *Model) saveAddAgent() {
	f := m.form
	t, err := m.loadTeam()
	if err != nil {
		f.err = errLine(err)
		return
	}
	a := f.agent()
	if err := f.binaryProblem(); err != nil {
		f.err = err.Error() // a missing binary blocks the save
		return
	}
	if err := config.AddAgent(t, a); err != nil {
		f.team = t
		f.err = errLine(err)
		return
	}
	if m.opts.Dir == "" {
		f.err = "no project directory: cannot write team.yaml"
		return
	}
	if err := config.Write(teamPath(m.opts.Dir), t); err != nil {
		f.err = errLine(err)
		return
	}
	m.opts.Team = t
	m.form, m.mode = nil, mList
	// Only promise a start when something actually reconciled the supervisor's team.
	if m.opts.TeamChanged != nil {
		if err := m.opts.TeamChanged(t); err == nil {
			m.setStatus(a.ID+" added; the supervisor will start it within a few seconds", false)
			return
		}
	}
	m.setStatus(a.ID+" added to team.yaml; run `slai init --edit` to start it (or restart slai)", false)
}
