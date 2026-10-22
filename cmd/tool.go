package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/oorbt/slai/internal/service"
)

// toolCmd invokes a team tool as $SLAI_AGENT_ID (used by scripted fake agents and tests).
func toolCmd() *cobra.Command {
	return &cobra.Command{Use: "tool <name> [json-args]", Short: "Invoke a team tool as SLAI_AGENT_ID (internal)", Hidden: true,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(c *cobra.Command, args []string) error {
			caller := os.Getenv("SLAI_AGENT_ID")
			if caller == "" {
				return errors.New("SLAI_AGENT_ID is not set")
			}
			dir := os.Getenv("SLAI_PROJECT")
			if dir == "" {
				dir, _ = os.Getwd()
			}
			svc, err := service.Open(dir)
			if err != nil {
				return err
			}
			defer svc.Store.Close()
			raw := json.RawMessage("{}")
			if len(args) == 2 {
				raw = json.RawMessage(args[1])
			}
			out, err := svc.Call(c.Context(), caller, args[0], raw)
			if err != nil {
				return err
			}
			b, err := json.Marshal(out)
			if err != nil {
				return err
			}
			fmt.Println(string(b))
			return nil
		}}
}
