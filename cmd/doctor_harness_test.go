package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/oorbt/slai/internal/config"
)

func harnessProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	tm := config.Preset("solo", "p", "d", "main")
	tm.Agents = append(tm.Agents,
		config.Agent{ID: "oc-1", Role: config.RoleEngineer, Model: "zhipuai-coding-plan/glm-5.3", Harness: "opencode"},
		config.Agent{ID: "pi-1", Role: config.RoleEngineer, Model: "zai/glm-5.3", Harness: "pi"})
	if err := os.MkdirAll(filepath.Join(dir, ".slai"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := config.Write(filepath.Join(dir, ".slai", "team.yaml"), tm); err != nil {
		t.Fatal(err)
	}
	return dir
}

func fakeBins(t *testing.T, scripts map[string]string) {
	t.Helper()
	bin := t.TempDir()
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
}

const goodOpencode = `case "$*" in
"--version") echo 1.18.16;;
"providers list") echo "zhipuai-coding-plan  api";;
"debug config") echo '{}';;
esac
`
const goodPi = `case "$*" in
"--version") echo "pi 1.3.0";;
"auth check --provider zai --json") echo '{"status":"ready","provider":"zai"}';;
*) echo '{"status":"not_ready"}'; exit 1;;
esac
`

// AC 16 (golden): both harnesses installed and authenticated.
func TestDoctorHarnessesGolden(t *testing.T) {
	dir := harnessProject(t)
	fakeBins(t, map[string]string{"opencode": goodOpencode, "pi": goodPi})
	// the run files exist (written by `slai up`)
	for id, files := range map[string][]string{"oc-1": {"opencode.json", "plugin.js", "system.md"}, "pi-1": {"system.md", "pi-extension.mjs"}} {
		run := filepath.Join(dir, ".slai", "run", id)
		_ = os.MkdirAll(run, 0o755)
		for _, f := range files {
			_ = os.WriteFile(filepath.Join(run, f), nil, 0o644)
		}
	}
	var buf bytes.Buffer
	if bad := doctorHarnesses(context.Background(), dir, &buf); bad != 0 {
		t.Fatalf("bad = %d\n%s", bad, buf.String())
	}
	want := `Harnesses:
  ✓ opencode  1.18.16
    ✓ auth for provider zhipuai-coding-plan
    - oc-1: degraded: statusline (not supported by opencode), resume_by_id (not supported by opencode)
  ✓ pi        pi 1.3.0
    ✓ auth for provider zai
    - pi-1: degraded: statusline (not supported by pi)
    - note: pi's ` + "`-e builtin:mcp`" + ` also connects every MCP server in your ~/.pi/agent/mcp.json, not only slai's
`
	if buf.String() != want {
		t.Fatalf("output:\n%s\nwant:\n%s", buf.String(), want)
	}
}

// AC 16: a missing binary or missing provider auth is a failure.
// A harness whose run files were never generated is called out, not silently fine.
func TestDoctorHarnessesNotesMissingRunFiles(t *testing.T) {
	dir := harnessProject(t)
	fakeBins(t, map[string]string{"opencode": goodOpencode, "pi": goodPi})
	var buf bytes.Buffer
	if bad := doctorHarnesses(context.Background(), dir, &buf); bad != 0 {
		t.Fatalf("bad = %d", bad)
	}
	for _, want := range []string{"oc-1: run files not generated yet (opencode.json, plugin.js, system.md)", "pi-1: run files not generated yet (system.md, pi-extension.mjs)"} {
		if !bytesContains(buf.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, buf.String())
		}
	}
}

func TestDoctorHarnessesFailsOnMissingBinaryOrAuth(t *testing.T) {
	dir := harnessProject(t)
	// no pi on PATH; opencode does not list the configured provider
	fakeBins(t, map[string]string{"opencode": `case "$*" in
"--version") echo 1.18.16;;
"providers list") echo "anthropic";;
esac
`})
	var buf bytes.Buffer
	bad := doctorHarnesses(context.Background(), dir, &buf)
	out := buf.String()
	if bad != 2 {
		t.Fatalf("bad = %d, want 2 (pi binary, opencode auth)\n%s", bad, out)
	}
	for _, want := range []string{"✗ pi        binary not found on PATH (needed by pi-1)", "✗ no auth for provider zhipuai-coding-plan"} {
		if !bytesContains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestDoctorHarnessesPiAuthNotReady(t *testing.T) {
	dir := harnessProject(t)
	fakeBins(t, map[string]string{"opencode": goodOpencode, "pi": `case "$*" in
"--version") echo "pi 1.3.0";;
*) echo '{"status":"not_ready"}'; exit 1;;
esac
`})
	var buf bytes.Buffer
	if bad := doctorHarnesses(context.Background(), dir, &buf); bad != 1 || !bytesContains(buf.String(), "✗ no auth for provider zai") {
		t.Fatalf("bad=%d\n%s", bad, buf.String())
	}
}

// AC 17: an invalid user opencode config is a warning, never a failure.
func TestDoctorWarnsAboutInvalidOpenCodeConfig(t *testing.T) {
	dir := harnessProject(t)
	fakeBins(t, map[string]string{"pi": goodPi, "opencode": `case "$*" in
"--version") echo 1.18.16;;
"providers list") echo "zhipuai-coding-plan";;
"debug config") echo "Configuration is invalid at ~/.config/opencode/opencode.jsonc" >&2; exit 1;;
esac
`})
	var buf bytes.Buffer
	bad := doctorHarnesses(context.Background(), dir, &buf)
	if bad != 0 || !bytesContains(buf.String(), "! warn: your opencode config is invalid") || !bytesContains(buf.String(), "isolated from it") {
		t.Fatalf("bad=%d\n%s", bad, buf.String())
	}
}

func TestDoctorHarnessesSilentForClaudeOnlyTeams(t *testing.T) {
	dir := t.TempDir()
	tm := config.Preset("solo", "p", "d", "main")
	_ = os.MkdirAll(filepath.Join(dir, ".slai"), 0o755)
	_ = config.Write(filepath.Join(dir, ".slai", "team.yaml"), tm)
	var buf bytes.Buffer
	if bad := doctorHarnesses(context.Background(), dir, &buf); bad != 0 || buf.Len() != 0 {
		t.Fatalf("bad=%d out=%q", bad, buf.String())
	}
}

func bytesContains(s, sub string) bool { return bytes.Contains([]byte(s), []byte(sub)) }
