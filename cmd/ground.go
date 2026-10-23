package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/hooks"
	"github.com/oorbt/slai/internal/roles"
	"github.com/oorbt/slai/internal/store"
)

// EventIdentityGround is recorded when you ask for a re-ground with `slai ground`.
const EventIdentityGround = "identity_ground"

// groundAgent queues a re-ground for agent id: the identity line (who it is, what it is for)
// goes into its inbox and the next wake-up opens with it, so it is typed only when the agent
// is idle and the pane is safe, the same rules as any wake-up. It reports false, changing
// nothing, when one is already waiting to be typed (so repeating the command is harmless).
func groundAgent(st *store.Store, team *config.Team, id string) (bool, error) {
	line := roles.IdentityLineFor(team, id)
	if line == "" {
		return false, fmt.Errorf("unknown agent %q (see `slai status`)", id)
	}
	if _, pending, err := st.GetKV(hooks.KVIdentityPending + id); err != nil {
		return false, err
	} else if pending {
		return false, nil
	}
	if err := st.SetKV(hooks.KVIdentityPending+id, "user"); err != nil {
		return false, err
	}
	if _, err := st.AddMessage(store.Message{From: "slai", To: id, Kind: "note",
		Body: line + " You were asked to re-ground: call whoami, then get_inbox and get_task to pick up where you were."}); err != nil {
		return false, err
	}
	return true, st.AddEvent(id, EventIdentityGround, `{"source":"user"}`)
}

func groundCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ground <agent>",
		Short: "Remind an agent who it is (identity re-ground), when it is idle",
		Long: `Queue the identity line for an agent: who it is and what it is for. The next wake-up
opens with it, typed only when the agent is idle and its pane is safe to type into (the same
rules as any wake-up), so a busy agent gets it when its turn ends. Asking again while the
first is still waiting changes nothing. "slai status" shows when each agent was last
re-grounded and any open identity correction.`,
		Example: `  slai ground eng-1`,
		Args:    cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			s, err := load(c.Context())
			if err != nil {
				return err
			}
			defer s.Store.Close()
			queued, err := groundAgent(s.Store, s.Team, args[0])
			if err != nil {
				return err
			}
			if !queued {
				fmt.Fprintf(c.OutOrStdout(), "a re-ground is already waiting for %s; it goes in at its next wake-up\n", args[0])
				return nil
			}
			fmt.Fprintf(c.OutOrStdout(), "queued a re-ground for %s; it goes in at its next idle wake-up\n", args[0])
			return nil
		}}
}
