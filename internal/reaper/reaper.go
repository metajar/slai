// Package reaper finds and kills processes that slai agents started.
//
// Every agent is launched with SLAI_PROJECT=<project dir> in its environment and
// children inherit it, so anything an agent spawned (Claude background sessions
// and their daemon, MCP servers, dev servers the agent started) can be found by
// that tag even after the tmux session that started it is gone.
package reaper

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Proc is a process belonging to a project's agents.
type Proc struct {
	PID int
	Cmd string
}

func (p Proc) String() string { return fmt.Sprintf("%d %s", p.PID, p.Cmd) }

// Find lists processes whose environment carries SLAI_PROJECT=<dir>, excluding
// this process and its ancestors (so `slai down` run from inside an agent's
// shell cannot kill itself).
func Find(dir string) ([]Proc, error) {
	tags := tagsFor(dir)
	skip := ancestors(os.Getpid())
	var procs []Proc
	var err error
	if runtime.GOOS == "linux" {
		procs, err = findProc(tags)
	} else {
		procs, err = findPS(tags)
	}
	if err != nil {
		return nil, err
	}
	out := procs[:0]
	for _, p := range procs {
		if !skip[p.PID] {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out, nil
}

// ListProjects groups every process that carries an SLAI_PROJECT tag by project
// directory (excluding this process and its ancestors). Linux reads /proc; elsewhere
// it is best effort via ps.
func ListProjects() (map[string][]Proc, error) {
	skip := ancestors(os.Getpid())
	out := map[string][]Proc{}
	if runtime.GOOS != "linux" {
		b, err := exec.Command("ps", "eww", "-A", "-o", "pid=,command=").Output()
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			sp := strings.IndexByte(line, ' ')
			i := strings.Index(line, " SLAI_PROJECT=")
			if sp < 0 || i < 0 {
				continue
			}
			pid, err := strconv.Atoi(line[:sp])
			if err != nil || skip[pid] {
				continue
			}
			proj := strings.Fields(line[i+len(" SLAI_PROJECT="):])
			if len(proj) > 0 {
				out[proj[0]] = append(out[proj[0]], Proc{PID: pid, Cmd: compact(strings.TrimSpace(line[sp:i]))})
			}
		}
		return out, nil
	}
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || skip[pid] {
			continue
		}
		env, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
		if err != nil {
			continue
		}
		for _, kv := range bytes.Split(env, []byte{0}) {
			if v, ok := strings.CutPrefix(string(kv), "SLAI_PROJECT="); ok && v != "" {
				out[v] = append(out[v], Proc{PID: pid, Cmd: cmdline(pid)})
				break
			}
		}
	}
	return out, nil
}

func tagsFor(dir string) []string {
	tags := []string{"SLAI_PROJECT=" + dir}
	if r, err := filepath.EvalSymlinks(dir); err == nil && r != dir {
		tags = append(tags, "SLAI_PROJECT="+r)
	}
	return tags
}

func findProc(tags []string) ([]Proc, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var out []Proc
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		env, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
		if err != nil {
			continue // other user's process or already gone
		}
		for _, kv := range bytes.Split(env, []byte{0}) {
			if hasTag(string(kv), tags) {
				out = append(out, Proc{PID: pid, Cmd: cmdline(pid)})
				break
			}
		}
	}
	return out, nil
}

// compact collapses whitespace (agents' command lines embed whole system prompts)
// and truncates for display.
func compact(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 100 {
		s = string(r[:100]) + "…"
	}
	return s
}

func hasTag(kv string, tags []string) bool {
	for _, t := range tags {
		if kv == t {
			return true
		}
	}
	return false
}

func cmdline(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil || len(b) == 0 {
		return "?"
	}
	return compact(strings.ReplaceAll(string(b), "\x00", " "))
}

// findPS is the best-effort fallback for systems without /proc (macOS):
// `ps eww` appends each process's environment to its command line.
func findPS(tags []string) ([]Proc, error) {
	out, err := exec.Command("ps", "eww", "-A", "-o", "pid=,command=").Output()
	if err != nil {
		return nil, err
	}
	var procs []Proc
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		sp := strings.IndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		pid, err := strconv.Atoi(line[:sp])
		if err != nil {
			continue
		}
		for _, t := range tags {
			if strings.Contains(line, " "+t+" ") || strings.HasSuffix(line, " "+t) {
				cmd := strings.TrimSpace(line[sp:])
				if i := strings.Index(cmd, " "+t); i > 0 {
					cmd = cmd[:i]
				}
				procs = append(procs, Proc{PID: pid, Cmd: compact(cmd)})
				break
			}
		}
	}
	return procs, nil
}

// ancestors returns pid and its parent chain.
func ancestors(pid int) map[int]bool {
	skip := map[int]bool{}
	for pid > 1 && !skip[pid] {
		skip[pid] = true
		pid = ppid(pid)
	}
	return skip
}

func ppid(pid int) int {
	if runtime.GOOS == "linux" {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return 0
		}
		// "pid (comm) S ppid ..." - comm may contain spaces/parens, so split after the last ')'.
		s := string(b)
		i := strings.LastIndexByte(s, ')')
		if i < 0 {
			return 0
		}
		f := strings.Fields(s[i+1:])
		if len(f) < 2 {
			return 0
		}
		n, _ := strconv.Atoi(f[1])
		return n
	}
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// KillAll terminates every process Find reports for dir: SIGTERM, a grace period,
// then SIGKILL. It repeats because a surviving parent (Claude's background daemon)
// can respawn children. It returns how many processes it killed and any survivors.
func KillAll(dir string, grace time.Duration) (killed int, survivors []Proc, err error) {
	seen := map[int]bool{}
	for round := 0; round < 4; round++ {
		procs, err := Find(dir)
		if err != nil {
			return killed, nil, err
		}
		if len(procs) == 0 {
			return killed, nil, nil
		}
		for _, p := range procs {
			if !seen[p.PID] {
				seen[p.PID] = true
				killed++
			}
			_ = syscall.Kill(p.PID, syscall.SIGTERM)
		}
		deadline := time.Now().Add(grace)
		for time.Now().Before(deadline) {
			if !anyAlive(procs) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		for _, p := range procs {
			if alive(p.PID) {
				_ = syscall.Kill(p.PID, syscall.SIGKILL)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	survivors, _ = Find(dir)
	return killed, survivors, nil
}

func anyAlive(ps []Proc) bool {
	for _, p := range ps {
		if alive(p.PID) {
			return true
		}
	}
	return false
}
