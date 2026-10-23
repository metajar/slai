package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/git"
	"github.com/oorbt/slai/internal/tmux"
)

type initFlags struct {
	edit                      bool
	yes                       bool
	preset, name, description string
	setModels                 []string // id=model (non-interactive edit)
	permissionMode            string
	checkModels               bool // run `claude -p` against each chosen model
	noRestart                 bool // --edit: do not reconcile live agents
}

func initCmd() *cobra.Command {
	var f initFlags
	c := &cobra.Command{Use: "init", Short: "Run the team setup wizard", Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error { return runInitWith(c.Context(), f) }}
	c.Flags().BoolVar(&f.edit, "edit", false, "edit the existing team")
	c.Flags().BoolVar(&f.yes, "yes", false, "non-interactive: accept defaults and flags")
	c.Flags().StringVar(&f.preset, "preset", "", "solo | squad | full (non-interactive)")
	c.Flags().StringVar(&f.name, "name", "", "project name")
	c.Flags().StringVar(&f.description, "description", "", "project description")
	c.Flags().StringArrayVar(&f.setModels, "set-model", nil, "agent=model override (repeatable; with --edit --yes)")
	c.Flags().StringVar(&f.permissionMode, "permission-mode", "", "set defaults.permission_mode (with --yes)")
	c.Flags().BoolVar(&f.checkModels, "check-models", false, "test each chosen model with a one-line claude -p call (warn only)")
	c.Flags().BoolVar(&f.noRestart, "no-restart", false, "with --edit: do not start/stop/restart live agents")
	return c
}

func runInit(ctx context.Context, edit bool) error { return runInitWith(ctx, initFlags{edit: edit}) }

func runInitWith(ctx context.Context, f initFlags) error {
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	repo := &git.Repo{Dir: dir}
	if err := preflight(ctx, repo, f.yes); err != nil {
		return err
	}
	teamPath := filepath.Join(dir, ".slai", "team.yaml")
	if _, err := os.Stat(teamPath); err == nil && !f.edit {
		return fmt.Errorf("%s already exists; use `slai init --edit`", teamPath)
	}
	main, _ := repo.DetectMainBranch(ctx)
	if main == "" {
		main = "main"
	}
	if f.name == "" {
		f.name = filepath.Base(dir)
	}
	var team *config.Team
	if f.edit {
		if team, err = config.Load(teamPath); err != nil {
			return err
		}
	}
	if f.yes {
		if f.edit && f.preset == "" {
			// keep the existing team; only apply explicit overrides
		} else {
			if f.preset == "" {
				f.preset = "squad"
			}
			if team = config.Preset(f.preset, f.name, f.description, main); team == nil {
				return fmt.Errorf("unknown preset %q (want %v)", f.preset, config.PresetNames)
			}
		}
		if err := applyEditFlags(team, f); err != nil {
			return err
		}
	} else if team, err = wizard(f, team, main); err != nil {
		return err
	}
	team.ApplyDefaults()
	if err := team.Validate(); err != nil {
		return err
	}
	if f.yes && f.checkModels {
		checkModels(ctx, team, os.Stderr)
	}
	if team.Defaults.PermissionMode == "bypassPermissions" && f.yes {
		fmt.Fprintln(os.Stderr, "warning: bypassPermissions skips all Claude Code prompts for every agent")
	}
	if err := os.MkdirAll(filepath.Join(dir, ".slai", "specs"), 0o755); err != nil {
		return err
	}
	if err := config.Write(teamPath, team); err != nil {
		return err
	}
	// Runtime state is ignored through .git/info/exclude, not .gitignore: an untracked
	// .gitignore we created would later block merging a branch that commits its own
	// (git refuses to overwrite untracked files), and a tracked one would show as modified.
	if err := ensureExclude(dir); err != nil {
		return err
	}
	fmt.Printf("wrote %s (%d agents). Next: run `slai`\n", teamPath, len(team.Agents))
	fmt.Println(initCostNotice(team))
	printSameReviewerLine(os.Stdout, team)
	if f.edit && !f.noRestart {
		return reconcileLive(ctx, os.Stdout)
	}
	return nil
}

// applyEditFlags applies --set-model and --permission-mode to team.
func applyEditFlags(team *config.Team, f initFlags) error {
	for _, kv := range f.setModels {
		id, model, ok := strings.Cut(kv, "=")
		if !ok || id == "" || model == "" {
			return fmt.Errorf("--set-model wants agent=model, got %q", kv)
		}
		found := false
		for i := range team.Agents {
			if team.Agents[i].ID == id {
				team.Agents[i].Model, found = model, true
			}
		}
		if !found {
			return fmt.Errorf("--set-model: unknown agent %q", id)
		}
	}
	if f.permissionMode != "" {
		team.Defaults.PermissionMode = f.permissionMode
	}
	return nil
}

// reconcileLive restarts changed agents if this project has live sessions.
func reconcileLive(ctx context.Context, w io.Writer) error {
	s, err := load(ctx)
	if err != nil {
		return err
	}
	defer s.Store.Close()
	live, err := s.Tmux.ListSessions(ctx, tmux.SessionName(s.Dir, ""))
	if err != nil || len(live) == 0 {
		return err
	}
	p, err := s.Reconcile(ctx)
	fmt.Fprintf(w, "reconciled live agents: started %v, restarted %v, stopped %v\n", p.Start, p.Restart, p.Stop)
	return err
}

// modelProbe runs one cheap claude call against model; swapped in tests.
var modelProbe = func(ctx context.Context, model string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "claude", "-p", "ok", "--model", model, "--no-session-persistence").CombinedOutput()
	if ctx.Err() != nil {
		return fmt.Errorf("timed out after 20s")
	}
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// checkModels probes each distinct resolved model and warns (never fails) on errors.
// Returns the models that failed.
func checkModels(ctx context.Context, t *config.Team, w io.Writer) []string {
	var bad []string
	seen := map[string]bool{}
	for _, a := range t.Agents {
		if a.HarnessKind() != config.HarnessClaude {
			continue // the claude probe means nothing for another harness's models
		}
		m := t.ResolveModel(a.Model)
		if seen[m] {
			continue
		}
		seen[m] = true
		if err := modelProbe(ctx, m); err != nil {
			fmt.Fprintf(w, "warning: model %q failed a test call: %v (continuing)\n", m, err)
			bad = append(bad, m)
		} else {
			fmt.Fprintf(w, "model %q ok\n", m)
		}
	}
	return bad
}

func preflight(ctx context.Context, repo *git.Repo, yes bool) error {
	var missing []string
	for _, b := range []string{"tmux", "git", "claude"} {
		if _, err := exec.LookPath(b); err != nil {
			missing = append(missing, b)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing on PATH: %s (run `slai doctor` for fixes)", strings.Join(missing, ", "))
	}
	if !repo.IsRepo(ctx) {
		ok := yes
		if !yes {
			if err := huh.NewConfirm().Title("This directory is not a git repo. Run git init?").Value(&ok).Run(); err != nil {
				return err
			}
		}
		if !ok {
			return fmt.Errorf("slai needs a git repository")
		}
		if err := repo.InitRepo(ctx); err != nil {
			return err
		}
	}
	// Branches and worktrees need at least one commit.
	if err := exec.CommandContext(ctx, "git", "-C", repo.Dir, "rev-parse", "--verify", "HEAD").Run(); err != nil {
		cmd := exec.CommandContext(ctx, "git", "-C", repo.Dir, "commit", "--allow-empty", "-m", "slai: initial commit")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("creating initial commit: %w: %s", err, out)
		}
	}
	return nil
}

func wizard(f initFlags, existing *config.Team, main string) (*config.Team, error) {
	name, desc, mainBranch, preset := f.name, f.description, main, "squad"
	if existing != nil {
		name, desc, mainBranch = existing.Project.Name, existing.Project.Description, existing.Project.MainBranch
	}
	err := huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("Project name").Value(&name),
		huh.NewText().Title("What is the product? (fed to every agent)").Value(&desc),
		huh.NewInput().Title("Main branch").Value(&mainBranch),
		huh.NewSelect[string]().Title("Team preset").Options(
			huh.NewOption("Squad — orchestrator, PM, principal, 2 engineers, focus group", "squad"),
			huh.NewOption("Solo+ — orchestrator + 1 engineer", "solo"),
			huh.NewOption("Full team — 4 engineers", "full"),
			huh.NewOption("Custom — start from squad and prune", "custom"),
		).Value(&preset),
	)).Run()
	if err != nil {
		return nil, err
	}
	base := preset
	if base == "custom" {
		base = "squad"
	}
	team := config.Preset(base, name, desc, mainBranch)
	if existing != nil {
		team.Agents, team.Models, team.Defaults = existing.Agents, existing.Models, existing.Defaults
	}
	if preset == "custom" {
		if err := pruneAgents(team); err != nil {
			return nil, err
		}
	}
	if err := assignModels(team); err != nil {
		return nil, err
	}
	if err := limits(team); err != nil {
		return nil, err
	}
	test := f.checkModels
	if !test {
		if err := huh.NewConfirm().Title("Test each chosen model with a one-line claude call? (uses a few tokens)").Value(&test).Run(); err != nil {
			return nil, err
		}
	}
	if test {
		checkModels(context.Background(), team, os.Stderr)
	}
	return team, nil
}

func pruneAgents(t *config.Team) error {
	var opts []huh.Option[string]
	var keep []string
	for _, a := range t.Agents {
		opts = append(opts, huh.NewOption(fmt.Sprintf("%s (%s)", a.ID, a.Role), a.ID).Selected(true))
		keep = append(keep, a.ID)
	}
	if err := huh.NewMultiSelect[string]().Title("Roles to keep (orchestrator is required)").Options(opts...).Value(&keep).Run(); err != nil {
		return err
	}
	set := map[string]bool{}
	for _, k := range keep {
		set[k] = true
	}
	var out []config.Agent
	for _, a := range t.Agents {
		if set[a.ID] || a.Role == config.RoleOrchestrator {
			out = append(out, a)
		}
	}
	t.Agents = out
	return nil
}

func assignModels(t *config.Team) error {
	var other bool
	if err := huh.NewConfirm().Title("Run any agents on OpenCode or Pi instead of Claude?").
		Description("Optional. Default is Claude for everyone; you can change one agent's harness later with the TUI n form or team.yaml.").
		Value(&other).Run(); err != nil {
		return err
	}
	harnesses := make([]string, len(t.Agents))
	for i := range harnesses {
		harnesses[i] = config.HarnessClaude
	}
	if other {
		var fields []huh.Field
		for i := range t.Agents {
			a := t.Agents[i]
			fields = append(fields, huh.NewSelect[string]().Title(fmt.Sprintf("Harness for %s (%s)", a.ID, a.Role)).
				Options(harnessOptions(exec.LookPath)...).Value(&harnesses[i]).
				Validate(func(h string) error { return harnessInstalled(h, exec.LookPath) }))
		}
		if err := huh.NewForm(huh.NewGroup(fields...)).Run(); err != nil {
			return err
		}
	}
	var fields []huh.Field
	for i := range t.Agents {
		a := &t.Agents[i]
		if harnesses[i] != config.HarnessClaude {
			a.Harness, a.Model = harnesses[i], ""
			fields = append(fields, huh.NewInput().Title(fmt.Sprintf("Model for %s (%s on %s)", a.ID, a.Role, a.Harness)).
				Description(config.ModelHint(a.Harness)).Value(&a.Model).Validate(validProviderModel))
			continue
		}
		a.Harness = ""
		fields = append(fields, huh.NewSelect[string]().Title(fmt.Sprintf("Model for %s (%s)", a.ID, a.Role)).
			Options(huh.NewOptions("opus", "sonnet", "haiku", "fable")...).Value(&a.Model))
	}
	return huh.NewForm(huh.NewGroup(fields...)).Run()
}

// harnessOptions lists the harnesses for the wizard, marking the ones whose
// binary is missing so the user sees why choosing them is rejected.
func harnessOptions(lookPath func(string) (string, error)) []huh.Option[string] {
	var opts []huh.Option[string]
	for _, h := range config.Harnesses {
		label := h
		if harnessInstalled(h, lookPath) != nil {
			label += " (not found on PATH)"
		}
		opts = append(opts, huh.NewOption(label, h))
	}
	return opts
}

// harnessInstalled is nil for claude (preflight covers it) and for any harness
// whose binary is on PATH.
func harnessInstalled(kind string, lookPath func(string) (string, error)) error {
	if kind == config.HarnessClaude {
		return nil
	}
	return config.CheckHarnessBinary(kind, lookPath)
}

// validProviderModel accepts "provider/model" with both sides non-empty.
func validProviderModel(v string) error {
	p, m, ok := strings.Cut(strings.TrimSpace(v), "/")
	if !ok || strings.TrimSpace(p) == "" || strings.TrimSpace(m) == "" {
		return fmt.Errorf("want provider/model, e.g. zai/glm-5.3")
	}
	return nil
}

func limits(t *config.Team) error {
	conc, turns := strconv.Itoa(t.Defaults.MaxConcurrentAgents), strconv.Itoa(t.Defaults.MaxTurnsPerTask)
	workMode := config.DefaultWorkingPermissionMode
	for _, a := range t.Agents {
		if config.IsWorkingRole(a.Role) && a.PermissionMode != "" {
			workMode = a.PermissionMode
			break
		}
	}
	err := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().Title("Permission mode for working agents (orchestrator, PM, principal, engineers)").
			Description("auto lets Claude's classifier approve routine commands (go test, git status) so agents don't stall; manual prompts for every command").
			Options(huh.NewOptions("auto", "acceptEdits", "manual", "plan")...).Value(&workMode),
		huh.NewInput().Title("Max concurrent agents").Value(&conc),
		huh.NewInput().Title("Max turns per task").Value(&turns),
		huh.NewConfirm().Title("Require principal review before merge?").Value(&t.Defaults.RequireReview),
		huh.NewSelect[string]().Title("Permission mode").Options(huh.NewOptions(config.PermissionModes...)...).Value(&t.Defaults.PermissionMode),
	)).Run()
	if err != nil {
		return err
	}
	for i := range t.Agents {
		if config.IsWorkingRole(t.Agents[i].Role) {
			t.Agents[i].PermissionMode = workMode
		}
	}
	t.Defaults.MaxConcurrentAgents, _ = strconv.Atoi(conc)
	t.Defaults.MaxTurnsPerTask, _ = strconv.Atoi(turns)
	if t.Defaults.PermissionMode == "bypassPermissions" {
		fmt.Fprintln(os.Stderr, "warning: bypassPermissions skips all Claude Code prompts for every agent")
	}
	return nil
}
