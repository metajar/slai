package e2e

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Test hygiene (T-0038). Each e2e project owns a supervisor, a private tmux server and fake
// agents. Before this file a test killed by its timeout, or one whose agents outlived `slai
// down`, left them running for days (seen: 20 to 35 hours old), and a few `go test` runs at
// once piled a dozen supervisors onto the machine the real team runs on.
//
//   - auditLeaks runs at the end of every project's cleanup: anything still alive that names
//     the project's directory or tmux socket is killed and the test FAILS.
//   - TestMain does the same for anything still running from the test binaries' directory.
//   - acquireSlot caps how many e2e projects run at once on the whole machine (across
//     `go test` processes), so concurrent runs queue instead of piling up.

// e2eSlots is how many e2e projects may run at once machine-wide. SLAI_E2E_SLOTS overrides it.
func e2eSlots() int {
	if n, err := strconv.Atoi(os.Getenv("SLAI_E2E_SLOTS")); err == nil && n > 0 {
		return n
	}
	return 2
}

const slotWait = 8 * time.Minute

// acquireSlot takes one machine-wide slot (an flock, released when the file is closed or the
// process dies, so a killed test run cannot hold a slot) and returns the release function.
// If no slot frees up within slotWait it logs and runs anyway: a stuck neighbour must not
// fail this test.
func acquireSlot(t *testing.T) (release func()) {
	t.Helper()
	n := e2eSlots()
	deadline := time.Now().Add(slotWait)
	for {
		for i := 0; i < n; i++ {
			f, err := os.OpenFile(filepath.Join(os.TempDir(), fmt.Sprintf("slai-e2e-slot-%d.lock", i)), os.O_CREATE|os.O_RDWR, 0o666)
			if err != nil {
				continue
			}
			if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
				return func() { _ = f.Close() }
			}
			_ = f.Close()
		}
		if time.Now().After(deadline) {
			t.Logf("no e2e slot free after %s; running anyway", slotWait)
			return func() {}
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// procsMatching lists the pids (never this process) whose command line or environment
// contains any of the needles. Linux only (reads /proc); elsewhere it finds nothing.
func procsMatching(needles ...string) []int {
	if runtime.GOOS != "linux" {
		return nil
	}
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var out []int
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		hay := readProc(pid, "cmdline")
		hay = append(hay, readProc(pid, "environ")...)
		for _, n := range needles {
			if n != "" && bytes.Contains(hay, []byte(n)) {
				out = append(out, pid)
				break
			}
		}
	}
	return out
}

func readProc(pid int, name string) []byte {
	b, _ := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), name))
	return b
}

func cmdlineOf(pid int) string {
	s := strings.ReplaceAll(string(readProc(pid, "cmdline")), "\x00", " ")
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return strings.TrimSpace(s)
}

// reapLeaks waits up to grace for the processes naming needles to exit on their own, kills
// the ones that do not, and describes what it had to kill.
func reapLeaks(grace time.Duration, needles ...string) []string {
	deadline := time.Now().Add(grace)
	var left []int
	for {
		left = procsMatching(needles...)
		if len(left) == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	var desc []string
	for _, pid := range left {
		desc = append(desc, fmt.Sprintf("pid %d: %s", pid, cmdlineOf(pid)))
	}
	for round := 0; round < 3 && len(left) > 0; round++ { // a killed supervisor's child may respawn once
		for _, pid := range left {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		time.Sleep(200 * time.Millisecond)
		left = procsMatching(needles...)
	}
	return desc
}

// auditLeaks fails the test for every process still alive after the project was torn down.
func (p *project) auditLeaks() {
	if desc := reapLeaks(5*time.Second, p.dir, p.sock); len(desc) > 0 {
		p.t.Errorf("e2e leaked %d process(es) after slai down (killed now):\n  %s", len(desc), strings.Join(desc, "\n  "))
	}
}

// TestAuditKillsAndReportsALeak: a process that names a project directory and outlives it is
// found, killed, and reported (this is what turns a leak into a failing test).
func TestAuditKillsAndReportsALeak(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo-leak-probe")
	cmd := newSleeper(dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }()
	if got := procsMatching(dir); len(got) != 1 || got[0] != cmd.Process.Pid {
		t.Fatalf("procsMatching = %v, want [%d]", got, cmd.Process.Pid)
	}
	desc := reapLeaks(300*time.Millisecond, dir)
	if len(desc) != 1 || !strings.Contains(desc[0], dir) {
		t.Fatalf("reapLeaks = %v", desc)
	}
	if got := procsMatching(dir); len(got) != 0 {
		t.Fatalf("still running after reap: %v", got)
	}
}

// TestSlotsAreExclusive: with one slot a second acquire waits until the first is released.
func TestSlotsAreExclusive(t *testing.T) {
	t.Setenv("SLAI_E2E_SLOTS", "1")
	rel := acquireSlot(t)
	got := make(chan func(), 1)
	go func() { got <- acquireSlot(t) }()
	select {
	case <-got:
		t.Fatal("second acquire succeeded while the only slot was held")
	case <-time.After(600 * time.Millisecond):
	}
	rel()
	select {
	case r := <-got:
		r()
	case <-time.After(5 * time.Second):
		t.Fatal("second acquire never got the slot after release")
	}
}
