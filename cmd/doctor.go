package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/oorbt/slai/internal/buildinfo"
	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/git"
	"github.com/oorbt/slai/internal/orphans"
	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/supervisor"
	"github.com/oorbt/slai/internal/tmux"
)

type check struct{ name, fix, ver string }

// Known-good Claude Code range (warn only outside it).
var (
	claudeMin = [3]int{2, 1, 0}
	claudeMax = [3]int{3, 0, 0} // exclusive
)

var verRe = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// parseVersion extracts the first x.y.z from s.
func parseVersion(s string) ([3]int, bool) {
	m := verRe.FindStringSubmatch(s)
	if m == nil {
		return [3]int{}, false
	}
	var v [3]int
	for i := range v {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	return v, true
}

func verLess(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// claudeVersionWarning returns "" when the version is inside the known-good range.
func claudeVersionWarning(out string) string {
	v, ok := parseVersion(out)
	if !ok {
		return "could not parse claude version"
	}
	if verLess(v, claudeMin) || !verLess(v, claudeMax) {
		return fmt.Sprintf("claude %d.%d.%d is outside the tested range >=%d.%d.%d,<%d.%d.%d; flags or hook payloads may differ",
			v[0], v[1], v[2], claudeMin[0], claudeMin[1], claudeMin[2], claudeMax[0], claudeMax[1], claudeMax[2])
	}
	return ""
}

var choicesRe = regexp.MustCompile(`(?s)--permission-mode\s+<mode>.*?\(choices:\s*([^)]*)\)`)

// permissionChoices parses the --permission-mode choices out of `claude --help`.
func permissionChoices(help string) []string {
	m := choicesRe.FindStringSubmatch(help)
	if m == nil {
		return nil
	}
	var out []string
	for _, c := range strings.Split(m[1], ",") {
		if c = strings.Trim(strings.TrimSpace(c), `"`); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// missingModes lists configured PermissionModes that claude does not accept.
func missingModes(choices []string) []string {
	var miss []string
	for _, m := range config.PermissionModes {
		found := false
		for _, c := range choices {
			found = found || c == m
		}
		if !found {
			miss = append(miss, m)
		}
	}
	return miss
}

func doctorCmd() *cobra.Command {
	var fix, panes, lane, all, asJSON bool
	c := &cobra.Command{Use: "doctor", Short: "Diagnose tmux, claude, git and stale state", Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) > 0 && !lane {
				return fmt.Errorf("unexpected argument %q (an agent id is only accepted with --lane)", args[0])
			}
			if asJSON && (lane || panes) {
				return fmt.Errorf("--json covers the checks only, not --lane or --panes")
			}
			if lane {
				only := ""
				if len(args) > 0 {
					only = args[0]
				}
				return laneFlagRun(c.OutOrStdout(), only)
			}
			if panes {
				return panesFlagRun(c.Context(), c.OutOrStdout())
			}
			dir, err := os.Getwd()
			if err != nil {
				return err
			}
			if asJSON {
				bad, err := doctorJSON(c.Context(), dir, fix, all, cliOrphanFinder(dir), c.OutOrStdout())
				if err != nil {
					return err
				}
				if bad > 0 {
					return fmt.Errorf("%d problem(s) found", bad)
				}
				return nil
			}
			bad, err := runDoctorChecks(c.Context(), dir, fix, all, cliOrphanFinder(dir), c.OutOrStdout())
			if err != nil {
				return err
			}
			if bad > 0 {
				return fmt.Errorf("%d problem(s) found", bad)
			}
			return nil
		}}
	c.Flags().BoolVar(&panes, "panes", false, "show what the pane detectors decide for each live agent, and which rule matched")
	c.Flags().BoolVar(&lane, "lane", false, "self-test each agent's lane guard offline with synthetic payloads (optionally name one agent)")
	c.Flags().BoolVar(&all, "all", false, "also list other slai teams whose supervisor is running (orphans are always listed)")
	c.Flags().BoolVar(&asJSON, "json", false, "machine-readable output: every check with its status, plus orphaned and other teams")
	c.Flags().BoolVar(&fix, "fix", false, "clean stale pidfile and kill orphan tmux sessions")
	return c
}

// runDoctorChecks is `slai doctor`: every check, printed to w, run for the project at dir.
// It returns the number of failures (warnings do not count). The CLI and the web UI's
// Doctor page both call it, so they report the same checks.
func runDoctorChecks(ctx context.Context, dir string, fix, all bool, f *orphans.Finder, w io.Writer) (int, error) {
	bad := 0
	for _, t := range []check{
		{"tmux", "install tmux (apt install tmux / brew install tmux)", "-V"},
		{"git", "install git", "--version"},
		{"claude", "install Claude Code: https://docs.claude.com/claude-code", "--version"},
	} {
		path, err := exec.LookPath(t.name)
		if err != nil {
			fmt.Fprintf(w, "✗ %-7s missing — %s\n", t.name, t.fix)
			bad++
			continue
		}
		out, _ := exec.Command(path, t.ver).CombinedOutput()
		fmt.Fprintf(w, "✓ %-7s %s\n", t.name, strings.TrimSpace(string(out)))
		if t.name == "claude" {
			if warn := claudeVersionWarning(string(out)); warn != "" {
				fmt.Fprintf(w, "! warn: %s\n", warn)
			}
			if help, err := exec.Command(path, "--help").CombinedOutput(); err == nil {
				if ch := permissionChoices(string(help)); ch == nil {
					fmt.Fprintln(w, "! warn: could not read --permission-mode choices from `claude --help`")
				} else if miss := missingModes(ch); len(miss) > 0 {
					fmt.Fprintf(w, "! warn: permission modes %v are not accepted by this claude (choices: %v); update internal/config PermissionModes\n", miss, ch)
				} else {
					fmt.Fprintln(w, "✓ permission modes match `claude --help`")
				}
			}
		}
	}
	if out, err := exec.Command("git", "-C", dir, "rev-parse", "--is-inside-work-tree").Output(); err != nil || !strings.Contains(string(out), "true") {
		fmt.Fprintln(w, "✗ not a git repository — run `git init` (or `slai init` to be offered it)")
		bad++
	} else {
		fmt.Fprintln(w, "✓ git repository")
	}
	bad += projectChecks(ctx, dir, fix, w)
	bad += doctorMemory(ctx, dir, w)
	bad += doctorHarnesses(ctx, dir, w)
	if f != nil {
		bad += doctorOtherTeams(ctx, *f, all, w)
	}
	return bad, nil
}

// doctorCheck is one line of the doctor report.
type doctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`     // false for a warning or a failure
	Detail string `json:"detail"` // what the check found
	Hint   string `json:"hint"`   // what to do about it, "" when the report gives none
	// Level is ok, warn, fail or info (a note that is neither a pass nor a problem, e.g. "no
	// state.db yet"); OK is true for ok and info.
	Level string `json:"level"`
}

// doctorTeam is one other slai team, as in the "Other slai teams" section.
type doctorTeam struct {
	Project      string `json:"project"`
	Prefix       string `json:"prefix"`
	Sessions     int    `json:"sessions"`
	Processes    int    `json:"processes"`
	LastActivity string `json:"last_activity,omitempty"` // RFC 3339; absent when unknown
	Orphan       bool   `json:"orphan"`
	Reason       string `json:"reason"`
}

// doctorJSON is `slai doctor --json`: the same checks as the text report (parsed from it, so the
// two can never disagree), the orphaned teams (always) and the other teams (with --all). It
// returns the same problem count the text mode does, so the exit code is unchanged.
func doctorJSON(ctx context.Context, dir string, fix, all bool, f *orphans.Finder, w io.Writer) (int, error) {
	var buf bytes.Buffer
	bad, err := runDoctorChecks(ctx, dir, fix, all, f, &buf)
	if err != nil {
		return 0, err
	}
	out := struct {
		Checks     []doctorCheck `json:"checks"`
		Problems   int           `json:"problems"`
		Orphans    []doctorTeam  `json:"orphans"`
		OtherTeams []doctorTeam  `json:"other_teams"` // healthy teams, listed only with --all
	}{Checks: parseDoctorChecks(buf.String()), Problems: bad, Orphans: []doctorTeam{}, OtherTeams: []doctorTeam{}}
	if f != nil {
		if teams, err := f.Teams(ctx); err == nil {
			for _, t := range teams {
				d := doctorTeam{Project: t.Project, Prefix: t.Prefix, Sessions: len(t.Sessions), Processes: len(t.Procs), Orphan: t.Orphan, Reason: t.Reason}
				if !t.LastActivity.IsZero() {
					d.LastActivity = t.LastActivity.UTC().Format(time.RFC3339)
				}
				switch {
				case t.Orphan:
					out.Orphans = append(out.Orphans, d)
				case all:
					out.OtherTeams = append(out.OtherTeams, d)
				}
			}
		}
	}
	return bad, writeJSON(w, out)
}

// parseDoctorChecks turns the text report into checks. A line starts with a marker: ✓ ok,
// ✗ fail, "! warn:" or ⚠ warn, "- " info; an unmarked indented line under "team health:" is an
// agent's health row (info). The "Other slai teams" list and its hint are not checks (doctorJSON
// reports those as data).
func parseDoctorChecks(text string) []doctorCheck {
	checks := []doctorCheck{}
	section := ""
	for _, raw := range strings.Split(text, "\n") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		if raw == "Other slai teams:" {
			section = "other teams"
			continue
		}
		indented := strings.HasPrefix(raw, " ")
		if !indented {
			if strings.HasSuffix(raw, ":") && !strings.HasPrefix(raw, "✓") && !strings.HasPrefix(raw, "✗") && !strings.HasPrefix(raw, "!") {
				section = strings.TrimSuffix(raw, ":")
				continue
			}
			section = ""
		}
		if section == "other teams" {
			continue
		}
		line := strings.TrimSpace(raw)
		status, msg := "info", line
		switch {
		case strings.HasPrefix(line, "✓ "):
			status, msg = "ok", strings.TrimPrefix(line, "✓ ")
		case strings.HasPrefix(line, "✗ "):
			status, msg = "fail", strings.TrimPrefix(line, "✗ ")
		case strings.HasPrefix(line, "! warn: "):
			status, msg = "warn", strings.TrimPrefix(line, "! warn: ")
		case strings.HasPrefix(line, "⚠ "):
			status, msg = "warn", strings.TrimPrefix(line, "⚠ ")
		case strings.HasPrefix(line, "- "):
			msg = strings.TrimPrefix(line, "- ")
		}
		msg = strings.Join(strings.Fields(msg), " ")
		name := section
		if name == "" {
			name = doctorCheckName(msg)
		}
		detail, hint := splitHint(msg)
		checks = append(checks, doctorCheck{Name: name, OK: status == "ok" || status == "info", Detail: detail, Hint: hint, Level: status})
	}
	return checks
}

var hintRe = regexp.MustCompile("(?:run|use|try) `[^`]+`(?: [^`;()]*)?")

// splitHint separates what a line found from what it says to do: the text after " — ", or a
// "run `...`" phrase inside it. Detail keeps the whole line when there is no hint.
func splitHint(msg string) (detail, hint string) {
	if i := strings.Index(msg, " — "); i >= 0 {
		return msg[:i], msg[i+len(" — "):]
	}
	if m := hintRe.FindString(msg); m != "" {
		return msg, strings.TrimSpace(m)
	}
	return msg, ""
}

// doctorCheckNames gives a stable name to the checks whose first word is not one: matched in
// order against the start of the message, else the first word names the check.
var doctorCheckNames = []struct{ prefix, name string }{
	{"permission modes", "permission-modes"},
	{"git repository", "git-repository"},
	{"not a git repository", "git-repository"},
	{"integration branch", "integration-branch"},
	{"worktree for", "worktree"},
	{"orphan tmux session", "orphan-session"},
	{"stale supervisor pidfile", "supervisor-pidfile"},
	{"no other slai teams", "other-teams"},
	{"no orphaned slai teams", "other-teams"},
	{"could not look for other", "other-teams"},
	{"no .slai/team.yaml", "team.yaml"},
	{"no state.db", "state.db"},
}

func doctorCheckName(msg string) string {
	for _, n := range doctorCheckNames {
		if strings.HasPrefix(msg, n.prefix) {
			return n.name
		}
	}
	return strings.Fields(msg + " ")[0]
}

// projectChecks inspects .slai state; returns the number of problems (warnings do not count).
func projectChecks(ctx context.Context, dir string, fix bool, w io.Writer) int {
	teamPath := filepath.Join(dir, ".slai", "team.yaml")
	if _, err := os.Stat(teamPath); err != nil {
		fmt.Fprintln(w, "- no .slai/team.yaml (run `slai init`, or `slai`, which runs it); skipping project checks")
		return 0
	}
	bad := 0
	team, err := config.Load(teamPath)
	if err != nil {
		fmt.Fprintf(w, "✗ team.yaml invalid: %v\n", err)
		return 1
	}
	fmt.Fprintf(w, "✓ team.yaml valid (%d agents)\n", len(team.Agents))
	printSameReviewerLine(w, team)
	repo := &git.Repo{Dir: dir}
	if repo.BranchExists(ctx, team.Project.IntegrationBranch) {
		fmt.Fprintf(w, "✓ integration branch %s\n", team.Project.IntegrationBranch)
	} else {
		fmt.Fprintf(w, "! warn: integration branch %s missing (created by `slai`)\n", team.Project.IntegrationBranch)
	}
	tm := tmux.New(socketName())
	inTeam := map[string]bool{}
	for _, a := range team.Agents {
		inTeam[tmux.SessionName(dir, a.ID)] = true
	}
	sessions, _ := tm.ListSessions(ctx, tmux.SessionName(dir, ""))
	liveSet := map[string]bool{}
	for _, s := range sessions {
		liveSet[s] = true
		if !inTeam[s] {
			fmt.Fprintf(w, "! warn: orphan tmux session %s (not in team.yaml)", s)
			if fix {
				if err := tm.KillSession(ctx, s); err == nil {
					fmt.Fprint(w, " — killed")
				}
			}
			fmt.Fprintln(w)
		}
	}
	// The running supervisor is reported by the team health section (pid,
	// heartbeat and build on one line); only a stale pidfile is flagged here.
	pidPath := supervisor.PidPath(dir)
	{
		if _, err := os.Stat(pidPath); err == nil {
			if _, running := supervisor.Running(dir); !running {
				fmt.Fprintf(w, "! warn: stale supervisor pidfile %s (no lock holder)", pidPath)
				if fix && os.Remove(pidPath) == nil {
					fmt.Fprint(w, " — removed")
				}
				fmt.Fprintln(w)
			}
		}
	}
	// worktrees
	for _, a := range team.Agents {
		if a.Role != config.RoleEngineer {
			continue
		}
		wt := filepath.Join(dir, ".slai", "worktrees", a.ID)
		if _, err := os.Stat(wt); err != nil {
			fmt.Fprintf(w, "! warn: worktree for %s missing (%s); `slai` recreates it\n", a.ID, wt)
			continue
		}
		worktreeIndexCheck(ctx, a.ID, wt, w)
	}
	// state.db
	dbPath := filepath.Join(dir, ".slai", "state.db")
	if _, err := os.Stat(dbPath); err != nil {
		fmt.Fprintln(w, "- no state.db yet")
		return bad
	}
	st, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintf(w, "✗ state.db: %v\n", err)
		return bad + 1
	}
	defer st.Close()
	if v, err := st.SchemaVersion(); err == nil {
		fmt.Fprintf(w, "✓ state.db migration version %d\n", v)
	}
	agents, _ := st.GetAgents()
	bad += teamHealth(w, st, dir, agents, liveSet, time.Now())
	bad += hookHealth(w, dir, st, agents, time.Now())
	ids := make([]string, 0, len(team.Agents))
	for _, a := range team.Agents {
		ids = append(ids, a.ID)
	}
	bad += identityCheck(w, st, dir, ids)
	bad += identityRunChecks(w, st, dir, ids, func(id string) bool {
		for _, ag := range agents {
			if ag.ID == id {
				return liveSet[ag.TmuxSession]
			}
		}
		return false
	})
	orchID := ""
	for _, ta := range team.Agents {
		if ta.Role == config.RoleOrchestrator && orchID == "" {
			orchID = ta.ID
		}
	}
	doctorWaiting(w, st, ids, orchID, func(id string) bool {
		for _, ag := range agents {
			if ag.ID == id {
				return liveSet[ag.TmuxSession]
			}
		}
		return false
	})
	for _, a := range agents {
		live := liveSet[a.TmuxSession]
		switch {
		case a.Status == "dead" && live:
			fmt.Fprintf(w, "! warn: %s is marked dead in state.db but its tmux session is live (supervisor will correct this)\n", a.ID)
		case a.Status != "dead" && !live && a.TmuxSession != "":
			fmt.Fprintf(w, "! warn: %s is %s in state.db but has no tmux session (run `slai`)\n", a.ID, a.Status)
		}
		if !inTeam[tmux.SessionName(dir, a.ID)] {
			fmt.Fprintf(w, "! warn: state.db has agent %s which is not in team.yaml (`slai init --edit` reconciles)\n", a.ID)
		}
	}
	return bad
}

// worktreeIndexCheck warns when an engineer worktree has changes staged in its
// index. A stale index (e.g. left over after a branch moved) turns the next
// `git commit -a` / `git add .` into a silent revert of merged work.
func worktreeIndexCheck(ctx context.Context, id, wt string, w io.Writer) {
	staged, err := (&git.Repo{Dir: wt}).StagedFiles(ctx)
	if err != nil || len(staged) == 0 {
		return
	}
	shown := staged
	if len(shown) > 5 {
		shown = shown[:5]
	}
	fmt.Fprintf(w, "! warn: worktree for %s has %d staged change(s) (%s); if you did not stage them, run `git -C %s reset --hard HEAD` before committing\n",
		id, len(staged), strings.Join(shown, "; "), wt)
}

// buildID is this binary's build identity (a var so tests can pin it).
var buildID = buildinfo.ID

// teamHealth prints the "team health" section and returns the number of
// failures: a supervisor that should be running but is not (pidfile present and
// lock free, or lock held with a heartbeat older than service.SupervisorDownAfter),
// and any agent with an open revive_gave_up / orch_down escalation. No pidfile
// means the project is stopped, which is information, not a failure.
func teamHealth(w io.Writer, st *store.Store, dir string, agents []store.Agent, liveSet map[string]bool, now time.Time) int {
	bad := 0
	fmt.Fprintln(w, "team health:")
	hb, hbOK, _ := st.SupervisorHeartbeat()
	hbAge := "no heartbeat"
	if hbOK {
		hbAge = fmt.Sprintf("heartbeat %ds old", int(now.Sub(time.Unix(hb, 0)).Seconds()))
	}
	pidb, pidErr := os.ReadFile(supervisor.PidPath(dir))
	pid := strings.TrimSpace(string(pidb))
	_, running := supervisor.Running(dir)
	_, stoppedErr := os.Stat(supervisor.StoppedPath(dir))
	builds := service.ReadBuildState(st, buildID(), agents)
	switch {
	case stoppedErr == nil && !running:
		fmt.Fprintln(w, "  - supervisor: stopped (slai down)") // on purpose: information, not a failure
	case pidErr != nil:
		fmt.Fprintln(w, "  - supervisor not running (stopped)")
	case !running:
		fmt.Fprintf(w, "  ✗ supervisor not running (%s)\n", hbAge)
		bad++
	case hbOK && now.Sub(time.Unix(hb, 0)) > service.SupervisorDownAfter:
		fmt.Fprintf(w, "  ✗ supervisor not running (%s)\n", hbAge)
		bad++
	case !hbOK || builds.SupervisorOld():
		// alive, but on a build that predates the heartbeat or differs from this binary
		was := builds.Supervisor
		if was == "" {
			was = "unknown"
		}
		fmt.Fprintf(w, "  ⚠ supervisor is running an older slai build (pid %s, build %s, installed %s); it re-execs itself on the next tick, or run `slai down && slai up`\n", pid, was, builds.Current)
	default:
		fmt.Fprintf(w, "  ✓ supervisor running (pid %s, build %s, %s)\n", pid, builds.Supervisor, hbAge)
	}
	esc, _ := st.OpenEscalations()
	byAgent := map[string][]store.Escalation{}
	for _, e := range esc {
		byAgent[e.Agent] = append(byAgent[e.Agent], e)
	}
	for _, a := range agents {
		h := service.AgentHealth(st, a.ID, liveSet[a.TmuxSession], now)
		reason := h.Reason
		if reason == "" {
			reason = "-"
		}
		last := "never"
		if h.LastActivity > 0 {
			last = now.Sub(time.Unix(h.LastActivity, 0)).Truncate(time.Second).String() + " ago"
		}
		var open []string
		for _, e := range byAgent[a.ID] {
			open = append(open, e.Kind)
			if e.Kind == "revive_gave_up" || e.Kind == "orch_down" {
				bad++
			}
		}
		escs := "none"
		if len(open) > 0 {
			escs = strings.Join(open, ",")
		}
		old := ""
		if builds.AgentOld(a.ID) {
			old = " old build"
		}
		fmt.Fprintf(w, "  %-12s %-12s reason=%s last-activity=%s escalation=%s%s\n", a.ID, h.Liveness, reason, last, escs, old)
	}
	for _, agent := range []string{"", "team"} {
		for _, e := range byAgent[agent] {
			fmt.Fprintf(w, "  team escalation: %s %s\n", e.Kind, e.Text)
		}
	}
	return bad
}
