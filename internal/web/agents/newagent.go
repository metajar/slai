package agents

import (
	"errors"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/oorbt/slai/internal/config"
)

// lookPath finds a harness binary (a var so tests can fake the machine).
var lookPath = exec.LookPath

func (m *module) teamPath() string { return filepath.Join(m.d.Dir, ".slai", "team.yaml") }

// loadTeam reads team.yaml afresh, so edits made meanwhile are kept (the TUI does the same).
func (m *module) loadTeam() (*config.Team, error) {
	if m.d.Dir != "" {
		return config.Load(m.teamPath())
	}
	return nil, errors.New("no project directory: cannot read team.yaml")
}

// newAgentForm is the New Agent form's state: what the user typed and what is wrong.
type newAgentForm struct {
	View                            string
	Roles, Harnesses, Perms         []string
	Role, Harness, ID, Focus, Model string
	Persona, Perm                   string
	Err                             string
	ModelHint                       string
	Hints                           map[string]string
	ReadOnly                        bool
	Result                          string
}

const teamDefaultPerm = ""

func (m *module) blankForm(t *config.Team) newAgentForm {
	f := newAgentForm{View: viewNew, Roles: config.Roles, Harnesses: config.Harnesses, Perms: config.PermissionModes,
		Role: config.RoleEngineer, Harness: config.HarnessClaude, ReadOnly: m.d.ReadOnly}
	f.ID = config.SuggestID(t, f.Role)
	f.Model = config.DefaultModelFor(f.Role)
	f.Perm = config.DefaultWorkingPermissionMode
	f.ModelHint = config.ModelHint(f.Harness)
	return f
}

func (m *module) newForm(w http.ResponseWriter, r *http.Request) {
	t, err := m.loadTeam()
	if err != nil {
		m.d.Fail(w, r, err)
		return
	}
	m.render(w, r, "New agent", m.blankForm(t))
}

// showForm renders the form: the bare form for an htmx request (it replaces the old one),
// the whole page otherwise.
func (m *module) showForm(w http.ResponseWriter, r *http.Request, f newAgentForm, code int) {
	if r.Header.Get("HX-Request") != "true" {
		w.WriteHeader(code)
		m.render(w, r, "New agent", f)
		return
	}
	tpl, err := m.d.Template("agents")
	if err != nil {
		m.d.Fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = tpl.ExecuteTemplate(w, "newform", f)
}

func (m *module) newTarget(r *http.Request) string {
	_ = r.ParseForm()
	return strings.TrimSpace(r.PostForm.Get("id"))
}

// agentFrom builds the agent from the form exactly as the TUI form does (addForm.agent):
// a non-claude harness is recorded, pi has no permission mode, only focus groups have a
// persona, and an empty permission means the team default.
func agentFrom(f newAgentForm) config.Agent {
	a := config.Agent{ID: strings.TrimSpace(f.ID), Role: f.Role, Model: strings.TrimSpace(f.Model),
		Focus: strings.TrimSpace(f.Focus), PermissionMode: f.Perm}
	if f.Harness != config.HarnessClaude {
		a.Harness = f.Harness
	}
	if f.Harness == config.HarnessPi {
		a.PermissionMode = ""
	}
	if a.Role == config.RoleFocusGroup {
		a.Persona = strings.TrimSpace(f.Persona)
	}
	return a
}

// create validates the form against the current team.yaml and appends the agent through
// config.AddAgent + config.Write, the TUI form's own path. Any problem re-shows the form with
// the validator's message and writes nothing.
func (m *module) create(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		m.d.Fail(w, r, err)
		return
	}
	t, err := m.loadTeam()
	if err != nil {
		m.d.Fail(w, r, err)
		return
	}
	f := m.blankForm(t)
	f.Role, f.Harness = r.PostForm.Get("role"), r.PostForm.Get("harness")
	f.ID, f.Focus, f.Model = r.PostForm.Get("id"), r.PostForm.Get("focus"), r.PostForm.Get("model")
	f.Persona, f.Perm = r.PostForm.Get("persona"), r.PostForm.Get("perm")
	f.ModelHint = config.ModelHint(f.Harness)
	fail := func(err error) {
		f.Err = oneLine(err)
		m.showForm(w, r, f, http.StatusUnprocessableEntity)
	}
	a := agentFrom(f)
	if f.Harness != config.HarnessClaude {
		// a missing binary for another harness blocks the save (claude is checked by `slai up`)
		if err := config.CheckHarnessBinary(f.Harness, lookPath); err != nil {
			fail(err)
			return
		}
	}
	if err := config.AddAgent(t, a); err != nil {
		fail(err)
		return
	}
	if err := config.Write(m.teamPath(), t); err != nil {
		fail(err)
		return
	}
	m.d.OK(w, r, a.ID+" added to team.yaml. A running supervisor starts it within a few seconds; otherwise run `slai up`.", "/agents")
}

func oneLine(err error) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(err.Error(), "\n", "; ")), " ")
}
