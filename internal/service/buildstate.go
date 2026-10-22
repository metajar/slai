package service

import "github.com/oorbt/slai/internal/store"

// BuildState is what the project's running processes report about their build,
// next to the build of the binary reading it.
type BuildState struct {
	Current    string            // build ID of this binary
	Supervisor string            // supervisor.build ("" = never reported)
	Agents     map[string]string // agent -> mcp.build.<agent> ("" = never reported)
	Legacy     map[string]bool   // agents flagged mcp.legacy: running code from before builds were tracked
}

// ReadBuildState reads the reported builds for the given agents.
func ReadBuildState(st *store.Store, current string, agents []store.Agent) BuildState {
	b := BuildState{Current: current, Agents: map[string]string{}, Legacy: map[string]bool{}}
	b.Supervisor, _, _ = st.GetKV(KVSupervisorBuild)
	for _, a := range agents {
		if v, ok, _ := st.GetKV(KVMCPBuildPrefix + a.ID); ok {
			b.Agents[a.ID] = v
		}
		if _, ok, _ := st.GetKV(KVLegacyPrefix + a.ID); ok {
			b.Legacy[a.ID] = true
		}
	}
	return b
}

// SupervisorOld reports a supervisor that reported a build other than Current.
func (b BuildState) SupervisorOld() bool { return b.Supervisor != "" && b.Supervisor != b.Current }

// AgentOld reports an agent whose MCP server reported a build other than Current.
func (b BuildState) AgentOld(id string) bool {
	if v := b.Agents[id]; v != "" {
		return v != b.Current
	}
	return b.Legacy[id]
}

// Upgrade counts agents already on Current against those that reported a build:
// restarted of total. Both are 0 until some agent has reported.
func (b BuildState) Upgrade() (restarted, total int) {
	for _, v := range b.Agents {
		if v == "" {
			continue
		}
		total++
		if v == b.Current {
			restarted++
		}
	}
	return
}

// Upgrading reports a rolling upgrade in progress: some agents still run an old
// build while the supervisor or other agents already run Current.
func (b BuildState) Upgrading() bool {
	r, t := b.Upgrade()
	return r < t && (r > 0 || b.Supervisor == b.Current)
}
