package tui

import (
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/service"
)

func TestWakeGateLine(t *testing.T) {
	if got := wakeGateLine(false, 1, 10, 0, 0); got != "" {
		t.Fatalf("no gate: %q", got)
	}
	want := "⏸ wake gate: wake-ups paused (p resumes) · daily spend cap reached ($10.00/$10.00) · 2 agent(s) held by slai (u resumes)"
	if got := wakeGateLine(true, 10, 10, 2, 0); got != want {
		t.Fatalf("got %q", got)
	}
	if got := wakeGateLine(false, 20, 0, 0, 0); got != "" {
		t.Fatalf("cap 0 means uncapped: %q", got)
	}
	if !agentSpendCapped(5, 5) || agentSpendCapped(4, 5) || agentSpendCapped(9, 0) {
		t.Fatal("agentSpendCapped")
	}
}

func TestWakeGateShownInBannerAndRow(t *testing.T) {
	m, s := newModel(t)
	m.opts.Team.Defaults.Caps.PerAgentUSD = 1
	_ = s.AddUsage("eng-1", 0, 0, 2)
	m.Update(m.refreshCmd()())
	m.paused = true
	v := screen(m)
	for _, want := range []string{"wake gate: wake-ups paused", "capped", "per-agent spend ca"} {
		if !strings.Contains(v, want) {
			t.Fatalf("missing %q:\n%s", want, v)
		}
	}
}

func TestGatedChipAndQueuedHeader(t *testing.T) {
	m, _ := newModel(t)
	m.health = healthState{byAgent: map[string]service.Health{
		"eng-1": {Liveness: "gated", Reason: "queued for slot"},
		"pm":    {Liveness: "gated", Reason: "held until 14:05"},
	}}
	m.rebuild()
	v := screen(m)
	for _, want := range []string{"gated", "⚠ gated: queued", "1 agent(s) queued for a slot"} {
		if !strings.Contains(v, want) {
			t.Fatalf("missing %q:\n%s", want, v)
		}
	}
}
