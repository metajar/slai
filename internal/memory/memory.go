package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/oorbt/slai/internal/store"
)

// IntegrationRev is the branch facts are grounded against, never the caller's worktree.
const IntegrationRev = "slai/integration"

// Git is the part of git.Repo the memory package uses.
type Git interface {
	RevParse(ctx context.Context, rev string) (string, error)
	// LsTree returns path -> blob id for the paths that are blobs in rev; one invocation.
	LsTree(ctx context.Context, rev string, paths []string) (map[string]string, error)
}

// ErrUnknownPath is returned by Put when a cited path is not a file on integration.
var ErrUnknownPath = errors.New("memory: cited path does not exist on slai/integration")

// Memory is the fact store for one project.
//
// Concurrency: writes are serialised by an in-process lock only; there is no
// cross-process lock. The CLI and the supervisor may both write: ids are deterministic
// and files are written as temp file + rename, so two writers of the same id produce
// equivalent files and the last rename wins harmlessly. Fact text is read only from the
// files, never from the index.
type Memory struct {
	Dir string // .slai/memory in the project's main checkout
	DB  *store.Store
	Git Git
	Rev string           // rev to ground against; IntegrationRev when empty
	Now func() time.Time // time.Now when nil

	mu sync.Mutex
	// afterTemp, when set, runs after the temp file is written and before the rename
	// (tests inject a crash here).
	afterTemp func() error
	// beforeRename, when set, runs right before the rename, after the index step.
	beforeRename func()
}

// New returns a Memory over dir. rev may be empty (IntegrationRev).
func New(dir string, db *store.Store, g Git, rev string) *Memory {
	return &Memory{Dir: dir, DB: db, Git: g, Rev: rev}
}

func (m *Memory) rev() string {
	if m.Rev != "" {
		return m.Rev
	}
	return IntegrationRev
}

func (m *Memory) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Memory) file(id string) string { return filepath.Join(m.Dir, id+".md") }

func hashBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Put grounds and stores f. The id is derived from kind, text and paths; ID, Created,
// IntegrationSHA and every path's Blob are filled in here (recorded against the
// integration branch; an unknown path is rejected with ErrUnknownPath). When a fact
// with the same id exists, it is returned unchanged with existed true. The caller sets
// Status/VerifiedBy/Author: the policy of who may verify lives in the service.
func (m *Memory) Put(ctx context.Context, f Fact) (_ Fact, existed bool, err error) {
	f.Text = strings.TrimSpace(f.Text)
	if err := f.Validate(); err != nil {
		return Fact{}, false, err
	}
	if f.Status == "" {
		f.Status = StatusProposed
	}
	f.Paths = append([]Path(nil), f.Paths...)
	sort.Slice(f.Paths, func(i, j int) bool { return f.Paths[i].Path < f.Paths[j].Path })
	f.ID = ID(f.Kind, f.Text, f.Paths)

	m.mu.Lock()
	defer m.mu.Unlock()
	if old, err := m.readFile(f.ID); err == nil {
		return old, true, nil
	}
	sha, err := m.Git.RevParse(ctx, m.rev())
	if err != nil {
		return Fact{}, false, fmt.Errorf("memory: resolving %s: %w", m.rev(), err)
	}
	if len(f.Paths) > 0 {
		names := make([]string, len(f.Paths))
		for i, p := range f.Paths {
			names[i] = p.Path
		}
		blobs, err := m.Git.LsTree(ctx, sha, names)
		if err != nil {
			return Fact{}, false, err
		}
		for i, p := range f.Paths {
			b, ok := blobs[p.Path]
			if !ok {
				return Fact{}, false, fmt.Errorf("%w: %s", ErrUnknownPath, p.Path)
			}
			f.Paths[i].Blob = b
		}
	}
	f.IntegrationSHA = sha
	f.Created = m.now().UTC().Format("2006-01-02")
	data := f.Marshal()
	if err := m.writeAtomic(f.ID, data); err != nil {
		return Fact{}, false, err
	}
	row := rowOf(f, hashBytes(data))
	row.CheckedSHA, row.CheckedAt = sha, m.now().Unix()
	if err := m.DB.UpsertFact(row); err != nil {
		return Fact{}, false, err
	}
	return f, false, nil
}

// writeAtomic writes <id>.md via a temp file and rename, so a crash leaves either
// nothing or the whole file. Callers hold m.mu.
func (m *Memory) writeAtomic(id string, data []byte) error { return m.writeAtomicPre(id, data, nil) }

// writeAtomicPre is writeAtomic with a step that runs after the temp file is complete
// and before the rename (the index update of a rewrite, so a reader that sees the new
// file always finds the new row). A failing step or rename leaves the old file in place.
func (m *Memory) writeAtomicPre(id string, data []byte, pre func() error) (err error) {
	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(m.Dir, "."+id+".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if m.afterTemp != nil {
		if err = m.afterTemp(); err != nil {
			return err
		}
	}
	if pre != nil {
		if err = pre(); err != nil {
			return err
		}
	}
	if m.beforeRename != nil {
		m.beforeRename()
	}
	return os.Rename(tmp.Name(), m.file(id))
}

func (m *Memory) readFile(id string) (Fact, error) {
	data, err := os.ReadFile(m.file(id))
	if err != nil {
		return Fact{}, err
	}
	f, err := Parse(data)
	if err != nil {
		return Fact{}, err
	}
	if f.ID != id {
		return Fact{}, fmt.Errorf("memory: file %s.md holds id %s", id, f.ID)
	}
	return f, nil
}

// Get reads and parses one fact file.
func (m *Memory) Get(id string) (Fact, error) { return m.readFile(id) }

// Invalid is a memory file that could not be parsed. It is kept on disk, never deleted.
type Invalid struct {
	ID     string // the file name without .md
	Reason string
	Hash   string
}

// List parses every <id>.md in Dir, sorted by id. Unparseable files are returned
// separately; temp files from an interrupted write are ignored.
func (m *Memory) List() ([]Fact, []Invalid, error) {
	ents, err := os.ReadDir(m.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var facts []Fact
	var bad []Invalid
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") || strings.HasPrefix(name, ".") {
			continue
		}
		id := strings.TrimSuffix(name, ".md")
		data, err := os.ReadFile(filepath.Join(m.Dir, name))
		if err != nil {
			bad = append(bad, Invalid{ID: id, Reason: err.Error()})
			continue
		}
		f, err := Parse(data)
		if err == nil && f.ID != id {
			err = fmt.Errorf("file name does not match id %s", f.ID)
		}
		if err != nil {
			bad = append(bad, Invalid{ID: id, Reason: err.Error(), Hash: hashBytes(data)})
			continue
		}
		facts = append(facts, f)
	}
	return facts, bad, nil
}

// Delete removes a fact's file and index row.
func (m *Memory) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := os.Remove(m.file(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return m.DB.DeleteFact(id)
}

// Result is the outcome of Check.
type Result struct {
	Stale  bool
	Reason string // first changed path, e.g. "internal/x.go changed"; empty when fresh
	SHA    string // the rev that was checked (a commit id when Check was given one)
}

// Check compares f's recorded blobs with those on integration, with a single ls-tree
// call. A changed, deleted or renamed path makes it stale. A fact that cites no path
// cannot go stale (and can never be verified-by-grounding).
func (m *Memory) Check(ctx context.Context, f Fact) (Result, error) {
	return m.checkAt(ctx, f, m.rev())
}

func (m *Memory) checkAt(ctx context.Context, f Fact, rev string) (Result, error) {
	res := Result{SHA: rev}
	if len(f.Paths) == 0 {
		return res, nil
	}
	names := make([]string, len(f.Paths))
	for i, p := range f.Paths {
		names[i] = p.Path
	}
	blobs, err := m.Git.LsTree(ctx, rev, names)
	if err != nil {
		return res, err
	}
	res.Stale, res.Reason = staleAgainst(f, blobs)
	return res, nil
}

func rowOf(f Fact, fileHash string) store.FactRow {
	ps, _ := json.Marshal(append([]Path{}, f.Paths...))
	return store.FactRow{ID: f.ID, Kind: f.Kind, Status: f.Status, Author: f.Author, VerifiedBy: f.VerifiedBy,
		IntegrationSHA: f.IntegrationSHA, Created: f.Created, PathsJSON: string(ps), Decision: f.Decision,
		Check: f.Check, FileHash: fileHash}
}

// Reindex rebuilds facts_index from the files: every valid file gets a row (with
// staleness computed against one resolved integration commit, one ls-tree per fact),
// every unparseable file an "invalid" row, and rows without a file are dropped. The
// usage counters of surviving rows are kept.
func (m *Memory) Reindex(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	facts, bad, err := m.List()
	if err != nil {
		return err
	}
	sha, err := m.Git.RevParse(ctx, m.rev())
	if err != nil {
		return fmt.Errorf("memory: resolving %s: %w", m.rev(), err)
	}
	at := m.now().Unix()
	keep := map[string]bool{}
	for _, f := range facts {
		data, err := os.ReadFile(m.file(f.ID))
		if err != nil {
			return err
		}
		res, err := m.checkAt(ctx, f, sha)
		if err != nil {
			return err
		}
		r := rowOf(f, hashBytes(data))
		r.Stale, r.StaleReason, r.CheckedSHA, r.CheckedAt = res.Stale, res.Reason, sha, at
		if err := m.DB.UpsertFact(r); err != nil {
			return err
		}
		keep[f.ID] = true
	}
	for _, b := range bad {
		r := store.FactRow{ID: b.ID, Status: "invalid", InvalidReason: b.Reason, FileHash: b.Hash, PathsJSON: "[]"}
		if err := m.DB.UpsertFact(r); err != nil {
			return err
		}
		keep[b.ID] = true
	}
	rows, err := m.DB.ListFacts()
	if err != nil {
		return err
	}
	for _, r := range rows {
		if !keep[r.ID] {
			if err := m.DB.DeleteFact(r.ID); err != nil {
				return err
			}
		}
	}
	return nil
}
