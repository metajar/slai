package cmd

import (
	"fmt"
	"strings"

	"github.com/oorbt/slai/internal/config"
)

// costNotice is the one line `slai init` and `slai up` print so the number of
// paid sessions and the state of the spend caps are never a surprise.
func costNotice(t *config.Team) string { return costLine("Starting", "", t) }

// initCostNotice is the init variant: nothing has started yet, `slai` starts it.
func initCostNotice(t *config.Team) string {
	return costLine("Will start", " when you run `slai`", t)
}

func costLine(verb, when string, t *config.Team) string {
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
	var set []string
	for _, x := range []struct {
		name string
		usd  float64
	}{{"daily", c.DailyUSD}, {"per-agent", c.PerAgentUSD}, {"per-task", c.PerTaskUSD}, {"per-run", c.PerRunUSD}} {
		if x.usd > 0 {
			set = append(set, x.name+" "+capText(x.usd))
		}
	}
	if len(set) == 0 {
		return verb + " " + n + when + ". Spend caps: OFF (set defaults.spend_caps in .slai/team.yaml)"
	}
	return verb + " " + n + when + ". Spend caps: " + strings.Join(set, ", ")
}

func capText(usd float64) string {
	if usd <= 0 {
		return "off"
	}
	return "$" + strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", usd), "0"), ".")
}
