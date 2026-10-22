package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/hooks"
)

// laneJudge is the guard the probes drive: the same decision the hook makes for a
// PreToolUse / neutral tool call. A var so a test can swap in a fail-open guard.
var laneJudge = hooks.JudgeTool

// laneProbeOutside is a path no role's lane (nor any scratch root) contains.
const laneProbeOutside = "/etc/slai-lane-probe"

type laneCase struct {
	name string
	deny bool
	in   string // hook stdin: Claude PreToolUse JSON, or the neutral payload
}

// laneCases builds the probe payloads for one agent. Nothing is executed or
// written: the payloads go to the guard function only, so no event is recorded.
func laneCases(a config.Agent, projectDir string) []laneCase {
	lane := hooks.LaneFor(a.Role, a.ID, "", projectDir, projectDir)
	inside := ""
	switch a.Role {
	case config.RoleEngineer:
		inside = filepath.Join(lane.Worktree, "slai-lane-probe.txt")
	case config.RolePM:
		inside = filepath.Join(lane.ProjectDir, ".slai", "specs", "slai-lane-probe.md")
	case config.RolePrincipal:
		inside = filepath.Join(lane.ProjectDir, ".slai", "decisions.md")
	}
	write := func(tool, key, path string, neutral bool) string {
		in, _ := json.Marshal(map[string]string{key: path})
		if neutral {
			return fmt.Sprintf(`{"event":"tool","tool":%q,"tool_input":%s,"cwd":%q}`, tool, in, lane.ProjectDir)
		}
		return fmt.Sprintf(`{"hook_event_name":"PreToolUse","tool_name":%q,"tool_input":%s,"cwd":%q}`, tool, in, lane.ProjectDir)
	}
	key := map[string]string{config.HarnessClaude: "file_path", config.HarnessOpenCode: "filePath", config.HarnessPi: "path"}[a.HarnessKind()]
	neutral := a.HarnessKind() != config.HarnessClaude
	tool := "Write"
	if neutral {
		tool = "write"
	}
	bashTool := "Bash"
	if neutral {
		bashTool = "bash"
	}
	cmd := func(c string) string { return write(bashTool, "command", c, neutral) }
	readTool := "Read"
	if neutral {
		readTool = "read"
	}
	read := func(p string) string { return write(readTool, key, p, neutral) }
	pick := func(p string) string { return write(tool, key, p, neutral) }

	cs := []laneCase{{name: "write outside lane", deny: true}, {name: "bash redirect outside lane", deny: true},
		{name: "read project file", deny: false}}
	if inside != "" {
		cs = append(cs, laneCase{name: "write inside lane", deny: false})
	} else {
		inside = filepath.Join(lane.ProjectDir, ".slai", "specs", "slai-lane-probe.md")
		cs = append(cs, laneCase{name: "write (read-only role)", deny: true})
	}
	for i := range cs {
		var in string
		switch cs[i].name {
		case "write outside lane":
			in = pick(laneProbeOutside)
		case "bash redirect outside lane":
			in = cmd("echo x > " + laneProbeOutside)
		case "read project file":
			in = read(filepath.Join(lane.ProjectDir, "docs", "slai-lane-probe.png"))
		default:
			in = pick(inside)
		}
		cs[i].in = in
	}
	if neutral {
		cs = append(cs, laneCase{name: "unknown tool", deny: true,
			in: write("probe_unknown_tool", "path", laneProbeOutside, true)})
	}
	return cs
}

// laneVerdict runs one probe through hook payload decoding and the guard.
func laneVerdict(a config.Agent, projectDir, stdin string) (deny bool, reason string, err error) {
	var p hooks.Payload
	if err := json.Unmarshal([]byte(stdin), &p); err != nil {
		return false, "", err
	}
	ev := "pretooluse"
	if a.HarnessKind() != config.HarnessClaude {
		ev = "tool"
	}
	_, p, _, err = hooks.Adapt(ev, p)
	if err != nil {
		return false, "", err
	}
	l := hooks.LaneFor(a.Role, a.ID, "", projectDir, p.Cwd)
	d := laneJudge(l, p)
	if d == nil {
		return false, "", nil
	}
	return d.Deny, d.Reason, nil
}

// doctorLane self-tests the lane guard of each configured agent (or just `only`),
// printing a pass/fail line per case. It returns the number of failures.
func doctorLane(team *config.Team, projectDir, only string, w io.Writer) (int, error) {
	bad, n := 0, 0
	for _, a := range team.Agents {
		if only != "" && a.ID != only {
			continue
		}
		n++
		for _, c := range laneCases(a, projectDir) {
			want := "allow"
			if c.deny {
				want = "deny"
			}
			deny, _, err := laneVerdict(a, projectDir, c.in)
			got := "allow"
			if deny {
				got = "deny"
			}
			label := fmt.Sprintf("%-10s %-8s %-18s %s", a.ID, a.HarnessKind(), a.Role, c.name)
			switch {
			case err != nil:
				bad++
				fmt.Fprintf(w, "✗ %s: probe error: %v\n", label, err)
			case got != want:
				bad++
				fmt.Fprintf(w, "✗ %s: expected %s, guard said %s\n", label, want, got)
			default:
				fmt.Fprintf(w, "✓ %s: %s\n", label, got)
			}
		}
	}
	if only != "" && n == 0 {
		return 0, fmt.Errorf("no agent %q in team.yaml", only)
	}
	return bad, nil
}

// laneFlagRun is `slai doctor --lane [agent]`.
func laneFlagRun(w io.Writer, only string) error {
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	team, err := config.Load(filepath.Join(dir, ".slai", "team.yaml"))
	if err != nil {
		return err
	}
	bad, err := doctorLane(team, dir, only, w)
	if err != nil {
		return err
	}
	if bad > 0 {
		return fmt.Errorf("%d lane guard probe(s) failed", bad)
	}
	return nil
}
