package cmd

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/demo"
	"github.com/oorbt/slai/internal/supervisor"
	"github.com/oorbt/slai/internal/web"
)

func serveCmd() *cobra.Command {
	var listen string
	var readOnly, insecure bool
	var allowHosts []string
	var demoMode, demoFirstRun bool
	var demoDir string
	c := &cobra.Command{Use: "serve", Short: "Open the team in a browser (local web UI)", Args: cobra.NoArgs,
		Long: `serve runs a web UI for the team: the same data and the same writes as the TUI, in a browser.

It is a separate process from the supervisor. It opens .slai/state.db like ` + "`slai status`" + ` does and never
starts a supervisor: without ` + "`slai up`" + ` it shows the team and takes messages, and says the supervisor is
not running. It can send messages and set priorities, hold and cancel; it cannot approve a review, merge,
commit or type into a terminal.

By default it listens on a random port of 127.0.0.1 and prints the URL. Every visitor gets a session cookie;
nothing turns that off. A non-loopback --listen prints a URL with a one-time token: the token is a password,
and whoever holds it can read every agent's terminal output. It travels over plain HTTP, so use it only on a
network you trust. --read-only makes the server refuse every write.`,
		RunE: func(c *cobra.Command, _ []string) error {
			if demoMode {
				return runDemo(c, listen, readOnly, insecure, demoDir, demo.Options{FirstRun: demoFirstRun})
			}
			s, err := load(c.Context())
			if err != nil {
				return err
			}
			defer s.Store.Close()
			ctx, stop := signal.NotifyContext(c.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			team := reloadingTeam(s.Dir, s.Team)
			return web.Serve(ctx, web.Options{
				Dir: s.Dir, Store: s.Store, Git: s.Git, Team: team, Actions: newWebActions(s, team),
				Listen: listen, ReadOnly: readOnly, InsecureHTTP: insecure, AllowHosts: allowHosts, Out: c.OutOrStdout(),
				SupervisorRunning: func() bool { _, running := supervisor.Running(s.Dir); return running },
				Orphans: func(ctx context.Context) ([]string, error) {
					teams, err := OtherTeams(ctx, s.Dir)
					var out []string
					for _, t := range teams {
						if t.Orphan {
							out = append(out, t.Prefix)
						}
					}
					return out, err
				},
			})
		}}
	c.Flags().StringVar(&listen, "listen", "127.0.0.1:0", "host:port to listen on (port 0 picks a free one)")
	c.Flags().StringVar(&listen, "addr", "127.0.0.1:0", "alias of --listen")
	_ = c.Flags().MarkHidden("addr")
	c.Flags().BoolVar(&insecure, "insecure-http", false, "allow plain HTTP on a non-loopback --listen (a trusted LAN); sessions and the token stay on")
	c.Flags().StringSliceVar(&allowHosts, "allow-host", nil, "also accept this Host header (host:port) on the bound address, e.g. a name behind a reverse proxy; repeatable. The Origin and session checks stay on")
	c.Flags().BoolVar(&demoMode, "demo", false, "serve a seeded fake project (for screenshots and reviews); never your project")
	c.Flags().StringVar(&demoDir, "demo-dir", "", "directory for --demo (must be new/empty or a previous demo; default a temp dir)")
	c.Flags().BoolVar(&demoFirstRun, "demo-first-run", false, "with --demo: a team with no tasks yet")
	for _, f := range []string{"demo", "demo-dir", "demo-first-run"} {
		_ = c.Flags().MarkHidden(f)
	}
	c.Flags().BoolVar(&readOnly, "read-only", false, "refuse every write (the page is for looking only)")
	return c
}

// reloadingTeam returns a func that yields the team, re-reading team.yaml when its mtime
// changes (the supervisor hot-reloads the same file), and keeping the last good team when
// the new file does not load.
func reloadingTeam(dir string, first *config.Team) func() *config.Team {
	path := filepath.Join(dir, ".slai", "team.yaml")
	var mu sync.Mutex
	cur := first
	var seen time.Time
	if fi, err := os.Stat(path); err == nil {
		seen = fi.ModTime()
	}
	return func() *config.Team {
		mu.Lock()
		defer mu.Unlock()
		if fi, err := os.Stat(path); err == nil && !fi.ModTime().Equal(seen) {
			seen = fi.ModTime()
			if t, err := config.Load(path); err == nil {
				cur = t
			}
		}
		return cur
	}
}
