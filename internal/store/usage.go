package store

// TaskSpend is the spend attributed to one open task and who owns it.
type TaskSpend struct {
	Assignee string
	USD      float64
}

// OpenTaskSpend sums the spend attributed to each task that is not finished (not merged,
// cancelled, done or reverted), summed across every agent that worked on it. Tasks with
// no attributed spend are left out. Read-only.
func (s *Store) OpenTaskSpend() (map[string]TaskSpend, error) {
	rows, err := s.db.Query(`SELECT t.id, t.assignee, SUM(u.cost_usd) FROM usage u JOIN tasks t ON t.id=u.task_id
 WHERE t.status NOT IN (?,?,?,?) GROUP BY t.id, t.assignee`,
		StatusMerged, StatusCancelled, StatusDone, StatusReverted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]TaskSpend{}
	for rows.Next() {
		var id string
		var v TaskSpend
		if err := rows.Scan(&id, &v.Assignee, &v.USD); err != nil {
			return nil, err
		}
		out[id] = v
	}
	return out, rows.Err()
}

// SpendSince sums all recorded spend from the given unix time. Read-only.
func (s *Store) SpendSince(since int64) (float64, error) {
	var v float64
	err := s.db.QueryRow(`SELECT COALESCE(SUM(cost_usd),0) FROM usage WHERE at>=?`, since).Scan(&v)
	return v, err
}

// TaskUsage is the usage recorded against one task.
type TaskUsage struct {
	CostUSD float64
	Tokens  int64 // input + output
	Rows    int   // usage records (0 = the harness reported nothing)
}

// UsageByTask sums usage per task from the given unix time (0 = all time); tasks with no
// usage row are absent. Usage with no task is not included. Read-only.
func (s *Store) UsageByTask(since int64) (map[string]TaskUsage, error) {
	rows, err := s.db.Query(`SELECT task_id, COALESCE(SUM(cost_usd),0), COALESCE(SUM(input_tokens+output_tokens),0), COUNT(*)
 FROM usage WHERE at>=? AND task_id<>'' GROUP BY task_id`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]TaskUsage{}
	for rows.Next() {
		var id string
		var u TaskUsage
		if err := rows.Scan(&id, &u.CostUSD, &u.Tokens, &u.Rows); err != nil {
			return nil, err
		}
		out[id] = u
	}
	return out, rows.Err()
}
