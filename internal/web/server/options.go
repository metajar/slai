// Package server is the shell of `slai serve`: listener, authentication, request checks,
// security headers, the route table, rendering and the helpers page packages build on.
//
// The shell never has more power than the TUI: pages call the same service, supervisor and
// store methods the TUI and CLI call. A page package contributes a Module (a func that
// returns its Routes); it never touches the mux, the headers or the auth directly.
package server

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/git"
	"github.com/oorbt/slai/internal/store"
)

// Actions is what the web layer may ask the supervisor side to do. Each method is the
// function the CLI and TUI already call; the web layer adds none of its own.
type Actions interface {
	// SendUser queues a message from the user to an agent (`slai send`).
	SendUser(to, body string) (int64, error)
	// CommitPreview is the plan `slai commit --dry-run` prints for the current branch state.
	CommitPreview(ctx context.Context) (string, error)
	// RevertPreview is the plan `slai revert <task> --dry-run` prints.
	RevertPreview(ctx context.Context, taskID string) (string, error)
}

// Options configures a server. There is deliberately no field, flag or environment
// variable that turns authentication off.
type Options struct {
	Dir   string
	Store *store.Store
	// Git is the project repository (read-only use: diffs, plans). Nil in tests that need none.
	Git *git.Repo
	// Team returns the current team definition; it is called per request so a
	// hot-reloaded team.yaml shows up.
	Team    func() *config.Team
	Actions Actions
	// Listen is host:port (default 127.0.0.1:0). Anything other than a loopback address
	// is "non-loopback": the session cookie is Secure and needs the one-time token.
	Listen string
	// AllowHosts are extra Host header values (host:port) accepted besides the bound
	// address, for a deployment behind a name.
	AllowHosts []string
	ReadOnly   bool
	// InsecureHTTP acknowledges serving over plain HTTP on a non-loopback address. It is
	// required there and changes nothing else: sessions, the token and every request check
	// stay on.
	InsecureHTTP bool
	Modules      []Module
	// SupervisorRunning reports whether a supervisor holds the project lock; nil means
	// unknown and no banner is shown.
	SupervisorRunning func() bool
	// Orphans lists the prefixes of orphaned slai teams (the dashboard banner). It may be
	// slow; nil means "not looked for".
	Orphans func(ctx context.Context) ([]string, error)
	// Out receives the "open this URL" line. Nil discards it.
	Out io.Writer
	Now func() time.Time
	// Token is the one-time token for non-loopback binds. Empty generates 32 random
	// bytes. Tests set it; the CLI never exposes it.
	Token string
}

// Module is one page package's contribution: it receives the shared Deps and returns the
// routes it serves.
type Module func(d *Deps) []Route

// Route is one entry of the route table. The table is the single list of everything the
// server answers; tests walk it to check the write rules and the forbidden routes.
type Route struct {
	// Method is GET, POST or DELETE. Anything else is rejected at registration.
	Method string
	// Pattern is a net/http ServeMux path pattern, e.g. "/agents/{id}/send".
	Pattern string
	// Action names a write for the web_action event ("send", "pause", ...). Required for
	// every non-GET route.
	Action string
	// Target describes what the write was aimed at for the event; default is the {id}
	// path value.
	Target func(r *http.Request) string
	// Calls says what the route calls (the function the CLI/TUI also uses). It is shown on
	// /capabilities, so a write route must set it.
	Calls string
	// Fallback routes are skipped when a module has already registered the pattern, so a
	// later page can replace a shell placeholder without a conflict.
	Fallback bool
	Handler  http.HandlerFunc
}

// Write reports whether the route changes state.
func (r Route) Write() bool { return r.Method != http.MethodGet }
