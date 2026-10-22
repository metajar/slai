// Package pages lists the page modules `slai serve` runs. Adding a page means writing a
// server.Module in its own package and adding one line to All.
package pages

import (
	"github.com/oorbt/slai/internal/web/agents"
	"github.com/oorbt/slai/internal/web/board"
	"github.com/oorbt/slai/internal/web/dashboard"
	"github.com/oorbt/slai/internal/web/doctor"
	"github.com/oorbt/slai/internal/web/inbox"
	"github.com/oorbt/slai/internal/web/logpage"
	"github.com/oorbt/slai/internal/web/memory"
	"github.com/oorbt/slai/internal/web/review"
	"github.com/oorbt/slai/internal/web/server"
	"github.com/oorbt/slai/internal/web/sse"
	"github.com/oorbt/slai/internal/web/stats"
)

// All returns every page module, in registration order. Routes of the shell that a module
// also serves (the dashboard at "/", the send route) are replaced by the module's.
func All() []server.Module {
	push := &agents.Push{} // keeps the agent pages' terminal view live, while somebody watches
	return []server.Module{
		sse.Module(func(h *sse.Hub) { push.Attach(h) }), // GET /events, the live-update stream
		dashboard.Module(dashboard.Config{}),            // home page
		board.Module,                                    // board + task detail
		agents.ModuleWithPush(push),                     // agents list/detail, pane, actions, new agent
		inbox.Module,
		review.Module, // reviews and the commit/revert preview
		stats.Module,
		doctor.Module,
		logpage.Module,
		memory.Module, // team memory page
	}
}
