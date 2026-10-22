package store

import (
	"database/sql"
	"errors"
)

// FactRow is one row of facts_index: a derived copy of a Markdown fact file under
// .slai/memory/ (the file wins on any disagreement; see internal/memory). The fact text
// is deliberately not indexed: it is read only from the file. Uses and
// LastUsedAt are the only columns the files cannot reproduce.
type FactRow struct {
	ID, Kind, Status, Author, VerifiedBy string
	IntegrationSHA, Created              string
	PathsJSON                            string // [{"path":..,"blob":..}]
	Decision, Check                      string
	FileHash                             string
	InvalidReason                        string // set when Status is "invalid"
	Stale                                bool
	StaleReason                          string
	CheckedSHA                           string
	CheckedAt                            int64
	Uses                                 int
	LastUsedAt                           int64
}

const factCols = `id, kind, status, author, verified_by, integration_sha, created, paths_json, decision, check_cmd,
  file_hash, invalid_reason, stale, stale_reason, checked_sha, checked_at, uses, last_used_at`

func scanFact(sc interface{ Scan(...any) error }) (FactRow, error) {
	var r FactRow
	var stale int
	err := sc.Scan(&r.ID, &r.Kind, &r.Status, &r.Author, &r.VerifiedBy, &r.IntegrationSHA, &r.Created, &r.PathsJSON,
		&r.Decision, &r.Check, &r.FileHash, &r.InvalidReason, &stale, &r.StaleReason, &r.CheckedSHA,
		&r.CheckedAt, &r.Uses, &r.LastUsedAt)
	r.Stale = stale != 0
	return r, err
}

// UpsertFact writes the row, keeping the existing usage counters.
func (s *Store) UpsertFact(r FactRow) error {
	stale := 0
	if r.Stale {
		stale = 1
	}
	_, err := s.db.Exec(`INSERT INTO facts_index(`+factCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,0,0)
	  ON CONFLICT(id) DO UPDATE SET kind=excluded.kind, status=excluded.status, author=excluded.author,
	  verified_by=excluded.verified_by, integration_sha=excluded.integration_sha, created=excluded.created,
	  paths_json=excluded.paths_json, decision=excluded.decision, check_cmd=excluded.check_cmd,
	  file_hash=excluded.file_hash, invalid_reason=excluded.invalid_reason, stale=excluded.stale,
	  stale_reason=excluded.stale_reason, checked_sha=excluded.checked_sha, checked_at=excluded.checked_at`,
		r.ID, r.Kind, r.Status, r.Author, r.VerifiedBy, r.IntegrationSHA, r.Created, r.PathsJSON, r.Decision, r.Check,
		r.FileHash, r.InvalidReason, stale, r.StaleReason, r.CheckedSHA, r.CheckedAt)
	return err
}

// GetFact returns the row for id, or ErrNotFound.
func (s *Store) GetFact(id string) (FactRow, error) {
	r, err := scanFact(s.db.QueryRow(`SELECT `+factCols+` FROM facts_index WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return FactRow{}, ErrNotFound
	}
	return r, err
}

// ListFacts returns every row ordered by id.
func (s *Store) ListFacts() ([]FactRow, error) {
	rows, err := s.db.Query(`SELECT ` + factCols + ` FROM facts_index ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FactRow
	for rows.Next() {
		r, err := scanFact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteFact removes the row for id (no error when absent).
func (s *Store) DeleteFact(id string) error {
	_, err := s.db.Exec(`DELETE FROM facts_index WHERE id=?`, id)
	return err
}

// SetFactCheck records a staleness check result against integration commit sha.
func (s *Store) SetFactCheck(id string, stale bool, reason, sha string, at int64) error {
	st := 0
	if stale {
		st = 1
	}
	_, err := s.db.Exec(`UPDATE facts_index SET stale=?, stale_reason=?, checked_sha=?, checked_at=? WHERE id=?`, st, reason, sha, at, id)
	return err
}

// TouchFacts bumps the usage counters of ids (facts delivered in a pack).
func (s *Store) TouchFacts(ids []string, at int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.Exec(`UPDATE facts_index SET uses=uses+1, last_used_at=? WHERE id=?`, at, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
