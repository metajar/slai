package memory

import (
	"errors"
	"fmt"
)

// MarkVerified records that a human re-proved the fact: status verified, verified_by by.
// Only the fact file's frontmatter changes (the cited blobs are not re-recorded). It is
// the one write besides Put and Delete, and goes through the same atomic writer.
func (m *Memory) MarkVerified(id, by string) (Fact, error) {
	if by == "" {
		return Fact{}, errors.New("memory: verified_by is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	f, err := m.readFile(id)
	if err != nil {
		return Fact{}, fmt.Errorf("memory: %s: %w", id, err)
	}
	f.Status, f.VerifiedBy = StatusVerified, by
	data := f.Marshal()
	if err := m.writeAtomic(id, data); err != nil {
		return Fact{}, err
	}
	row := rowOf(f, hashBytes(data))
	if old, err := m.DB.GetFact(id); err == nil { // keep the computed staleness; only the file changed
		row.Stale, row.StaleReason, row.CheckedSHA, row.CheckedAt = old.Stale, old.StaleReason, old.CheckedSHA, old.CheckedAt
	}
	return f, m.DB.UpsertFact(row)
}
