package tui

import (
	"fmt"

	"github.com/oorbt/slai/internal/buildinfo"
	"github.com/oorbt/slai/internal/service"
)

// buildID is this binary's build identity (a var so tests can pin it).
var buildID = buildinfo.ID

// shortBuild trims a build ID for the header.
func shortBuild(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// headerBuild is the header's build segment: the build of this dashboard, and
// while a rolling upgrade runs, how many agents have restarted onto it.
func headerBuild(b service.BuildState) string {
	s := styleDim.Render("build " + shortBuild(b.Current))
	if b.SupervisorOld() {
		s += styleDim.Render(" · ") + styleAmber.Render("supervisor on older build")
	}
	if b.Upgrading() {
		r, t := b.Upgrade()
		s += styleDim.Render(" · ") + styleAmber.Render(fmt.Sprintf("upgrading: %d of %d agents restarted", r, t))
	}
	return s
}

// oldBuildAgents is the set of agents whose MCP server runs an older build.
func oldBuildAgents(b service.BuildState) map[string]bool {
	out := map[string]bool{}
	for id := range b.Agents {
		if b.AgentOld(id) {
			out[id] = true
		}
	}
	for id := range b.Legacy {
		if b.AgentOld(id) {
			out[id] = true
		}
	}
	return out
}
