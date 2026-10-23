package e2e

import "os/exec"

// newSleeper starts nothing yet: a long sleep whose command line names marker, standing in for
// an agent process that outlived its project.
func newSleeper(marker string) *exec.Cmd {
	return exec.Command("sh", "-c", "sleep 300", "slai-e2e-probe", marker)
}
