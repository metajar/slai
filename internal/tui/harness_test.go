package tui

import (
	"github.com/charmbracelet/x/ansi"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/store"
)

// mixedModel is newModel with eng-1 on pi and eng-2 on opencode.
func mixedModel(t *testing.T) *Model {
	t.Helper()
	m, st := newModel(t)
	team := *m.opts.Team
	team.Agents = append([]config.Agent(nil), m.opts.Team.Agents...)
	for i := range team.Agents {
		switch team.Agents[i].ID {
		case "eng-1":
			team.Agents[i].Harness, team.Agents[i].Model = config.HarnessPi, "zai/glm-5.3"
		case "eng-2":
			team.Agents[i].Harness, team.Agents[i].Model = config.HarnessOpenCode, "zhipuai-coding-plan/glm-5.3"
		}
	}
	m.opts.Team = &team
	_ = st.UpsertAgent(store.Agent{ID: "eng-1", Role: "engineer", Model: "zai/glm-5.3", Status: "idle"})
	_ = st.UpsertAgent(store.Agent{ID: "eng-2", Role: "engineer", Model: "zhipuai-coding-plan/glm-5.3", Status: "idle"})
	m.Update(m.refreshCmd()())
	return m
}

// AC 19: rows of non-claude agents carry a short harness tag; claude rows do not.
func TestRowsShowHarnessTagForNonClaudeAgents(t *testing.T) {
	m := mixedModel(t)
	v := screen(m)
	var eng1, eng2, orch string
	for _, l := range strings.Split(v, "\n") {
		switch {
		case rowRe("eng-1").MatchString(l):
			eng1 = l
		case rowRe("eng-2").MatchString(l):
			eng2 = l
		case rowRe("orch").MatchString(l):
			orch = l
		}
	}
	if !strings.Contains(eng1, "eng-1 pi") {
		t.Errorf("pi row lacks its tag: %q", eng1)
	}
	if !strings.Contains(eng2, "eng-2 oc") {
		t.Errorf("opencode row lacks its tag: %q", eng2)
	}
	if strings.Contains(orch, " oc ") || strings.Contains(orch, " pi ") {
		t.Errorf("claude row must carry no tag: %q", orch)
	}
	for kind, want := range map[string]string{config.HarnessClaude: "", config.HarnessOpenCode: "oc", config.HarnessPi: "pi"} {
		if got := harnessTagFor(kind); got != want {
			t.Errorf("harnessTagFor(%s) = %q, want %q", kind, got, want)
		}
	}
}

// The agent info overlay (I) lists harness, model and degraded features.
func TestAgentInfoShowsHarnessModelAndDegraded(t *testing.T) {
	m := mixedModel(t)
	m.cursorTo("eng-1")
	m.Update(key("I"))
	if m.ov != ovDetail {
		t.Fatalf("I did not open the agent info (ov=%v)", m.ov)
	}
	v := screen(m)
	for _, want := range []string{"agent eng-1", "harness", "pi", "zai/glm-5.3", "degraded", "statusline", "not supported by pi", "5h/weekly", "n/a"} {
		if !strings.Contains(v, want) {
			t.Errorf("info lacks %q:\n%s", want, v)
		}
	}
	m.Update(key("esc"))
	m.cursorTo("orch")
	m.Update(key("I"))
	v = screen(m)
	if !strings.Contains(v, "harness") || !strings.Contains(v, "claude") || strings.Contains(v, "n/a") {
		t.Errorf("claude info:\n%s", v)
	}
}

// AC 15 (TUI half): the usage panel shows n/a for non-Claude agents, never 0%.
func TestUsagePanelShowsNAForNonClaudeAgents(t *testing.T) {
	m := mixedModel(t)
	m.agentSpend = map[string]float64{"eng-1": 0.42}
	body := usageDetails(m.usage, m.spend, m.month, m.opts.Now(), "~/.claude", m.harnessUsageLines())
	body = ansi.Strip(body)
	for _, want := range []string{"Other harnesses", "eng-1", "5h n/a · weekly n/a · ~$0.42 today (not supported by pi)", "eng-2", "spend n/a", "not supported by opencode"} {
		if !strings.Contains(body, want) {
			t.Errorf("usage panel lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "eng-1 ") && strings.Contains(body, "0%") {
		t.Errorf("missing data must not read as 0%%:\n%s", body)
	}
	// a team of only claude agents gets no extra section
	plain, _ := newModel(t)
	if got := plain.harnessUsageLines(); got != "" {
		t.Errorf("claude-only team: %q", got)
	}
}

func TestPreviewHeaderNamesTheHarness(t *testing.T) {
	m := mixedModel(t)
	m.setSel("eng-1")
	if v := screen(m); !strings.Contains(v, "eng-1 · zai/glm-5.3") || !strings.Contains(v, "· pi") {
		t.Errorf("preview header lacks the harness:\n%s", v)
	}
}
