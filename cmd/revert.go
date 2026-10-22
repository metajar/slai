package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/oorbt/slai/internal/supervisor"
)

// revertWait is how long `slai revert` waits for the supervisor to carry the revert out.
var revertWait = 30 * time.Second

func revertCmd() *cobra.Command {
	var dry, force bool
	c := &cobra.Command{
		Use:   "revert <task>",
		Short: "Take one merged task back out of the integration branch",
		Long: `revert undoes one task's merge on the integration branch (slai/integration) with a git
revert, marks the task reverted, and tells the orchestrator and the task's engineer. Your own
branches are never touched, and a later ` + "`slai commit`" + ` does not carry the reverted change.

It refuses if later merged tasks depend on this one (pass --force to revert anyway) or if the
integration checkout has uncommitted changes. --dry-run shows what it would do.
The revert runs in the supervisor, in the same queue as merges, so the supervisor must be running.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			s, err := load(c.Context())
			if err != nil {
				return err
			}
			defer s.Store.Close()
			plan, err := s.PlanRevert(c.Context(), args[0], force)
			if err != nil {
				return err
			}
			fmt.Print(revertPlanText(plan))
			if dry {
				fmt.Println("\n(dry run: nothing changed)")
				return nil
			}
			if _, running := supervisor.Running(s.Dir); !running {
				return errors.New("the supervisor is not running, and it carries out reverts (in the same queue as merges). Start it with `slai up`, then run this again")
			}
			data, _ := json.Marshal(supervisor.RevertRequest{TaskID: plan.Task.ID, Force: force, By: "user"})
			before := s.Store.LastEventID()
			if err := s.Store.AddEvent("user", supervisor.EvRevertRequested, string(data)); err != nil {
				return err
			}
			// our request is the first revert_requested for this task after `before`
			reqs, err := s.Store.EventsAfter(before, supervisor.EvRevertRequested, 50)
			if err != nil {
				return err
			}
			for _, e := range reqs {
				if strings.Contains(e.Data, `"`+plan.Task.ID+`"`) {
					return waitRevert(s, e.ID, plan.Task.ID)
				}
			}
			return errors.New("could not find the queued revert request")
		}}
	c.Flags().BoolVar(&dry, "dry-run", false, "show what would be reverted and change nothing")
	c.Flags().BoolVar(&force, "force", false, "revert even though later merged tasks depend on this one")
	return c
}

// revertPlanText is what `slai revert` prints before it acts (or stops, with --dry-run).
func revertPlanText(plan supervisor.RevertPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "will revert merge %s of %s %q on %s\n", plan.MergeSHA, plan.Task.ID, plan.Task.Title, plan.Integration)
	if len(plan.Later) > 0 {
		fmt.Fprintf(&b, "  forced: %s already merged and depend on it\n", strings.Join(plan.Later, ", "))
	}
	if len(plan.OpenAfter) > 0 {
		fmt.Fprintf(&b, "  note: %s (not merged yet) depend on it and will be missing its code\n", strings.Join(plan.OpenAfter, ", "))
	}
	return b.String()
}

func waitRevert(s *supervisor.Supervisor, reqID int64, task string) error {
	deadline := time.Now().Add(revertWait)
	for time.Now().Before(deadline) {
		evs, _ := s.Store.EventsAfter(reqID, "", 200)
		for _, e := range evs {
			var d struct {
				Request int64  `json:"request"`
				Text    string `json:"text"`
			}
			if json.Unmarshal([]byte(e.Data), &d) != nil || d.Request != reqID {
				continue
			}
			switch e.Kind {
			case supervisor.EvReverted:
				fmt.Println("done:", d.Text)
				return nil
			case supervisor.EvRevertRefused, supervisor.EvRevertFailed:
				return errors.New(d.Text)
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("the revert of %s is queued but the supervisor has not carried it out after %s; check `slai doctor`", task, revertWait)
}
