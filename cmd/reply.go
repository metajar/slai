package cmd

import (
	"fmt"
	"strconv"
	"time"

	"github.com/oorbt/slai/internal/service"
	"github.com/spf13/cobra"
)

func replyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   `reply <ask-id> "text"`,
		Short: "Answer an agent's question (ask_user) by its ask id",
		Long: `Answer one specific question an agent asked you. The ask id is shown next to the
waiting agent in "slai status" and on the web inbox. The answer is sent to the asking agent as
a message from you, linked to the ask, and the question shows as answered in the web inbox too.

Unlike "slai send", reply refuses an unknown id or a question that was already answered or that
the agent has moved on from.`,
		Example: `  slai status                  # eng-2 is asking you (ask 17)
  slai reply 17 "use the second option"`,
		Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil || id <= 0 {
				return fmt.Errorf("ask id %q is not a number (see `slai status`)", args[0])
			}
			s, err := load(c.Context())
			if err != nil {
				return err
			}
			defer s.Store.Close()
			agent, err := service.AnswerAsk(s.Store, time.Now(), id, args[1], func(to, body string) (int64, error) {
				return s.SendUserReply(to, body, id)
			})
			if err != nil {
				return err
			}
			fmt.Printf("answered ask %d: sent to %s\n", id, agent)
			return nil
		}}
}
