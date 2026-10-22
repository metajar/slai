package supervisor

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/oorbt/slai/internal/buildinfo"
	"github.com/oorbt/slai/internal/hooks"
	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
)

// Upgrade-aware self-heal: when the slai binary on disk changes, the supervisor
// re-execs onto it and the agents' MCP servers (which keep running the old code)
// are restarted one at a time, when idle.
var (
	ExeSettle     = 3 * time.Second // the binary must stay unchanged this long before it is trusted (an install may still be writing it)
	UpgradeComeUp = 3 * time.Minute // a restarted agent must report the new build within this long
)

const (
	EscUpgradeFailed      = "upgrade_failed"       // the supervisor could not exec the new binary
	EscAgentUpgradeFailed = "agent_upgrade_failed" // a restarted agent never came back on the new build

	kvUpgraded   = "upgraded."         // + agent: "1" until its next wake line says slai was upgraded
	kvUpgradeTry = "upgrade.tried."    // + agent: "<build>@<unix>" of its upgrade restart
	kvExecFailed = "upgrade.execfail." // + exe hash: "1" once exec of that binary was attempted
)

// exeWatch notices that the executable on disk is no longer the one that is
// running: it stats size+mtime each call and hashes only after they change.
type exeWatch struct {
	path    string
	size    int64
	mtime   time.Time
	base    string    // hash of the contents the process started with
	changed time.Time // when size/mtime last moved
	pending bool
	newHash string // hash of the replacement, once settled and different
}

func newExeWatch(path string) *exeWatch {
	w := &exeWatch{path: path}
	if path == buildinfo.ExePath() {
		w.base = buildinfo.ExeHash() // already computed for the build id
	} else {
		w.base = buildinfo.HashFile(path)
	}
	if fi, err := os.Stat(path); err == nil {
		w.size, w.mtime = fi.Size(), fi.ModTime()
	}
	return w
}

// Check reports the hash of a replacement binary once one has settled; "" means
// the running binary is still the one on disk.
func (w *exeWatch) Check(now time.Time) string {
	fi, err := os.Stat(w.path)
	if err != nil {
		return "" // mid-replace (renamed away) or removed: look again next tick
	}
	if fi.Size() != w.size || !fi.ModTime().Equal(w.mtime) {
		w.size, w.mtime, w.changed, w.pending = fi.Size(), fi.ModTime(), now, true
	}
	if w.pending && now.Sub(w.changed) >= ExeSettle {
		w.pending = false
		if h := buildinfo.HashFile(w.path); h != "unknown" && h != w.base {
			w.newHash = h
		} else {
			w.newHash = ""
		}
	}
	return w.newHash
}

func (s *Supervisor) build() string {
	if s.Build != "" {
		return s.Build
	}
	return buildinfo.ID()
}

// watchExe starts watching the executable (once), baselined on what is there now.
func (s *Supervisor) watchExe() {
	if s.exe != nil {
		return
	}
	path := s.ExePath
	if path == "" {
		path = buildinfo.ExePath()
	}
	if path != "" {
		s.exe = newExeWatch(path)
	}
}

// recordBuild stores this process's build identity and notes a change since the
// previous supervisor ran.
func (s *Supervisor) recordBuild() {
	prev := s.kvGet(service.KVSupervisorBuild)
	cur := s.build()
	s.watchExe()
	_ = s.Store.SetKV(service.KVSupervisorBuild, cur)
	_ = s.Store.ClearEscalation("team", EscUpgradeFailed)
	if prev == "" { // the first supervisor to track builds: MCP servers already serving are older code
		var ids []string
		for _, a := range s.Team.Agents {
			ids = append(ids, a.ID)
		}
		service.MarkLegacyMCP(s.Store, ids)
	}
	if prev != "" && prev != cur {
		s.event("", "supervisor_upgraded", map[string]any{"from": prev, "to": cur})
	}
}

// reexecIfUpgraded runs between ticks (nothing in flight): if a new binary has
// settled on disk it replaces this process with it. On success it does not
// return; on failure the supervisor keeps running and says so once.
func (s *Supervisor) reexecIfUpgraded() {
	s.watchExe()
	if s.exe == nil {
		return
	}
	h := s.exe.Check(s.now())
	if h == "" || s.kvGet(kvExecFailed+h) != "" {
		return
	}
	exec := s.Exec
	if exec == nil {
		exec = syscall.Exec
	}
	args := s.ExecArgs
	if args == nil {
		args = os.Args
	}
	s.logf("supervisor: new slai binary on disk (%s); re-executing", h)
	// The flock is not released first: its descriptor is close-on-exec, so it drops
	// exactly when the exec succeeds and a failed exec leaves us holding it.
	err := exec(s.exe.path, args, os.Environ())
	_ = s.Store.SetKV(kvExecFailed+h, "1") // never retry the same binary
	if err != nil {
		s.logf("supervisor: exec %s: %v", s.exe.path, err)
		s.escalate(escalation{agent: "team", kind: EscUpgradeFailed, user: true,
			text: "the supervisor could not switch to the newly installed slai binary (" + err.Error() + "); run `slai down && slai up`"})
	}
}

// upgradeTick restarts agents still on an older build onto the new one, one at a
// time and only when it is safe: idle, nothing unread, no tool in flight, not
// blocked, attached, focused, paused or stopped. Busy agents are caught when they
// next go idle; the orchestrator goes last.
func (s *Supervisor) upgradeTick(ctx context.Context) {
	if !s.Team.Defaults.UpgradeRestart() {
		return
	}
	if _, held := s.CurrentHold(); held {
		return // a team hold keeps every session as it is; the restart waits for `slai resume`
	}
	agents, err := s.Store.GetAgents()
	if err != nil {
		return
	}
	paused, _ := s.Store.PausedAgents()
	cur := s.build()
	now := s.now()
	orch := s.orchestrator()

	var pending []store.Agent // stale, with a live session, that slai may still restart
	inFlight := false
	for _, a := range agents {
		if a.Status == "dead" || s.kvGet(kvStopped+a.ID) != "" || paused[a.ID] {
			continue
		}
		if !s.sessionLive(ctx, a) {
			continue
		}
		stale := service.StaleBuild(s.Store, a.ID, cur)
		tried := strings.SplitN(s.kvGet(kvUpgradeTry+a.ID), "@", 2)
		if !stale {
			if len(tried) == 2 && tried[0] == cur {
				_ = s.Store.DeleteKV(kvUpgradeTry + a.ID)
				_ = s.Store.ClearEscalation(a.ID, EscAgentUpgradeFailed)
			}
			continue
		}
		if len(tried) == 2 && tried[0] == cur {
			// already restarted for this build and not back yet
			at, _ := strconv.ParseInt(tried[1], 10, 64)
			if now.Sub(time.Unix(at, 0)) < UpgradeComeUp {
				inFlight = true
			} else {
				s.escalate(escalation{agent: a.ID, kind: EscAgentUpgradeFailed,
					text:    "restarted onto the new slai build but its MCP server did not report it",
					orchMsg: fmt.Sprintf("%s was restarted after the slai upgrade but its slai MCP server still shows the old build. slai will not retry; check it (slai attach) or restart it.", a.ID)})
			}
			continue
		}
		pending = append(pending, a)
	}
	if inFlight {
		return // one at a time
	}
	for _, last := range []bool{false, true} {
		for _, a := range pending {
			if (a.ID == orch) != last {
				continue
			}
			if last && len(pendingOthers(pending, orch)) > 0 {
				return // the orchestrator waits for everyone else
			}
			if !s.upgradeSafe(ctx, a, paused) {
				continue
			}
			s.upgradeRestart(ctx, a, cur)
			return
		}
	}
}

func pendingOthers(pending []store.Agent, orch string) []store.Agent {
	var out []store.Agent
	for _, a := range pending {
		if a.ID != orch {
			out = append(out, a)
		}
	}
	return out
}

// upgradeSafe: nothing is happening in the agent that a restart would cut off.
func (s *Supervisor) upgradeSafe(ctx context.Context, a store.Agent, paused map[string]bool) bool {
	if a.Status != hooks.StatusIdle && a.Status != hooks.StatusFinished {
		return false // working, waiting (blocked), rate limited, ...
	}
	if s.holdOff(ctx, a, paused) {
		return false
	}
	now := s.now()
	if s.toolInFlight(a.ID, now) {
		return false
	}
	if msgs, _ := s.Store.UndeliveredFor(a.ID); len(msgs) > 0 {
		return false // let it read its mail first
	}
	if len(s.oldUnread(a.ID)) > 0 {
		return false
	}
	pane, err := s.Tmux.CapturePane(ctx, a.TmuxSession, false, 0)
	if err == nil && s.modalExcerpt(a, pane) != "" {
		return false
	}
	return true
}

// upgradeRestart replaces the agent's session with a fresh one that resumes its
// own conversation (StartAgent resumes when the agent has run before).
func (s *Supervisor) upgradeRestart(ctx context.Context, a store.Agent, cur string) {
	cfg, ok := s.agentCfg(a.ID)
	if !ok {
		return
	}
	tasks, _ := s.Store.ListTasks("")
	owed := len(owedBy(cfg, tasks))
	from := s.kvGet(service.KVMCPBuildPrefix + a.ID)
	cause := fmt.Sprintf("slai was upgraded: this agent's MCP server still runs build %s, the supervisor runs %s", orDash(from), cur)
	if err := s.killSession(ctx, a.ID, a.TmuxSession, cause); err != nil {
		s.logf("upgrade restart %s: kill: %v", a.ID, err)
		return
	}
	_ = s.Store.SetAgentStatus(a.ID, "dead")
	_ = s.Store.SetKV(kvUpgradeTry+a.ID, cur+"@"+strconv.FormatInt(s.now().Unix(), 10))
	if _, err := s.StartAgent(ctx, cfg); err != nil {
		s.logf("upgrade restart %s: start: %v", a.ID, err) // autoRevive takes over if it owes work
		return
	}
	// The restarted agent only needs telling if it has something to pick up; the
	// orchestrator always does, since it coordinates everyone else.
	if owed > 0 || a.ID == s.orchestrator() {
		_ = s.Store.SetKV(kvUpgraded+a.ID, "1")
		_, _ = s.Store.AddMessage(store.Message{From: "slai", To: a.ID, Kind: "note",
			Body: "slai was upgraded and your session was restarted (your conversation was resumed). Call get_task to see what you still owe and carry on."})
	}
	s.restartEvent(a.ID, "agent_upgraded", cause, map[string]any{"from": from, "to": cur, "owed": owed})
}

func orDash(s string) string {
	if s == "" {
		return "(unrecorded)"
	}
	return s
}

// upgradeWakeLine is the wake text for an agent restarted by an upgrade.
func upgradeWakeLine(n int) string {
	return fmt.Sprintf("[slai] slai was upgraded; you were restarted (your conversation was resumed). You have %d new items. Call get_inbox.", n)
}
