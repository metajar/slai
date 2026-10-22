package store

import (
	"errors"
	"strings"
)

// ACClaim is the engineer's statement about one acceptance criterion of a spec'd
// task, made with request_review.
type ACClaim struct {
	AC   string `json:"ac"`
	Met  bool   `json:"met"`
	Note string `json:"note,omitempty"`
}

// ACVerdict is the reviewer's answer for one criterion, made with approve or
// request_changes.
type ACVerdict struct {
	AC   string `json:"ac"`
	Pass bool   `json:"pass"`
	Note string `json:"note,omitempty"`
	By   string `json:"by,omitempty"`
	At   int64  `json:"at,omitempty"`
}

// ACKey normalises a criterion label for matching a claim to a verdict: case and
// surrounding space do not matter ("AC 1", "ac 1" and " AC 1 " are one criterion).
func ACKey(ac string) string { return strings.ToLower(strings.Join(strings.Fields(ac), " ")) }

// SetACClaims records the claims of a new review request and returns its number
// (1 for the first request of the task, then 2, ...). Claims of earlier requests
// are kept as history.
func (s *Store) SetACClaims(taskID string, claims []ACClaim) (int, error) {
	if len(claims) == 0 {
		return 0, errors.New("no claims")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(review_no),0) FROM task_ac_claims WHERE task_id=?`, taskID).Scan(&n); err != nil {
		return 0, err
	}
	n++
	for _, c := range claims {
		met := 0
		if c.Met {
			met = 1
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO task_ac_claims(task_id, review_no, ac, met, note) VALUES(?,?,?,?,?)`,
			taskID, n, strings.TrimSpace(c.AC), met, c.Note); err != nil {
			return 0, err
		}
	}
	return n, tx.Commit()
}

// LatestACClaims returns the claims of the task's latest review request, in the
// order they were made, with that request's number (0, nil when there are none).
func (s *Store) LatestACClaims(taskID string) (int, []ACClaim, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(review_no),0) FROM task_ac_claims WHERE task_id=?`, taskID).Scan(&n); err != nil || n == 0 {
		return 0, nil, err
	}
	rows, err := s.db.Query(`SELECT ac, met, note FROM task_ac_claims WHERE task_id=? AND review_no=? ORDER BY rowid`, taskID, n)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	var out []ACClaim
	for rows.Next() {
		var c ACClaim
		var met int
		if err := rows.Scan(&c.AC, &met, &c.Note); err != nil {
			return 0, nil, err
		}
		c.Met = met != 0
		out = append(out, c)
	}
	return n, out, rows.Err()
}

// SaveACVerdicts records the reviewer's verdicts against the task's latest review
// request (a later verdict for the same criterion replaces an earlier one).
func (s *Store) SaveACVerdicts(taskID, by string, verdicts []ACVerdict) error {
	n, _, err := s.LatestACClaims(taskID)
	if err != nil {
		return err
	}
	if n == 0 {
		n = 1 // verdicts without claims (a legacy request) still get stored
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, v := range verdicts {
		pass := 0
		if v.Pass {
			pass = 1
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO task_ac_verdicts(task_id, review_no, ac, pass, note, by, at) VALUES(?,?,?,?,?,?,?)`,
			taskID, n, strings.TrimSpace(v.AC), pass, v.Note, by, s.ts()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LatestACVerdicts returns the verdicts given for the task's latest review request.
func (s *Store) LatestACVerdicts(taskID string) ([]ACVerdict, error) {
	n, _, err := s.LatestACClaims(taskID)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		n = 1
	}
	rows, err := s.db.Query(`SELECT ac, pass, note, by, at FROM task_ac_verdicts WHERE task_id=? AND review_no=? ORDER BY rowid`, taskID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ACVerdict
	for rows.Next() {
		var v ACVerdict
		var pass int
		if err := rows.Scan(&v.AC, &pass, &v.Note, &v.By, &v.At); err != nil {
			return nil, err
		}
		v.Pass = pass != 0
		out = append(out, v)
	}
	return out, rows.Err()
}

// UnansweredClaims lists the claims (of the latest request) that have no verdict yet.
func (s *Store) UnansweredClaims(taskID string) ([]string, error) {
	_, claims, err := s.LatestACClaims(taskID)
	if err != nil {
		return nil, err
	}
	vs, err := s.LatestACVerdicts(taskID)
	if err != nil {
		return nil, err
	}
	answered := map[string]bool{}
	for _, v := range vs {
		answered[ACKey(v.AC)] = true
	}
	var out []string
	for _, c := range claims {
		if !answered[ACKey(c.AC)] {
			out = append(out, c.AC)
		}
	}
	return out, nil
}
