// Package orphans finds slai teams other than the current project's: tmux
// sessions named slai_<hash>_<agent> and SLAI_PROJECT-tagged processes, grouped by
// project, flagging the ones nobody is running any more.
package orphans

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/oorbt/slai/internal/reaper"
	"github.com/oorbt/slai/internal/supervisor"
	"github.com/oorbt/slai/internal/tmux"
)

// InactiveAfter is how long a team without a supervisor may sit untouched before it
// counts as abandoned.
const InactiveAfter = 12 * time.Hour

var sessionRe = regexp.MustCompile(`^(slai_[0-9a-f]{6}_)`)

// Session is one tmux session of a team and the server it lives on.
type Session struct {
	Name   string
	Client *tmux.Client
}

// Team is one foreign slai team.
type Team struct {
	Project      string // project directory; "" when it cannot be determined
	Prefix       string // slai_<hash>_
	Sessions     []Session
	Procs        []reaper.Proc
	LastActivity time.Time // zero when unknown
	Orphan       bool
	Reason       string // why it is (or is not) an orphan
}

// Finder discovers teams. The zero value is not usable: set Clients and Current.
type Finder struct {
	Clients []*tmux.Client // every tmux server to look on (private socket, default server)
	Current []string       // the current project (several spellings): never listed, never killed
	Now     func() time.Time

	// Test seams; nil picks the real thing.
	Running   func(dir string) bool
	ListProcs func() (map[string][]reaper.Proc, error)
}

// ProjectRoots names the current project for a command run in dir: dir mapped out
// of an agent's worktree, and the SLAI_PROJECT an agent's shell carries. Both are
// excluded so a doctor run from inside an agent never mistakes its own team for a
// foreign one.
func ProjectRoots(dir string) []string {
	roots := []string{dir}
	if i := strings.Index(dir, "/.slai/"); i >= 0 {
		roots = []string{dir[:i]}
	}
	if p := os.Getenv("SLAI_PROJECT"); p != "" {
		roots = append(roots, p)
	}
	return roots
}

func variants(dir string) []string {
	out := []string{dir}
	if r, err := filepath.EvalSymlinks(dir); err == nil && r != dir {
		out = append(out, r)
	}
	return out
}

func prefixOf(project string) string { return tmux.SessionName(project, "") }

// Teams lists the other teams, orphans flagged, most recently active last.
func (f Finder) Teams(ctx context.Context) ([]Team, error) {
	now := time.Now
	if f.Now != nil {
		now = f.Now
	}
	running := f.Running
	if running == nil {
		running = func(dir string) bool { _, ok := supervisor.Running(dir); return ok }
	}
	listProcs := f.ListProcs
	if listProcs == nil {
		listProcs = reaper.ListProjects
	}
	cur := map[string]bool{} // prefixes and project paths that are ours
	for _, c := range f.Current {
		for _, v := range variants(c) {
			cur[prefixOf(v)] = true
			cur[v] = true
		}
	}

	teams := map[string]*Team{}
	get := func(prefix string) *Team {
		if t, ok := teams[prefix]; ok {
			return t
		}
		t := &Team{Prefix: prefix}
		teams[prefix] = t
		return t
	}
	for _, c := range f.Clients {
		infos, err := c.ListSessionInfo(ctx, "slai_")
		if err != nil {
			continue
		}
		for _, si := range infos {
			m := sessionRe.FindStringSubmatch(si.Name)
			if m == nil || cur[m[1]] {
				continue
			}
			t := get(m[1])
			t.Sessions = append(t.Sessions, Session{Name: si.Name, Client: c})
			if si.Activity.After(t.LastActivity) {
				t.LastActivity = si.Activity
			}
			if t.Project == "" {
				for _, cand := range []string{c.SessionEnv(ctx, si.Name, "SLAI_PROJECT"), projectOfPath(si.Path)} {
					if cand != "" && matches(cand, m[1]) {
						t.Project = cand
						break
					}
				}
			}
		}
	}
	if procs, err := listProcs(); err == nil {
		for project, ps := range procs {
			if cur[project] || cur[prefixOf(project)] {
				continue
			}
			t := get(prefixOf(project))
			if t.Project == "" {
				t.Project = project
			}
			t.Procs = append(t.Procs, ps...)
		}
	}

	var out []Team
	for _, t := range teams {
		f.judge(t, now(), running)
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastActivity.Equal(out[j].LastActivity) {
			return out[i].LastActivity.Before(out[j].LastActivity)
		}
		return out[i].Prefix < out[j].Prefix
	})
	return out, nil
}

// projectOfPath maps a session's working directory to its project root.
func projectOfPath(p string) string {
	if i := strings.Index(p, "/.slai/"); i >= 0 {
		return p[:i]
	}
	return p
}

func matches(project, prefix string) bool {
	for _, v := range variants(project) {
		if prefixOf(v) == prefix {
			return true
		}
	}
	return false
}

// judge decides whether the team is abandoned: no supervisor holds its lock, and
// either its project directory is gone or nothing has touched it for InactiveAfter.
func (f Finder) judge(t *Team, now time.Time, running func(string) bool) {
	if t.Project != "" {
		if _, err := os.Stat(t.Project); err != nil {
			t.Orphan, t.Reason = true, "project directory is gone"
			return
		}
		if running(t.Project) {
			t.Reason = "supervisor running"
			return
		}
		for _, name := range []string{"state.db", "state.db-wal"} {
			if fi, err := os.Stat(filepath.Join(t.Project, ".slai", name)); err == nil && fi.ModTime().After(t.LastActivity) {
				t.LastActivity = fi.ModTime()
			}
		}
	}
	idle := now.Sub(t.LastActivity)
	switch {
	case t.LastActivity.IsZero():
		t.Orphan, t.Reason = true, "supervisor not running, no recorded activity"
	case idle > InactiveAfter:
		t.Orphan, t.Reason = true, fmt.Sprintf("supervisor not running, inactive %s", idle.Round(time.Hour))
	default:
		t.Reason = "supervisor not running, active recently"
	}
}

// Kill stops an orphan team: its tmux sessions, then every process tagged with its
// project. It refuses a team that is not flagged orphan or whose supervisor has
// since come back. It returns what it stopped.
func (f Finder) Kill(ctx context.Context, t Team) (sessions, procs int, err error) {
	if !t.Orphan {
		return 0, 0, fmt.Errorf("%s is not an orphan", t.Prefix)
	}
	for _, c := range f.Current {
		for _, v := range variants(c) {
			if prefixOf(v) == t.Prefix || v == t.Project {
				return 0, 0, fmt.Errorf("%s is the current project", t.Prefix)
			}
		}
	}
	running := f.Running
	if running == nil {
		running = func(dir string) bool { _, ok := supervisor.Running(dir); return ok }
	}
	if t.Project != "" && running(t.Project) {
		return 0, 0, fmt.Errorf("%s: a supervisor is running now", t.Project)
	}
	for _, s := range t.Sessions {
		if err := s.Client.KillSession(ctx, s.Name); err != nil {
			return sessions, procs, err
		}
		sessions++
	}
	if t.Project != "" {
		n, survivors, err := reaper.KillAll(t.Project, 3*time.Second)
		procs = n
		if err != nil {
			return sessions, procs, err
		}
		if len(survivors) > 0 {
			return sessions, procs, fmt.Errorf("%d process(es) of %s would not stop", len(survivors), t.Project)
		}
	}
	return sessions, procs, nil
}
