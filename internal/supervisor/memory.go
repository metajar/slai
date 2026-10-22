package supervisor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oorbt/slai/internal/git"
	"github.com/oorbt/slai/internal/memory"
	"github.com/oorbt/slai/internal/store"
)

// kvMemoryIntegration holds the integration commit the fact index was last brought up to.
const kvMemoryIntegration = "memory:integration_sha"

// memoryNewFileGrace: a file this young, indexed or not, may be slai's own write from
// another process, so it is looked at on a later tick.
const memoryNewFileGrace = 2 * time.Second

// memFS is the filesystem seam of the memory tick (tests count content reads).
type memFS interface {
	ReadDir(dir string) ([]fs.DirEntry, error)
	ReadFile(name string) ([]byte, error)
}

type osMemFS struct{}

func (osMemFS) ReadDir(dir string) ([]fs.DirEntry, error) { return os.ReadDir(dir) }
func (osMemFS) ReadFile(name string) ([]byte, error)      { return os.ReadFile(name) }

// memGit is what the memory tick needs of git: memory.Git plus the paths between commits.
type memGit interface {
	memory.Git
	DiffNames(ctx context.Context, a, b string) ([]string, error)
}

type fileStat struct {
	size  int64
	mtime int64
}

// memWatch is the memory tick's per-process cache: the size and mtime each file had
// when it was last read, so an unchanged tree costs a directory listing and no reads.
type memWatch struct {
	stat map[string]fileStat
	// test seams (nil: the real filesystem and the project's git repo)
	fs  memFS
	git memGit
}

func (s *Supervisor) memoryDir() string { return filepath.Join(s.Dir, ".slai", "memory") }

func (s *Supervisor) memoryGit() memGit {
	if s.mem.git != nil {
		return s.mem.git
	}
	return &git.Repo{Dir: s.Dir}
}

// memoryTick keeps the fact index in step with what is on disk and on integration:
//   - a fact file that changed outside slai's own writes (size/mtime differ, content hash
//     differs from the index) is a hand edit: accepted as verified by the user, or flagged
//     invalid and left alone;
//   - when integration moved, only facts citing a path in the diff are re-checked.
//
// Nothing here ever deletes a file.
func (s *Supervisor) memoryTick(ctx context.Context) {
	ents, err := s.memFS().ReadDir(s.memoryDir())
	if err != nil {
		return // no memory directory: nothing to watch
	}
	g := s.memoryGit()
	m := memory.New(s.memoryDir(), s.Store, g, s.Team.Project.IntegrationBranch)
	m.Now = s.now
	rows, err := s.Store.ListFacts()
	if err != nil {
		return
	}
	byID := map[string]store.FactRow{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	if s.mem.stat == nil {
		s.mem.stat = map[string]fileStat{}
	}
	seen := map[string]bool{}
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") || strings.HasPrefix(name, ".") {
			continue
		}
		id := strings.TrimSuffix(name, ".md")
		seen[id] = true
		info, err := e.Info()
		if err != nil {
			continue
		}
		st := fileStat{info.Size(), info.ModTime().UnixNano()}
		if s.mem.stat[id] == st {
			continue
		}
		// A file this young may be slai's own write (an MCP server process renames it,
		// then updates the index): look at it on a later tick, indexed or not.
		if s.now().Sub(info.ModTime()) < memoryNewFileGrace {
			continue
		}
		row, indexed := byID[id]
		data, err := s.memFS().ReadFile(filepath.Join(s.memoryDir(), name))
		if err != nil {
			continue
		}
		sum := sha256.Sum256(data)
		if indexed && row.FileHash == hex.EncodeToString(sum[:]) {
			s.mem.stat[id] = st // slai's own write, or a touch
			continue
		}
		f, rewrote, err := m.AcceptEdit(ctx, id, data)
		if err != nil {
			if row.Status != "invalid" || row.FileHash != hex.EncodeToString(sum[:]) {
				s.event("", "memory_invalid", map[string]any{"fact": id, "reason": err.Error()})
			}
			s.mem.stat[id] = st
			continue
		}
		if fi, err := os.Stat(filepath.Join(s.memoryDir(), name)); err == nil {
			st = fileStat{fi.Size(), fi.ModTime().UnixNano()}
		}
		s.mem.stat[id] = st
		s.event("", "memory_edited", map[string]any{"fact": f.ID, "verified_by": f.VerifiedBy, "rewritten": rewrote})
	}
	for id := range byID {
		if !seen[id] {
			_ = s.Store.DeleteFact(id) // the file was deleted by hand
			delete(s.mem.stat, id)
		}
	}
	s.memoryIntegrationMoved(ctx, m, g)
}

func (s *Supervisor) memFS() memFS {
	if s.mem.fs != nil {
		return s.mem.fs
	}
	return osMemFS{}
}

// memoryIntegrationMoved re-checks the facts that cite a path changed since the last
// tick's integration commit. Unaffected facts are not re-checked; their index sha is
// bumped without touching git or any file.
func (s *Supervisor) memoryIntegrationMoved(ctx context.Context, m *memory.Memory, g memGit) {
	cur, err := g.RevParse(ctx, s.Team.Project.IntegrationBranch)
	if err != nil {
		return
	}
	old := s.kvGet(kvMemoryIntegration)
	if old == cur {
		return
	}
	var changed map[string]bool
	if old != "" {
		if names, err := g.DiffNames(ctx, old, cur); err == nil {
			changed = map[string]bool{}
			for _, n := range names {
				changed[n] = true
			}
		}
	}
	if changed == nil { // first run, or the old commit is gone: check everything
		if err := m.Reindex(ctx); err == nil {
			_ = s.Store.SetKV(kvMemoryIntegration, cur)
		}
		return
	}
	rows, err := s.Store.ListFacts()
	if err != nil {
		return
	}
	now := s.now().Unix()
	for _, r := range rows {
		if r.Status == "invalid" {
			continue
		}
		var paths []memory.Path
		_ = json.Unmarshal([]byte(r.PathsJSON), &paths)
		affected := false
		for _, p := range paths {
			if changed[p.Path] {
				affected = true
				break
			}
		}
		if !affected {
			_ = s.Store.SetFactCheck(r.ID, r.Stale, r.StaleReason, cur, now)
			continue
		}
		res, err := m.CheckPaths(ctx, paths, cur)
		if err != nil {
			return // retry the whole move next tick
		}
		_ = s.Store.SetFactCheck(r.ID, res.Stale, res.Reason, cur, now)
		if res.Stale && !r.Stale {
			s.event("", "memory_stale", map[string]any{"fact": r.ID, "reason": res.Reason})
		}
	}
	_ = s.Store.SetKV(kvMemoryIntegration, cur)
}
