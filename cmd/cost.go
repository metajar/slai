package cmd

import (
	"fmt"
	"strings"

	"github.com/oorbt/slai/internal/config"
)

// costNotice is the one line `slai init` and `slai up` print so the number of
// paid sessions and the state of the spend caps are never a surprise.
func costNotice(t *config.Team) string {
	claude, other := 0, 0
	for _, a := range t.Agents {
		if a.Harness == "" || a.Harness == config.HarnessClaude {
			claude++
		} else {
			other++
		}
	}
	n := fmt.Sprintf("%d Claude sessions", claude)
	if claude == 1 {
		n = "1 Claude session"
	}
	if other > 0 {
		n += fmt.Sprintf(" + %d on other harnesses", other)
	}
	c := t.Defaults.Caps
	if c.PerAgentUSD <= 0 && c.DailyUSD <= 0 {
		return "Starting " + n + ". Spend caps: OFF (set defaults.spend_caps in .slai/team.yaml)"
	}
	return "Starting " + n + ". Spend caps: per-agent " + capText(c.PerAgentUSD) + " / daily " + capText(c.DailyUSD)
}

func capText(usd float64) string {
	if usd <= 0 {
		return "off"
	}
	return "$" + strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", usd), "0"), ".")
}
