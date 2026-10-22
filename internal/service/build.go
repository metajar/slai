package service

import (
	"strconv"

	"github.com/oorbt/slai/internal/buildinfo"
	"github.com/oorbt/slai/internal/store"
)

// KV keys that record which slai build each process runs.
const (
	KVSupervisorBuild = "supervisor.build"
	KVMCPBuildPrefix  = "mcp.build." // + agent
	// KVLegacyPrefix marks an agent whose MCP server was already running when the
	// first supervisor that tracks builds started: it predates mcp.build.
	KVLegacyPrefix = "mcp.legacy." // + agent
)

// CurrentBuild is the build the team should be running: the supervisor's (it
// re-execs onto a new binary), else this process's own.
func CurrentBuild(st *store.Store) string {
	if v, ok, _ := st.GetKV(KVSupervisorBuild); ok && v != "" {
		return v
	}
	return buildinfo.ID()
}

// RecordMCPBuild notes the build an agent's MCP server started on.
func RecordMCPBuild(st *store.Store, agent string) {
	_ = st.SetKV(KVMCPBuildPrefix+agent, buildinfo.ID())
	_ = st.DeleteKV(KVLegacyPrefix + agent)
}

// MarkLegacyMCP flags every agent that has served calls without recording a
// build: their MCP servers run code from before builds were tracked.
func MarkLegacyMCP(st *store.Store, agents []string) {
	for _, a := range agents {
		if _, ok, _ := st.GetKV(KVMCPBuildPrefix + a); ok {
			continue
		}
		v, _, _ := st.GetKV(lastSeenKey(a))
		if n, _ := strconv.ParseInt(v, 10, 64); n > 0 {
			_ = st.SetKV(KVLegacyPrefix+a, "1")
		}
	}
}

// StaleBuild reports whether the agent's slai MCP server runs older code than
// current: it recorded a different build, or it is a flagged legacy server that
// never recorded one.
func StaleBuild(st *store.Store, agent, current string) bool {
	if v, ok, _ := st.GetKV(KVMCPBuildPrefix + agent); ok && v != "" {
		return v != current
	}
	_, legacy, _ := st.GetKV(KVLegacyPrefix + agent)
	return legacy
}
