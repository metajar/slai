package e2e

import (
	"os"
	"os/exec"
)

// newSleeper starts nothing yet: a long sleep that carries marker in its own environment,
// standing in for an agent process that outlived its project. It is the sleeper itself (no
// wrapping shell), so killing the pid the audit finds leaves no orphan behind.
func newSleeper(marker string) *exec.Cmd {
	c := exec.Command("sleep", "300")
	c.Env = append(os.Environ(), "SLAI_E2E_PROBE="+marker)
	return c
}
