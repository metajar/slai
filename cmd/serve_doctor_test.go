package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/config"
)

// The web Doctor page's text is the text `slai doctor` prints: the page calls the same
// runDoctorChecks, so the two cannot disagree.
func TestWebDoctorReportIsTheDoctorsText(t *testing.T) {
	t.Setenv("SLAI_NO_ORPHAN_SCAN", "1") // hermetic: do not look at the host's other teams
	d := t.TempDir()                     // no state.db, so nothing in the output depends on the clock
	if err := os.MkdirAll(filepath.Join(d, ".slai"), 0o755); err != nil {
		t.Fatal(err)
	}
	tm := config.Preset("solo", "demo", "d", "main")
	if err := config.Write(filepath.Join(d, ".slai", "team.yaml"), tm); err != nil {
		t.Fatal(err)
	}
	a := webSup(t, d)
	a.Dir = d
	got, fails, err := a.DoctorReport(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	wantFails, _ := runDoctorChecks(context.Background(), d, false, false, cliOrphanFinder(d), &want)
	if got != want.String() || fails != wantFails || got == "" {
		t.Fatalf("web doctor text differs from slai doctor:\nweb:\n%s\ncli:\n%s", got, want.String())
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "killed") || strings.Contains(line, "removed") {
			t.Errorf("the web doctor must never fix anything: %q", line)
		}
	}
}

func TestWebDoctorProbes(t *testing.T) {
	d := newLandRepo(t)
	if err := os.MkdirAll(filepath.Join(d, ".slai"), 0o755); err != nil {
		t.Fatal(err)
	}
	tm := config.Preset("solo", "demo", "d", "main")
	if err := config.Write(filepath.Join(d, ".slai", "team.yaml"), tm); err != nil {
		t.Fatal(err)
	}
	a := webSup(t, d)
	a.Dir = d
	ctx := context.Background()
	lane, err := a.DoctorProbe(ctx, "lane")
	if err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	if _, err := doctorLane(tm, d, "", &want); err != nil {
		t.Fatal(err)
	}
	if lane != want.String() || !strings.Contains(lane, "✓") {
		t.Fatalf("lane probe differs from slai doctor --lane:\n%s\nvs\n%s", lane, want.String())
	}
	panes, err := a.DoctorProbe(ctx, "panes")
	if err != nil || !strings.Contains(panes, "pane decisions") {
		t.Fatalf("panes probe: %v %q", err, panes)
	}
	if _, err := a.DoctorProbe(ctx, "fix"); err == nil {
		t.Fatal("an unknown probe must be refused")
	}
}
