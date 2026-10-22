package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/oorbt/slai/internal/ratelimit"
	"github.com/oorbt/slai/internal/store"
)

// statuslineCmd is the hidden `slai statusline` command Claude Code runs to
// render its status line. Same contract as `slai hook`: it always exits 0,
// is bounded by a timeout, and never fails loudly.
func statuslineCmd() *cobra.Command {
	var agent string
	c := &cobra.Command{
		Use: "statusline", Hidden: true, Args: cobra.ArbitraryArgs, SilenceUsage: true, SilenceErrors: true,
		RunE: func(c *cobra.Command, args []string) error {
			res := make(chan string, 1)
			go func() { res <- runStatusline(c.InOrStdin(), pinnedGetenv(agent, os.Getenv), time.Now) }()
			select {
			case out := <-res:
				fmt.Fprintln(c.OutOrStdout(), out)
			case <-time.After(hookTimeout):
			}
			return nil
		},
	}
	c.Flags().StringVar(&agent, "agent", "", "agent id (wins over $SLAI_AGENT_ID)")
	return c
}

// runStatusline saves the parsed snapshot (when slai env is present and any
// window is known) and returns the one-line status text.
func runStatusline(in io.Reader, getenv func(string) string, now func() time.Time) (line string) {
	defer func() {
		if r := recover(); r != nil {
			line = "slai"
		}
	}()
	b, _ := io.ReadAll(io.LimitReader(in, 4<<20))
	snap, err := ratelimit.Parse(b)
	if err != nil {
		return "slai"
	}
	snap.Agent, snap.At = getenv("SLAI_AGENT_ID"), now().Unix()
	home, _ := os.UserHomeDir()
	snap.Auth = ratelimit.DetectAuth(getenv, home)
	var today *float64
	if project := getenv("SLAI_PROJECT"); project != "" {
		if st, err := store.Open(filepath.Join(project, ".slai", "state.db")); err == nil {
			defer st.Close()
			// Claude Code runs this on nearly every state change, from every agent, so
			// keep it cheap: write when the limits changed, or at most once a minute to
			// keep At fresh (the TUI dims stale data). An empty
			// snapshot (rate_limits absent) is stored only to record Auth when
			// nothing exists yet, never over good windows.
			old, have, _ := ratelimit.Load(st)
			if (!snap.Empty() || !have) && (!have || !ratelimit.Same(old, snap) || snap.At-old.At >= 60) {
				_ = ratelimit.Save(st, snap)
			}
			if v, err := st.TodaySpend(); err == nil {
				today = &v
			}
		}
	}
	var parts []string
	if snap.FiveHour != nil {
		parts = append(parts, fmt.Sprintf("5h %.0f%%", snap.FiveHour.UsedPercent))
	}
	if snap.SevenDay != nil {
		parts = append(parts, fmt.Sprintf("wk %.0f%%", snap.SevenDay.UsedPercent))
	}
	if today != nil {
		parts = append(parts, fmt.Sprintf("$%.2f today", *today))
	}
	if len(parts) == 0 {
		return "slai"
	}
	return strings.Join(parts, " · ")
}
