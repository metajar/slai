package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/memory"
)

// runCmd runs a cobra command with args and returns stdout alone.
func runCmd(t *testing.T, c *cobra.Command, args ...string) (string, error) {
	t.Helper()
	var out, errb bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&errb)
	c.SetArgs(args)
	err := c.Execute()
	return out.String(), err
}

func TestVersionJSON(t *testing.T) {
	out, err := runCmd(t, versionCmd(), "--json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Version, Build, Revision, Built, OS, Arch string
		Dirty                                     bool
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("not one JSON document: %v\n%s", err, out)
	}
	if v.Version == "" || v.Build == "" || v.Revision == "" || v.Built == "" || v.OS == "" || v.Arch == "" {
		t.Fatalf("version json = %+v", v)
	}
	text, _ := runCmd(t, versionCmd())
	if !strings.Contains(text, "slai "+v.Version) || !strings.Contains(text, "build "+v.Build) {
		t.Errorf("json and text disagree:\n%s\n%+v", text, v)
	}
}

func TestMemoryLsAndShowJSON(t *testing.T) {
	e, out := memFixture(t)
	ctx := context.Background()
	fresh := e.put(t, memory.Fact{Kind: memory.KindGotcha, Text: "Lane guard resolves variables first.", Status: memory.StatusVerified, VerifiedBy: "principal", Paths: lanePath()})
	old := e.put(t, memory.Fact{Kind: memory.KindArchitecture, Text: "App entry is cmd/app.", Status: memory.StatusVerified, VerifiedBy: "principal", Paths: []memory.Path{{Path: "cmd/app/main.go"}}})
	writeFile(t, e.mem.Dir, "f-badbad00.md", "not a fact\n")
	changeOnIntegration(t, e, "cmd/app/main.go", "package main\n\nfunc main() { println() }\n")

	if err := e.lsJSON(ctx, false); err != nil {
		t.Fatal(err)
	}
	var ls struct {
		Facts []struct {
			ID, Kind, Status, Path, Age, Reason string
			Paths                               []memory.Path
		}
	}
	if err := json.Unmarshal(out.Bytes(), &ls); err != nil {
		t.Fatalf("ls --json is not one JSON document: %v\n%s", err, out.String())
	}
	got := map[string]string{}
	for _, f := range ls.Facts {
		got[f.ID] = f.Status
	}
	want := map[string]string{fresh.ID: "fresh", old.ID: "stale", "f-badbad00": "invalid"}
	for id, st := range want {
		if got[id] != st {
			t.Errorf("%s: status %q, want %q (%v)", id, got[id], st, got)
		}
	}
	for _, f := range ls.Facts {
		if f.ID == fresh.ID && (f.Path != "internal/lane/lane.go" || len(f.Paths) != 1 || f.Kind != "gotcha" || f.Age != "0d") {
			t.Errorf("fresh row = %+v", f)
		}
		if f.ID == old.ID && f.Reason == "" {
			t.Errorf("a stale fact should say why: %+v", f)
		}
	}
	out.Reset()
	if err := e.lsJSON(ctx, true); err != nil {
		t.Fatal(err)
	}
	ls.Facts = nil
	if err := json.Unmarshal(out.Bytes(), &ls); err != nil || len(ls.Facts) != 1 || ls.Facts[0].ID != old.ID {
		t.Fatalf("--stale --json: %v %+v", err, ls.Facts)
	}

	out.Reset()
	if err := e.showJSON(ctx, old.ID); err != nil {
		t.Fatal(err)
	}
	var sh struct {
		ID, File, Status, Kind, Against string
		Grounding                       []struct{ Path, Recorded, Current, State string }
	}
	if err := json.Unmarshal(out.Bytes(), &sh); err != nil {
		t.Fatalf("show --json is not one JSON document: %v\n%s", err, out.String())
	}
	cur := gitIn(t, e.dir, "rev-parse", "slai/integration:cmd/app/main.go")
	if sh.ID != old.ID || sh.Status != "stale" || sh.Kind != "architecture" || sh.Against != "slai/integration" || !strings.Contains(sh.File, "App entry is cmd/app.") ||
		len(sh.Grounding) != 1 || sh.Grounding[0].State != "CHANGED" || sh.Grounding[0].Recorded != old.Paths[0].Blob || sh.Grounding[0].Current != cur {
		t.Fatalf("show json = %+v (current blob %s)", sh, cur)
	}
	out.Reset()
	if err := e.showJSON(ctx, "f-badbad00"); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out.Bytes(), &sh); err != nil || sh.Status != "invalid" {
		t.Fatalf("invalid fact: %v %+v", err, sh)
	}
	if err := e.showJSON(ctx, "f-nope"); err == nil {
		t.Error("unknown id should fail")
	}
}

func TestDoctorJSON(t *testing.T) {
	t.Setenv("SLAI_NO_ORPHAN_SCAN", "1")
	d := newLandRepo(t)
	if err := os.MkdirAll(filepath.Join(d, ".slai"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := config.Write(filepath.Join(d, ".slai", "team.yaml"), config.Preset("solo", "demo", "d", "main")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	bad, err := doctorJSON(context.Background(), d, false, false, &out)
	if err != nil {
		t.Fatal(err)
	}
	var rep struct {
		Checks     []struct{ Name, Status, Message string }
		Problems   int
		Orphans    []doctorTeam
		OtherTeams []doctorTeam `json:"other_teams"`
	}
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("doctor --json is not one JSON document: %v\n%s", err, out.String())
	}
	if rep.Problems != bad || rep.Orphans == nil || rep.OtherTeams == nil {
		t.Fatalf("problems=%d (want %d), lists must be [] not null: %+v", rep.Problems, bad, rep)
	}
	byName := map[string]string{}
	for _, c := range rep.Checks {
		if c.Name == "" || c.Message == "" || (c.Status != "ok" && c.Status != "warn" && c.Status != "fail" && c.Status != "info") {
			t.Errorf("bad check %+v", c)
		}
		byName[c.Name] = c.Status
	}
	if byName["git-repository"] != "ok" || byName["team.yaml"] != "ok" {
		t.Errorf("checks = %v", byName)
	}
	// the same checks as the text report
	var text strings.Builder
	runDoctorChecks(context.Background(), d, false, false, &text)
	if n := strings.Count(text.String(), "\n"); len(rep.Checks) != n {
		t.Errorf("%d checks in json, %d lines in the text:\n%s", len(rep.Checks), n, text.String())
	}
}

func TestParseDoctorChecks(t *testing.T) {
	got := parseDoctorChecks("✓ tmux    tmux 3.4\n✗ git     missing — install git\n! warn: worktree for eng-1 missing (/x)\n- no state.db yet\nteam health:\n  ✓ supervisor running (pid 1)\n  ⚠ supervisor is running an older slai build\n  eng-1        idle         reason=- last-activity=never escalation=none\nOther slai teams:\n  ORPHAN /x [slai_1*] 1 session(s)\n  stop the orphans with `slai down --orphans`\n")
	want := []doctorCheck{
		{"tmux", "ok", "tmux tmux 3.4"}, {"git", "fail", "git missing — install git"}, {"worktree", "warn", "worktree for eng-1 missing (/x)"},
		{"state.db", "info", "no state.db yet"}, {"team health", "ok", "supervisor running (pid 1)"},
		{"team health", "warn", "supervisor is running an older slai build"},
		{"team health", "info", "eng-1 idle reason=- last-activity=never escalation=none"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("check %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestDoctorJSONListsOrphans(t *testing.T) {
	t.Setenv("SLAI_NO_ORPHAN_SCAN", "")
	cur, gone, _ := orphanEnv(t)
	var out bytes.Buffer
	if _, err := doctorJSON(context.Background(), cur, false, false, &out); err != nil {
		t.Fatal(err)
	}
	var rep struct {
		Orphans    []doctorTeam
		OtherTeams []doctorTeam `json:"other_teams"`
	}
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if len(rep.Orphans) != 1 || rep.Orphans[0].Project != gone || !rep.Orphans[0].Orphan || rep.Orphans[0].Sessions != 2 {
		t.Fatalf("orphans = %+v (gone=%s)", rep.Orphans, gone)
	}
}
