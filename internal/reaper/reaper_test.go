package reaper

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

func start(t *testing.T, env ...string) *exec.Cmd {
	t.Helper()
	c := exec.Command("sleep", "60")
	c.Env = append(os.Environ(), env...)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Process.Kill(); _, _ = c.Process.Wait() })
	return c
}

func TestFindAndKillAllOnlyTouchTaggedProcesses(t *testing.T) {
	dir := t.TempDir()
	mine := start(t, "SLAI_PROJECT="+dir)
	child := exec.Command("sh", "-c", "sleep 60 & wait") // grandchild inherits the tag
	child.Env = append(os.Environ(), "SLAI_PROJECT="+dir)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _, _ = child.Process.Wait() })
	other := start(t, "SLAI_PROJECT=/somewhere/else")
	untagged := start(t)
	time.Sleep(200 * time.Millisecond)

	procs, err := Find(dir)
	if err != nil {
		t.Fatal(err)
	}
	pids := map[int]bool{}
	for _, p := range procs {
		pids[p.PID] = true
	}
	if !pids[mine.Process.Pid] || !pids[child.Process.Pid] {
		t.Fatalf("tagged processes not found: %v", procs)
	}
	if pids[other.Process.Pid] || pids[untagged.Process.Pid] || pids[os.Getpid()] {
		t.Fatalf("found a process that is not ours: %v", procs)
	}

	killed, survivors, err := KillAll(dir, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if killed < 3 || len(survivors) != 0 {
		t.Fatalf("killed=%d survivors=%v", killed, survivors)
	}
	for _, c := range []*exec.Cmd{mine, child} {
		go c.Wait()
	}
	time.Sleep(100 * time.Millisecond)
	if !alive(other.Process.Pid) || !alive(untagged.Process.Pid) {
		t.Fatal("KillAll touched processes outside the project")
	}
}

func TestFindSkipsOwnAncestors(t *testing.T) {
	// the test binary itself carries the tag; it must never report itself
	t.Setenv("SLAI_PROJECT", "/tmp/reaper-self")
	procs, _ := Find("/tmp/reaper-self")
	for _, p := range procs {
		if p.PID == os.Getpid() {
			t.Fatal("Find reported its own process")
		}
	}
}
