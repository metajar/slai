package store

// TaskTokens sums the input and output tokens recorded against each task (usage rows carry the
// task their agent was working on; rows with no task are left out). Tasks with no recorded
// usage are absent. Read-only.
func (s *Store) TaskTokens() (map[string]int64, error) {
	rows, err := s.db.Query(`SELECT task_id, SUM(input_tokens + output_tokens) FROM usage WHERE task_id <> '' GROUP BY task_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var id string
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}
