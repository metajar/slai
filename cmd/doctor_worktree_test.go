package cmd

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorktreeIndexCheck(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@e.com"}, args...)...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x\n"), 0o644)
	run("add", "-A")
	run("commit", "-qm", "init")

	var buf bytes.Buffer
	worktreeIndexCheck(context.Background(), "eng-1", dir, &buf)
	if buf.Len() != 0 {
		t.Fatalf("clean worktree flagged: %s", buf.String())
	}
	// an unstaged edit is normal work in progress
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("y\n"), 0o644)
	worktreeIndexCheck(context.Background(), "eng-1", dir, &buf)
	if buf.Len() != 0 {
		t.Fatalf("unstaged edit flagged: %s", buf.String())
	}
	run("rm", "-q", "--cached", "a.txt")
	worktreeIndexCheck(context.Background(), "eng-1", dir, &buf)
	if !strings.Contains(buf.String(), "eng-1") || !strings.Contains(buf.String(), "staged") || !strings.Contains(buf.String(), "a.txt") {
		t.Fatalf("staged deletion not flagged: %q", buf.String())
	}
}
