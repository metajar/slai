package cmd

import (
	"os"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/config"
)

func TestWizardHarnessHelpers(t *testing.T) {
	missing := func(string) (string, error) { return "", os.ErrNotExist }
	have := func(s string) (string, error) { return "/bin/" + s, nil }
	if harnessInstalled(config.HarnessClaude, missing) != nil {
		t.Error("claude is covered by preflight, not the harness check")
	}
	if err := harnessInstalled(config.HarnessPi, missing); err == nil || !strings.Contains(err.Error(), "pi not found") {
		t.Errorf("missing pi: %v", err)
	}
	if harnessInstalled(config.HarnessOpenCode, have) != nil {
		t.Error("installed opencode must pass")
	}
	opts := harnessOptions(missing)
	if len(opts) != 3 || opts[0].Key != "claude" || opts[1].Key != "opencode (not found on PATH)" || opts[2].Key != "pi (not found on PATH)" {
		t.Errorf("options = %+v", opts)
	}
	for _, v := range []string{"zai/glm-5.3", " zhipuai-coding-plan/glm-5.3 "} {
		if validProviderModel(v) != nil {
			t.Errorf("%q should be valid", v)
		}
	}
	for _, v := range []string{"", "glm-5.3", "/glm", "zai/", " / "} {
		if validProviderModel(v) == nil {
			t.Errorf("%q should be rejected", v)
		}
	}
}
