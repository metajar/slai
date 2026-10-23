package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/hooks"
	"github.com/oorbt/slai/internal/roles"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/supervisor"
)

// hookTimeout bounds a hook run; Claude must never be blocked by slai.
var hookTimeout = 8 * time.Second

// spawnSupervisor starts the detached supervisor (a var so tests can fake it).
var spawnSupervisor = startDaemon

// handleHook is hooks.Handle (a var so tests can make it fail).
var handleHook = hooks.Handle

// hookCmd is the hidden `slai hook <event>` command. It always exits 0. For
// Claude it prints to stdout only a PreToolUse deny decision (lane guard); internal
// errors never block Claude. A neutral `tool` event (the lane guard of a plugin or
// extension) is different: it is always answered explicitly, {"decision":"allow"} or
// {"decision":"deny",...}, and any internal error, panic or timeout is a deny, so a
// caller that reads silence as "allow" can never be let through by a failure here.
func hookCmd() *cobra.Command {
	var agent, project string
	c := &cobra.Command{
		Use: "hook <event>", Hidden: true, Args: cobra.ArbitraryArgs, SilenceUsage: true, SilenceErrors: true,
		RunE: func(c *cobra.Command, args []string) error {
			ev := ""
			if len(args) > 0 {
				ev = args[0]
			}
			res := make(chan []byte, 1)
			go func() { res <- runHook(ev, c.InOrStdin(), pinnedEnv(agent, project, os.Getenv)) }()
			select {
			case out := <-res:
				if len(out) > 0 {
					fmt.Fprintln(c.OutOrStdout(), string(out))
				}
			case <-time.After(hookTimeout):
				if isGuardEvent(ev) {
					fmt.Fprintln(c.OutOrStdout(), string(guardDeny("timed out")))
				}
			}
			return nil
		},
	}
	c.Flags().StringVar(&agent, "agent", "", "agent id (wins over $SLAI_AGENT_ID)")
	c.Flags().StringVar(&project, "project", "", "project dir (wins over $SLAI_PROJECT)")
	return c
}

// runHook does the work; errors are logged to .slai/run/<agent>/hook.log.
// It returns the hook stdout JSON (a deny decision) or nil.
func runHook(event string, in io.Reader, getenv func(string) string) (out []byte) {
	guard := isGuardEvent(event) // a neutral tool call: must be answered explicitly
	// fail answers a failure: a deny for the guard, silence (as ever) for everything else.
	fail := func(why string) []byte {
		if guard {
			return guardDeny(why)
		}
		return nil
	}
	agent, project := getenv("SLAI_AGENT_ID"), getenv("SLAI_PROJECT")
	if agent == "" || project == "" {
		return fail("no agent identity")
	}
	logf := func(format string, a ...any) {
		dir := filepath.Join(project, ".slai", "run", agent)
		if os.MkdirAll(dir, 0o755) != nil {
			return
		}
		f, err := os.OpenFile(filepath.Join(dir, "hook.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		defer f.Close()
		fmt.Fprintf(f, "%s %s %s\n", time.Now().Format(time.RFC3339), event, fmt.Sprintf(format, a...))
	}
	defer func() {
		if r := recover(); r != nil {
			logf("panic: %v", r)
			out = fail(fmt.Sprint("panic: ", r))
		}
	}()
	var p hooks.Payload
	if b, err := io.ReadAll(io.LimitReader(in, 16<<20)); err == nil && len(b) > 0 {
		if err := json.Unmarshal(b, &p); err != nil {
			logf("bad payload: %v", err)
			if guard {
				return fail("unreadable tool call")
			}
		}
	}
	event, p, neutral, err := hooks.Adapt(event, p)
	if err != nil {
		logf("%v", err)
		return fail(err.Error())
	}
	guard = neutral && event == "pretooluse"
	st, err := store.Open(filepath.Join(project, ".slai", "state.db"))
	if err != nil {
		logf("open store: %v", err)
		return fail("state unavailable: " + err.Error())
	}
	defer st.Close()
	opt := hooks.Options{ProjectDir: project}
	if team, err := config.Load(filepath.Join(project, ".slai", "team.yaml")); err == nil {
		opt.MaxTurnsPerTask = team.Defaults.MaxTurnsPerTask
		opt.IdentityLine = func(id string) string { return roles.IdentityLineFor(team, id) }
	}
	res, err := handleHook(st, agent, event, p, opt)
	if neutral && event == "sessionstart" {
		// a harness that assigns its own session ids reports it here; resume needs it
		if serr := hooks.SaveSessionID(project, agent, p.SessionID); serr != nil {
			logf("save session id: %v", serr)
		}
	}
	if err != nil {
		logf("handle: %v", err)
		// leave a trace the supervisor can raise: hooks_failing
		_ = hooks.RecordErr(st, hooks.KVHookErr+agent, err.Error(), time.Now())
	}
	if event == "stop" && supervisor.ShouldRespawn(project) {
		respawnSupervisor(project, agent, st, logf)
	}
	if res.Decision != nil {
		if neutral {
			return res.Decision.NeutralJSON()
		}
		return res.Decision.HookJSON()
	}
	if guard {
		if err != nil { // the lane check may not have run to completion
			return fail("handle: " + err.Error())
		}
		return []byte(`{"decision":"allow"}`)
	}
	return nil
}

// isGuardEvent reports whether the command-line event is a neutral tool call.
func isGuardEvent(event string) bool { return strings.EqualFold(event, "tool") }

// guardDeny is the deny answer for a guard that could not decide.
func guardDeny(why string) []byte {
	return hooks.Decision{Deny: true, Reason: "guard error: " + why}.NeutralJSON()
}

// respawnSupervisor brings back a supervisor that died (crash, OOM, killed
// terminal) without `slai down`: a Stop hook is the one place that still runs.
// Agents that end turns together race here; the spawn lock lets exactly one start
// the daemon and the rest return quietly. The daemon logs to supervisor.log; this
// process's stdout stays reserved for hook decisions.
func respawnSupervisor(project, agent string, st *store.Store, logf func(string, ...any)) {
	unlock, ok := supervisor.TryLockSpawn(project)
	if !ok {
		return
	}
	defer unlock()
	if !supervisor.ShouldRespawn(project) { // someone else got there first
		return
	}
	if err := spawnSupervisor(project); err != nil {
		logf("respawn supervisor: %v", err)
		return
	}
	_ = st.AddEvent(agent, "supervisor_respawn", "{}")
}
