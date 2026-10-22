package supervisor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ErrRunning is returned by Acquire when another supervisor holds the lock.
var ErrRunning = errors.New("supervisor already running")

// PidPath is the supervisor pidfile (also the flock target).
func PidPath(dir string) string { return filepath.Join(dir, ".slai", "run", "supervisor.pid") }

// LogPath is where a detached supervisor logs.
func LogPath(dir string) string { return filepath.Join(dir, ".slai", "run", "supervisor.log") }

// Lock is the single-supervisor flock.
type Lock struct {
	f   *os.File
	dir string
}

// Acquire takes the exclusive supervisor lock and writes our pid. The lock is
// released automatically if the process dies, so stale pidfiles are harmless.
func Acquire(dir string) (*Lock, error) {
	p := PidPath(dir)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		pid := readPid(f)
		f.Close()
		return nil, fmt.Errorf("%w (pid %d)", ErrRunning, pid)
	}
	_ = os.Remove(StoppedPath(dir))
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	return &Lock{f: f, dir: dir}, nil
}

// Release drops the lock.
func (l *Lock) Release() {
	if l == nil || l.f == nil {
		return
	}
	// A clean exit removes the pidfile (while still holding the lock), so a
	// leftover pidfile with a free lock means the supervisor crashed.
	_ = os.Remove(PidPath(l.dir))
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	l.f.Close()
	l.f = nil
}

func readPid(f *os.File) int {
	b := make([]byte, 32)
	n, _ := f.ReadAt(b, 0)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b[:n])))
	return pid
}

// Running reports whether a supervisor currently holds the lock, and its pid.
func Running(dir string) (int, bool) {
	f, err := os.Open(PidPath(dir))
	if err != nil {
		return 0, false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return 0, false
	}
	return readPid(f), true
}

// StopDaemon SIGTERMs a running supervisor and waits for it to exit.
// Returns false if none was running.
func StopDaemon(dir string, timeout time.Duration) (bool, error) {
	pid, ok := Running(dir)
	if !ok {
		return false, nil
	}
	if pid > 0 {
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			return true, err
		}
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, ok := Running(dir); !ok {
			return true, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return true, fmt.Errorf("supervisor (pid %d) did not exit within %s", pid, timeout)
}

// StoppedPath marks a supervisor stopped on purpose by `slai down`; hooks must
// not respawn it while the marker exists. Acquire clears it.
func StoppedPath(dir string) string { return filepath.Join(dir, ".slai", "run", "supervisor.stopped") }

// MarkStopped records that the user stopped the supervisor on purpose.
func MarkStopped(dir string) {
	_ = os.MkdirAll(filepath.Dir(StoppedPath(dir)), 0o755)
	_ = os.WriteFile(StoppedPath(dir), []byte(time.Now().Format(time.RFC3339)+"\n"), 0o644)
}

// ShouldRespawn reports whether a dead supervisor should be restarted by a hook:
// it ran before (pidfile exists), nobody holds the lock, and `slai down` did not
// stop it on purpose.
func ShouldRespawn(dir string) bool {
	if _, err := os.Stat(PidPath(dir)); err != nil {
		return false
	}
	if _, err := os.Stat(StoppedPath(dir)); err == nil {
		return false
	}
	_, running := Running(dir)
	return !running
}

// TryLockSpawn takes the short-lived lock that serializes supervisor respawns, so
// agents ending turns together start one daemon, not eight. ok is false when
// another process is already respawning it.
func TryLockSpawn(dir string) (unlock func(), ok bool) {
	f, err := os.OpenFile(filepath.Join(dir, ".slai", "run", "supervisor.spawn.lock"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, false
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, false
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, true
}
