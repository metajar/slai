// Package cmd holds the cobra commands.
package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/git"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/supervisor"
	"github.com/oorbt/slai/internal/tmux"
)

// rootHelpFooter ends `slai --help` (root only) with a pointer to the command table.
const rootHelpFooter = "Which command do I use? See docs/commands.md (CLI commands are for you; MCP tools are for agents)."

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "slai",
		Short:         "Turn a repo into a managed AI software team",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(c *cobra.Command, _ []string) error {
			dir, _ := os.Getwd()
			if _, err := os.Stat(filepath.Join(dir, ".slai", "team.yaml")); err != nil {
				return runInit(c.Context(), false)
			}
			return runTUI(c.Context())
		},
	}
	root.AddCommand(initCmd(), upCmd(), downCmd(), attachCmd(), statusCmd(), doctorCmd(), memoryCmd(), superviseCmd(), sendCmd(), holdCmd(), resumeCmd(), priorityCmd(), revertCmd(), cleanCmd(), commitCmd(), serveCmd())
	root.AddCommand(mcpCmd(), toolCmd(), hookCmd(), statuslineCmd(), versionCmd())
	root.SetHelpTemplate(root.HelpTemplate() + "{{if not .HasParent}}\n" + rootHelpFooter + "\n{{slaiNextStep}}\n{{end}}")
	return root
}

func Execute() {
	root := newRootCmd()
	if err := root.ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "slai:", err)
		os.Exit(1)
	}
}

// load opens the project in the current directory.
func load(ctx context.Context) (*supervisor.Supervisor, error) {
	dir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	team, err := config.Load(filepath.Join(dir, ".slai", "team.yaml"))
	if err != nil {
		return nil, fmt.Errorf("%w (run `slai init`)", err)
	}
	st, err := store.Open(filepath.Join(dir, ".slai", "state.db"))
	if err != nil {
		return nil, err
	}
	bin, err := os.Executable()
	if err != nil {
		bin = "slai"
	}
	return &supervisor.Supervisor{Dir: dir, Team: team, Tmux: tmux.New(socketName()),
		Git: &git.Repo{Dir: dir}, Store: st, SlaiBin: bin,
		GateOnTrust: os.Getenv("SLAI_NO_TRUST_GATE") == "",
		Driver:      os.Getenv("SLAI_NO_DRIVER") == ""}, nil
}

func attachCmd() *cobra.Command {
	return &cobra.Command{Use: "attach <agent>", Short: "Attach to an agent's tmux pane", Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			s, err := load(c.Context())
			if err != nil {
				return err
			}
			defer s.Store.Close()
			for _, a := range s.Team.Agents {
				if a.ID == args[0] {
					return s.Tmux.Attach(c.Context(), s.Session(a.ID))
				}
			}
			return fmt.Errorf("unknown agent %q", args[0])
		}}
}

// socketName is the tmux server slai's agents live on. A private server keeps
// them out of the user's own tmux (like `tmux -L slai attach -t <session>`).
func socketName() string {
	if s := os.Getenv("SLAI_TMUX_SOCKET"); s != "" {
		return s
	}
	return "slai"
}
