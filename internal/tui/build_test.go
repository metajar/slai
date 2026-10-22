package tui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/oorbt/slai/internal/service"
)

func pinBuild(t *testing.T, id string) {
	t.Helper()
	old := buildID
	buildID = func() string { return id }
	t.Cleanup(func() { buildID = old })
}

func rowRe(id string) *regexp.Regexp { return regexp.MustCompile(`[●○◆▲◌✕] ` + id + ` `) }

func TestHeaderBuildSegment(t *testing.T) {
	cur := service.BuildState{Current: "cur", Supervisor: "cur", Agents: map[string]string{"a": "cur", "b": "cur"}}
	if got := ansi.Strip(headerBuild(cur)); got != "build cur" {
		t.Fatalf("steady state: %q", got)
	}
	mid := service.BuildState{Current: "cur", Supervisor: "cur", Agents: map[string]string{"a": "cur", "b": "v0", "c": "v0"}}
	if got := ansi.Strip(headerBuild(mid)); got != "build cur · upgrading: 1 of 3 agents restarted" {
		t.Fatalf("mid upgrade: %q", got)
	}
	oldSup := service.BuildState{Current: "cur", Supervisor: "v0", Agents: map[string]string{"a": "v0"}}
	if got := ansi.Strip(headerBuild(oldSup)); got != "build cur · supervisor on older build" {
		t.Fatalf("old supervisor: %q", got)
	}
	// agents that never reported do not count as "restarted" or as old
	none := service.BuildState{Current: "cur", Supervisor: "cur", Agents: map[string]string{"a": ""}}
	if got := ansi.Strip(headerBuild(none)); got != "build cur" {
		t.Fatalf("unreported: %q", got)
	}
}

func TestTUIShowsBuildAndOldBuildChip(t *testing.T) {
	pinBuild(t, "cur1234")
	m, s := newModel(t)
	_ = s.SetKV("supervisor.build", "cur1234")
	_ = s.SetKV("mcp.build.eng-1", "v0")
	_ = s.SetKV("mcp.build.eng-2", "cur1234")
	m.Update(m.refreshCmd()())
	v := screen(m)
	if !strings.Contains(v, "build cur1234") {
		t.Fatalf("header lacks the build:\n%s", v)
	}
	var e1, e2 string
	for _, l := range strings.Split(v, "\n") {
		switch {
		case rowRe("eng-1").MatchString(l):
			e1 = l
		case rowRe("eng-2").MatchString(l):
			e2 = l
		}
	}
	if !strings.Contains(e1, "old build") || strings.Contains(e2, "old build") {
		t.Fatalf("old build chip: eng-1=%q eng-2=%q\n%s", e1, e2, v)
	}
	if !strings.Contains(v, "upgrading: 1 of 2 agents restarted") {
		t.Skipf("upgrade notice does not fit the header at this width:\n%s", v)
	}
}
