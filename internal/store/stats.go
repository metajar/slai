package store

import "time"

// TaskEventStats counts, for one task, the events the quality stats are built from.
type TaskEventStats struct {
	Reviews  int   // request_review calls
	Changes  int   // request_changes calls
	Refused  int   // merge_refused events (test gate, moved SHA, ...)
	MergedAt int64 // time of the newest "merged" event; 0 when there is none
}

// SetClock replaces the clock used for timestamps (tests).
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// TaskEventStats counts the review and merge events of every task, keyed by task id.
// Read-only.
func (s *Store) TaskEventStats() (map[string]*TaskEventStats, error) {
	rows, err := s.db.Query(`SELECT json_extract(data,'$.task_id'), kind, COUNT(*), MAX(at) FROM events
 WHERE kind IN ('request_review','request_changes','merge_refused','merged') AND json_valid(data)
 AND json_extract(data,'$.task_id') IS NOT NULL GROUP BY 1, 2`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*TaskEventStats{}
	for rows.Next() {
		var id, kind string
		var n int
		var at int64
		if err := rows.Scan(&id, &kind, &n, &at); err != nil {
			return nil, err
		}
		e := out[id]
		if e == nil {
			e = &TaskEventStats{}
			out[id] = e
		}
		switch kind {
		case "request_review":
			e.Reviews = n
		case "request_changes":
			e.Changes = n
		case "merge_refused":
			e.Refused = n
		case "merged":
			e.MergedAt = at
		}
	}
	return out, rows.Err()
}

// UsageByAgent sums recorded spend per agent from the given unix time (0 = all time).
// Read-only.
func (s *Store) UsageByAgent(since int64) (map[string]float64, error) {
	rows, err := s.db.Query(`SELECT agent, COALESCE(SUM(cost_usd),0) FROM usage WHERE at>=? GROUP BY agent`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]float64{}
	for rows.Next() {
		var a string
		var v float64
		if err := rows.Scan(&a, &v); err != nil {
			return nil, err
		}
		out[a] = v
	}
	return out, rows.Err()
}
