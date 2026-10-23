package cmd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/oorbt/slai/internal/config"
	"github.com/spf13/cobra"
)

// groupName is the address of a role group: the role with underscores as
// hyphens ("focus_group" -> "focus-group"). The plural ("engineers") is
// accepted too.
func groupName(role string) string { return strings.ReplaceAll(role, "_", "-") }

// sendTargets resolves a `slai send` address to agent ids: an exact agent id
// first (so an agent named like a group is still reachable as itself), then
// "all", then a role group.
func sendTargets(t *config.Team, name string) ([]string, error) {
	for _, a := range t.Agents {
		if a.ID == name {
			return []string{a.ID}, nil
		}
	}
	var ids []string
	if name == "all" {
		for _, a := range t.Agents {
			ids = append(ids, a.ID)
		}
		return ids, nil
	}
	for _, a := range t.Agents {
		if g := groupName(a.Role); name == g || name == g+"s" {
			ids = append(ids, a.ID)
		}
	}
	if len(ids) > 0 {
		return ids, nil
	}
	var agents []string
	groups := map[string]bool{"all": true}
	for _, a := range t.Agents {
		agents = append(agents, a.ID)
		groups[groupName(a.Role)] = true
	}
	var gl []string
	for g := range groups {
		gl = append(gl, g)
	}
	sort.Strings(gl)
	return nil, fmt.Errorf("unknown agent or group %q (agents: %s; groups: %s)", name, strings.Join(agents, ", "), strings.Join(gl, ", "))
}

func sendCmd() *cobra.Command {
	return &cobra.Command{
		Use:   `send <agent|group> "message"`,
		Short: "Send a message from you to an agent, a role group or everyone",
		Long: `Send a message from you to an agent.

The address is an agent id (see "slai status"), "all" for every agent, or a role
group: the role name with hyphens, singular or plural (orchestrator,
product-manager, principal-engineer, engineers, focus-group). A group queues one
message per member and prints who it was queued for. An agent id that matches a
group name is sent to that agent only.`,
		Example: `  slai send orch "build a CLI that ..."
  slai send engineers "rebase on slai/integration before your next review"
  slai send all "stop and commit what you have"`,
		Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			s, err := load(c.Context())
			if err != nil {
				return err
			}
			defer s.Store.Close()
			ids, err := sendTargets(s.Team, args[0])
			if err != nil {
				return err
			}
			var queued []string
			var msgs []int64
			for _, id := range ids {
				s.RecordManualNudge(id) // counted for `slai status --stats` when it moves a silent agent that owes work
				m, err := s.SendUser(id, args[1])
				if err != nil {
					return fmt.Errorf("%s: %w (already queued for: %s)", id, err, strings.Join(queued, ", "))
				}
				queued = append(queued, id)
				msgs = append(msgs, m)
			}
			if len(ids) == 1 && ids[0] == args[0] {
				fmt.Printf("queued message %d for %s\n", msgs[0], ids[0])
				return nil
			}
			var parts []string
			for i, id := range queued {
				parts = append(parts, fmt.Sprintf("%s (message %d)", id, msgs[i]))
			}
			fmt.Printf("queued for %s: %s\n", args[0], strings.Join(parts, ", "))
			return nil
		}}
}
