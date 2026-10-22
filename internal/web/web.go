// Package web is `slai serve`: a browser UI over the same store, service and supervisor
// requests the CLI and TUI use. main/cmd call Serve; a later `slai up --serve` can host it
// the same way. The pieces live in sub-packages: server (shell, auth, route table),
// templates and assets (embedded), pages (the list of page modules), webtest (the shared
// httptest harness).
package web

import (
	"context"

	"github.com/oorbt/slai/internal/web/pages"
	"github.com/oorbt/slai/internal/web/server"
)

// Options configures Serve; see server.Options.
type Options = server.Options

// Serve runs the web UI with every page module until ctx is cancelled. It listens on
// opts.Listen (default 127.0.0.1:0, a random loopback port) and prints the URL to opts.Out.
func Serve(ctx context.Context, opts Options) error {
	opts.Modules = append(pages.All(), opts.Modules...)
	return server.Serve(ctx, opts)
}
