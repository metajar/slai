// Package buildinfo identifies the running slai binary, so the supervisor and
// the agents' MCP servers can tell whether they run the code that is on disk.
package buildinfo

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"runtime/debug"
	"strings"
	"sync"
)

var (
	once sync.Once
	id   string
)

// ID names this build exactly: the VCS revision ("-dirty" when the tree had local
// changes) plus a short hash of the executable's contents, because two builds of
// one revision (different flags or toolchain, or any dirty tree) are different
// code. A binary without VCS stamps is just the content hash.
func ID() string {
	once.Do(func() { id = compute() })
	return id
}

func revision() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var rev string
	dirty := false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if rev != "" && dirty {
		rev += "-dirty"
	}
	return rev
}

var (
	hashOnce sync.Once
	exeHash  string
)

// ExeHash is the content hash of the running executable, computed once.
func ExeHash() string {
	hashOnce.Do(func() { exeHash = HashFile(ExePath()) })
	return exeHash
}

func compute() string {
	h := ExeHash()
	if rev := revision(); rev != "" {
		return rev + "." + h[:min(8, len(h))]
	}
	return h
}

// Version is the build for display: the VCS revision, or the id when there is none.
func Version() string {
	if rev := revision(); rev != "" {
		return rev
	}
	return ID()
}

// ExePath is the path of the running executable. A binary replaced on disk
// while running reads back from /proc with a " (deleted)" suffix, which is
// dropped so the path names whatever now sits there.
func ExePath() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(p, " (deleted)")
}

// HashFile returns a short sha256 of the file's contents ("unknown" if unreadable).
func HashFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "unknown"
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}
