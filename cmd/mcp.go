package cmd

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/oorbt/slai/internal/mcpserver"
)

func mcpCmd() *cobra.Command {
	var agent, project string
	c := &cobra.Command{Use: "mcp", Short: "Run the slai MCP server over stdio (internal)", Hidden: true, Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if project == "" {
				project = os.Getenv("SLAI_PROJECT")
			}
			id, mismatch := resolveAgent(agent, os.Getenv("SLAI_AGENT_ID"))
			if mismatch != "" {
				noteMismatch(project, id, mismatch)
			}
			return mcpserver.RunStdio(c.Context(), project, id)
		}}
	c.Flags().StringVar(&agent, "agent", "", "agent id to serve (wins over $SLAI_AGENT_ID)")
	c.Flags().StringVar(&project, "project", "", "project dir to serve (wins over $SLAI_PROJECT)")
	return c
}
