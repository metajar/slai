package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/demo"
	"github.com/oorbt/slai/internal/git"
	"github.com/oorbt/slai/internal/supervisor"
	"github.com/oorbt/slai/internal/tmux"
	"github.com/oorbt/slai/internal/web"
)

// demoActions is the real web actions (a real Supervisor over the demo project, so sends,
// priorities, holds and task writes behave exactly as they do for a real team) with the
// pieces that would look at the machine replaced by canned text: panes, previews, doctor.
type demoActions struct{ *webActions }

func (a demoActions) CapturePane(_ context.Context, agentID string, _ bool) (string, error) {
	return demo.Pane(agentID), nil
}
func (a demoActions) CommitPreview(context.Context) (string, error) { return demo.CommitPlan, nil }
func (a demoActions) RevertPreview(context.Context, string) (string, error) {
	return demo.RevertPlan, nil
}
func (a demoActions) DoctorReport(context.Context) (string, int, error) {
	return demo.DoctorText, 0, nil
}
func (a demoActions) DoctorProbe(_ context.Context, which string) (string, error) {
	return "slai doctor --" + which + "\n  ok    (demo: nothing was probed)\n", nil
}

// runDemo serves a seeded fake project (never the real one) for screenshots and reviews.
func runDemo(c *cobra.Command, listen string, readOnly, insecure bool, dir string, opts demo.Options) error {
	if dir == "" {
		d, err := os.MkdirTemp("", "slai-demo-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(d)
		dir = d
	}
	p, err := demo.Seed(c.Context(), dir, opts)
	if err != nil {
		return err
	}
	defer p.Store.Close()
	ctx, stop := signal.NotifyContext(c.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(c.ErrOrStderr(), "slai serve --demo: fake project in %s (not your project; nothing here is real)\n", dir)
	so := demoOptions(p)
	so.Listen, so.ReadOnly, so.InsecureHTTP, so.Out = listen, readOnly, insecure, c.OutOrStdout()
	return web.Serve(ctx, so)
}

// demoOptions are the server options over a seeded demo project: the real web actions over a
// real Supervisor value, with the machine-looking pieces canned (see demoActions).
func demoOptions(p *demo.Project) web.Options {
	repo := &git.Repo{Dir: p.Dir}
	sup := &supervisor.Supervisor{Dir: p.Dir, Team: p.Team, Store: p.Store, Git: repo, Tmux: tmux.New("slai-demo"), Now: func() time.Time { return demo.Now }}
	team := func() *config.Team { return p.Team }
	return web.Options{
		Dir: p.Dir, Store: p.Store, Git: repo, Team: team, Actions: demoActions{newWebActions(sup, team)},
		Now:               func() time.Time { return demo.Now },
		SupervisorRunning: func() bool { return true },
		Orphans:           func(context.Context) ([]string, error) { return []string{"slai_ab12_"}, nil },
	}
}
