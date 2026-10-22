package memory

import (
	"bytes"
	"context"
	"fmt"
	"reflect"

	"github.com/oorbt/slai/internal/store"
)

// CheckPaths is Check for a bare list of cited paths, against rev (a commit id or
// branch): one ls-tree call, none when paths is empty.
func (m *Memory) CheckPaths(ctx context.Context, paths []Path, rev string) (Result, error) {
	return m.checkAt(ctx, Fact{Paths: paths}, rev)
}

// AcceptEdit takes data, the current bytes of <id>.md, which a person edited by hand
// (anything that is not slai's own write). A valid file becomes verified by "user": its
// status and verified_by are set, its cited blobs are re-recorded against the current
// integration commit, and the index row is refreshed. The file is rewritten (atomically)
// only when one of those changes alters it, so the user's formatting is otherwise kept.
// An invalid file gets an "invalid" index row, stays on disk untouched, and the parse
// error is returned. rewrote reports whether the file was rewritten.
func (m *Memory) AcceptEdit(ctx context.Context, id string, data []byte) (f Fact, rewrote bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, err = Parse(data)
	if err == nil && f.ID != id {
		err = fmt.Errorf("file name does not match id %s", f.ID)
	}
	if err != nil {
		row := store.FactRow{ID: id, Status: "invalid", InvalidReason: err.Error(), FileHash: hashBytes(data), PathsJSON: "[]"}
		if uerr := m.DB.UpsertFact(row); uerr != nil {
			return Fact{}, false, uerr
		}
		return Fact{}, false, err
	}
	orig := f
	orig.Paths = append([]Path(nil), f.Paths...)
	sha, err := m.Git.RevParse(ctx, m.rev())
	if err != nil {
		return Fact{}, false, fmt.Errorf("memory: resolving %s: %w", m.rev(), err)
	}
	f.Status, f.VerifiedBy = StatusVerified, "user"
	if f.Created == "" {
		f.Created = m.now().UTC().Format("2006-01-02")
	}
	var res Result
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
			if b, ok := blobs[p.Path]; ok {
				f.Paths[i].Blob = b
			} else if !res.Stale {
				res.Stale, res.Reason = true, p.Path+" no longer exists"
			}
		}
	}
	f.IntegrationSHA = sha
	out := data
	if !reflect.DeepEqual(f, orig) {
		out = f.Marshal()
		if !bytes.Equal(out, data) {
			if err := m.writeAtomic(id, out); err != nil {
				return Fact{}, false, err
			}
			rewrote = true
		}
	}
	row := rowOf(f, hashBytes(out))
	row.Stale, row.StaleReason, row.CheckedSHA, row.CheckedAt = res.Stale, res.Reason, sha, m.now().Unix()
	return f, rewrote, m.DB.UpsertFact(row)
}

// ErrStale is returned by Verify for a fact whose cited blobs changed on integration.
var ErrStale = fmt.Errorf("memory: fact is stale")

// Verify marks the proposed fact id as verified by `by` (e.g. "principal") and rewrites
// its file atomically. A stale fact cannot be verified. The caller (the service) decides
// who may call this; it is the only path that flips proposed to verified besides Put with
// Status verified and a user's hand edit (AcceptEdit).
func (m *Memory) Verify(ctx context.Context, id, by string) (Fact, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, err := m.readFile(id)
	if err != nil {
		return Fact{}, err
	}
	res, err := m.checkAt(ctx, f, m.rev())
	if err != nil {
		return Fact{}, err
	}
	if res.Stale {
		return Fact{}, fmt.Errorf("%w: %s", ErrStale, res.Reason)
	}
	f.Status, f.VerifiedBy = StatusVerified, by
	data := f.Marshal()
	row := rowOf(f, hashBytes(data))
	row.CheckedSHA, row.CheckedAt = res.SHA, m.now().Unix()
	prev, perr := m.DB.GetFact(id)
	// Index first, then rename: a reader that sees the new file finds the new row, so the
	// supervisor's tick can never take slai's own rewrite for a hand edit.
	err = m.writeAtomicPre(id, data, func() error { return m.DB.UpsertFact(row) })
	if err != nil {
		if perr == nil {
			_ = m.DB.UpsertFact(prev) // the old file is still there: so is its row
		}
		return Fact{}, err
	}
	return f, nil
}
