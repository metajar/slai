package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/hooks"
	"github.com/oorbt/slai/internal/store"
)

func laneTeam(harness string) *config.Team {
	h := func(id, role string) config.Agent { return config.Agent{ID: id, Role: role, Harness: harness} }
	return &config.Team{Agents: []config.Agent{
		h("orch", config.RoleOrchestrator), h("pm", config.RolePM), h("principal", config.RolePrincipal),
		h("eng-1", config.RoleEngineer), h("fg", config.RoleFocusGroup),
	}}
}

// AC 1: deny / allow / unknown-tool cases give the expected verdicts on every harness.
func TestDoctorLaneVerdictsPerHarness(t *testing.T) {
	proj := t.TempDir()
	for _, harness := range []string{config.HarnessClaude, config.HarnessOpenCode, config.HarnessPi} {
		t.Run(harness, func(t *testing.T) {
			var out bytes.Buffer
			bad, err := doctorLane(laneTeam(harness), proj, "", &out)
			if err != nil || bad != 0 {
				t.Fatalf("bad=%d err=%v\n%s", bad, err, out.String())
			}
			s := out.String()
			for _, want := range []string{"write outside lane: deny", "bash redirect outside lane: deny", "write inside lane: allow", "write (read-only role): deny", "read project file: allow"} {
				if !strings.Contains(s, want) {
					t.Errorf("missing %q in\n%s", want, s)
				}
			}
			if unk := strings.Contains(s, "unknown tool: deny"); unk != (harness != config.HarnessClaude) {
				t.Errorf("unknown-tool case present = %v on %s", unk, harness)
			}
		})
	}
}

// AC 2: a guard that fails open is reported, naming agent and case, and --lane exits non-zero.
func TestDoctorLaneFailOpenGuardFails(t *testing.T) {
	old := laneJudge
	laneJudge = func(hooks.Lane, hooks.Payload) *hooks.Decision { return nil }
	t.Cleanup(func() { laneJudge = old })
	var out bytes.Buffer
	bad, err := doctorLane(laneTeam(config.HarnessPi), t.TempDir(), "eng-1", &out)
	if err != nil || bad == 0 {
		t.Fatalf("bad=%d err=%v", bad, err)
	}
	s := out.String()
	if !strings.Contains(s, "✗ eng-1") || !strings.Contains(s, "write outside lane: expected deny, guard said allow") ||
		!strings.Contains(s, "unknown tool: expected deny") {
		t.Fatalf("output does not name agent and case:\n%s", s)
	}
	if strings.Contains(s, "pm ") {
		t.Errorf("--lane eng-1 probed other agents:\n%s", s)
	}
}

func TestDoctorLaneUnknownAgent(t *testing.T) {
	if _, err := doctorLane(laneTeam(""), t.TempDir(), "nobody", &bytes.Buffer{}); err == nil {
		t.Fatal("want error for an agent not in team.yaml")
	}
}

func TestDoctorLaneFlagRegistered(t *testing.T) {
	if doctorCmd().Flags().Lookup("lane") == nil {
		t.Fatal("no --lane flag")
	}
}

// AC 3: probes record no events, so lane_denied counts (denied_count) are unchanged.
func TestDoctorLaneRecordsNoEvents(t *testing.T) {
	proj := t.TempDir()
	_ = os.MkdirAll(filepath.Join(proj, ".slai"), 0o755)
	st, err := store.Open(filepath.Join(proj, ".slai", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, a := range laneTeam("pi").Agents {
		_ = st.UpsertAgent(store.Agent{ID: a.ID, Role: a.Role})
	}
	if bad, err := doctorLane(laneTeam(config.HarnessPi), proj, "", &bytes.Buffer{}); bad != 0 || err != nil {
		t.Fatalf("bad=%d err=%v", bad, err)
	}
	counts, err := st.LaneDenialCounts(0)
	if err != nil || len(counts) != 0 {
		t.Fatalf("lane_denied counts after probes = %v (err %v)", counts, err)
	}
}
