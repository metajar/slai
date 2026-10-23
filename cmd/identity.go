package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/roles"
	"github.com/oorbt/slai/internal/store"
)

// An agent's identity comes from its run files (the --agent flag and the env
// block in mcp.json / settings.json), not from the process env it inherited:
// Claude Code can re-host a session in a background process whose env belongs to
// another agent.

// kvIdentityMismatch + agent: the inherited SLAI_AGENT_ID that disagreed with the pinned one.
const kvIdentityMismatch = "identity.mismatch."

// pinnedGetenv is getenv with SLAI_AGENT_ID forced to agent when one was given.
func pinnedGetenv(agent string, getenv func(string) string) func(string) string {
	return pinnedEnv(agent, "", getenv)
}

// pinnedEnv is getenv with SLAI_AGENT_ID and SLAI_PROJECT forced to the --agent
// and --project flags when given: a plugin or extension bakes both into the
// command, so neither the shell's env nor the tmux session's can change them.
func pinnedEnv(agent, project string, getenv func(string) string) func(string) string {
	if agent == "" && project == "" {
		return getenv
	}
	return func(k string) string {
		switch {
		case k == "SLAI_AGENT_ID" && agent != "":
			return agent
		case k == "SLAI_PROJECT" && project != "":
			return project
		}
		return getenv(k)
	}
}

// resolveAgent returns the identity to use: the flag when set, else env. mismatch
// is the differing env value when both are set and disagree.
func resolveAgent(flag, env string) (id, mismatch string) {
	if flag == "" {
		return env, ""
	}
	if env != "" && env != flag {
		return flag, env
	}
	return flag, ""
}

// noteMismatch records that the inherited env named another agent: a line in
// .slai/run/<agent>/mcp.log and KV identity.mismatch.<agent>. Best effort.
func noteMismatch(project, agent, inherited string) {
	if project == "" {
		return
	}
	dir := filepath.Join(project, ".slai", "run", agent)
	if os.MkdirAll(dir, 0o755) != nil {
		return
	}
	msg := fmt.Sprintf("inherited SLAI_AGENT_ID=%s but this server is pinned to %s; using %s", inherited, agent, agent)
	if f, err := os.OpenFile(filepath.Join(dir, "mcp.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		fmt.Fprintf(f, "%s warning: %s\n", time.Now().Format(time.RFC3339), msg)
		f.Close()
	}
	if st, err := store.Open(filepath.Join(project, ".slai", "state.db")); err == nil {
		_ = st.SetKV(kvIdentityMismatch+agent, fmt.Sprintf("%s|%d", inherited, time.Now().Unix()))
		// C6 signal (iv): an agent whose environment names another agent may have taken that
		// agent's identity; tell it who it is.
		line := ""
		if team, err := config.Load(filepath.Join(project, ".slai", "team.yaml")); err == nil {
			line = roles.IdentityLineFor(team, agent)
		}
		_, _ = st.RecordIdentityCorrection(agent, "env_mismatch", "its environment named "+inherited, line, time.Now())
		st.Close()
	}
}

// identityProblems inspects an agent's run files. old lists files in the
// pre-pinning format (rewritten on the next agent start); wrong lists files
// pinned to a different agent than their directory.
func identityProblems(dir, agent string) (old, wrong []string) {
	rd := filepath.Join(dir, ".slai", "run", agent)
	check := func(file string, ids func(map[string]any) (args []string, env map[string]any)) {
		b, err := os.ReadFile(filepath.Join(rd, file))
		if err != nil {
			return // not generated yet
		}
		var doc map[string]any
		if json.Unmarshal(b, &doc) != nil {
			wrong = append(wrong, file+" is not valid JSON")
			return
		}
		args, env := ids(doc)
		pinned := false
		for i, a := range args {
			if a == "--agent" && i+1 < len(args) {
				pinned = true
				if args[i+1] != agent {
					wrong = append(wrong, fmt.Sprintf("%s: --agent %s", file, args[i+1]))
				}
			}
		}
		if file == "mcp.json" && !pinned {
			old = append(old, file+": no --agent")
		}
		if v, ok := env["SLAI_AGENT_ID"].(string); !ok {
			old = append(old, file+": no env SLAI_AGENT_ID")
		} else if v != agent {
			wrong = append(wrong, fmt.Sprintf("%s: env SLAI_AGENT_ID=%s", file, v))
		}
	}
	check("mcp.json", func(d map[string]any) ([]string, map[string]any) {
		srv, _ := d["mcpServers"].(map[string]any)
		slai, _ := srv["slai"].(map[string]any)
		var args []string
		if l, ok := slai["args"].([]any); ok {
			for _, a := range l {
				args = append(args, fmt.Sprint(a))
			}
		}
		env, _ := slai["env"].(map[string]any)
		return args, env
	})
	check("settings.json", func(d map[string]any) ([]string, map[string]any) {
		env, _ := d["env"].(map[string]any)
		return nil, env
	})
	return old, wrong
}

// identityCheck prints the per-agent run-file identity check and returns the number of failures.
func identityCheck(w interface{ Write([]byte) (int, error) }, st *store.Store, dir string, agents []string) int {
	bad := 0
	for _, id := range agents {
		old, wrong := identityProblems(dir, id)
		switch {
		case len(wrong) > 0:
			fmt.Fprintf(w, "✗ %s run files are pinned to another agent (%s); restart it to rewrite them\n", id, strings.Join(wrong, "; "))
			bad++
		case len(old) > 0:
			fmt.Fprintf(w, "! warn: %s run files predate identity pinning (%s); rewritten on its next start\n", id, strings.Join(old, "; "))
		}
		if st != nil {
			if v, ok, _ := st.GetKV(kvIdentityMismatch + id); ok {
				who, at, _ := strings.Cut(v, "|")
				fmt.Fprintf(w, "! warn: %s's MCP server inherited SLAI_AGENT_ID=%s and ignored it (at %s)\n", id, who, at)
			}
		}
	}
	return bad
}
