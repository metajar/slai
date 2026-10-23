package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/oorbt/slai/internal/claude"
	"github.com/oorbt/slai/internal/store"
)

// identityRunChecks is the doctor's second look at what keeps an agent knowing who it is
// (identityCheck in identity.go reads the run files; this adds what only matters while an
// agent is running, and the two ways an agent loses its identity silently). It returns the
// number of failures:
//   - a running agent whose mcp.json lacks --agent or whose settings.json lacks the env pin: it
//     cannot be repaired without a restart, and a re-hosted Claude session can speak as another agent;
//   - a system.md that is missing (for an agent with run files), empty, or does not name the agent;
//   - two agents recorded under one session id, in .slai/run/<agent>/session-id or in recent
//     hook:sessionstart events: the second resumes the first's conversation, which is how the
//     /tmp/minion orchestrator lost its identity.
func identityRunChecks(w interface{ Write([]byte) (int, error) }, st *store.Store, dir string, agents []string, live func(id string) bool) int {
	bad := 0
	for _, id := range agents {
		rd := claude.RunDir(dir, id)
		if _, err := os.Stat(rd); err != nil {
			continue // never started: nothing generated yet
		}
		if live(id) {
			if old, _ := identityProblems(dir, id); len(old) > 0 {
				fmt.Fprintf(w, "✗ %s is running with run files that do not pin its identity (%s); restart it (`slai down`, then `slai`)\n", id, strings.Join(old, "; "))
				bad++
			}
		}
		b, err := os.ReadFile(filepath.Join(rd, "system.md"))
		switch {
		case err != nil:
			fmt.Fprintf(w, "✗ %s has no system.md (%s): its session would not know who it is; restart it to rewrite it\n", id, filepath.Join(".slai", "run", id, "system.md"))
			bad++
		case strings.TrimSpace(string(b)) == "":
			fmt.Fprintf(w, "✗ %s's system.md is empty: its session would not know who it is; check .slai/prompts/ and restart it\n", id)
			bad++
		case !strings.Contains(string(b), id):
			fmt.Fprintf(w, "✗ %s's system.md does not name %s; restart it to rewrite it\n", id, id)
			bad++
		}
	}
	bad += sharedSessionChecks(w, st, dir, agents)
	return bad
}

// sharedSessionChecks flags a session id that more than one agent uses.
func sharedSessionChecks(w interface{ Write([]byte) (int, error) }, st *store.Store, dir string, agents []string) int {
	users := map[string]map[string]bool{} // session id -> agents
	use := func(session, agent string) {
		if session == "" {
			return
		}
		if users[session] == nil {
			users[session] = map[string]bool{}
		}
		users[session][agent] = true
	}
	for _, id := range agents {
		if b, err := os.ReadFile(filepath.Join(claude.RunDir(dir, id), "session-id")); err == nil {
			use(strings.TrimSpace(string(b)), id)
		}
	}
	if st != nil {
		known := map[string]bool{}
		for _, id := range agents {
			known[id] = true
		}
		if evs, err := st.RecentEvents(1000); err == nil {
			for _, e := range evs {
				if e.Kind != "hook:sessionstart" || !known[e.Agent] {
					continue
				}
				var d struct{ Session string }
				_ = json.Unmarshal([]byte(e.Data), &d)
				use(d.Session, e.Agent)
			}
		}
	}
	var ids []string
	for session, who := range users {
		if len(who) > 1 {
			ids = append(ids, session)
		}
	}
	sort.Strings(ids)
	for _, session := range ids {
		var names []string
		for a := range users[session] {
			names = append(names, a)
		}
		sort.Strings(names)
		short := session
		if len(short) > 8 {
			short = short[:8]
		}
		fmt.Fprintf(w, "✗ %s share session id %s…: the later one resumes the earlier one's conversation and takes its identity; restart them (`slai down`, then `slai`) and check nothing else starts claude in this folder\n", strings.Join(names, " and "), short)
	}
	return len(ids)
}
