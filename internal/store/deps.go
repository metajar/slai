package store

import (
	"database/sql"
	"fmt"
)

// DepSatisfied reports whether a dependency no longer holds its dependents back.
func DepSatisfied(status string) bool {
	return status == StatusMerged || status == StatusCancelled || status == StatusReverted // reverted: the work was withdrawn, like cancelled
}

// SetTaskDeps replaces the dependencies of task id. Ids must exist, may not be
// the task itself, and may not make a cycle.
func (s *Store) SetTaskDeps(id string, deps []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	seen := map[string]bool{}
	var clean []string
	for _, d := range deps {
		if d == id {
			return fmt.Errorf("task %s cannot depend on itself", id)
		}
		if seen[d] {
			continue
		}
		seen[d] = true
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM tasks WHERE id=?`, d).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("depends_on %s: %w", d, ErrNotFound)
		}
		clean = append(clean, d)
	}
	// A cycle exists if id is reachable from any dependency.
	for _, d := range clean {
		if reach, err := reachable(tx.Query, d, id); err != nil {
			return err
		} else if reach {
			return fmt.Errorf("depends_on %s would make a cycle with %s", d, id)
		}
	}
	if _, err := tx.Exec(`DELETE FROM task_deps WHERE task_id=?`, id); err != nil {
		return err
	}
	for _, d := range clean {
		if _, err := tx.Exec(`INSERT INTO task_deps(task_id, dep_id) VALUES(?,?)`, id, d); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// reachable reports whether target is among the (transitive) dependencies of from, from included.
func reachable(query func(string, ...any) (*sql.Rows, error), from, target string) (bool, error) {
	seen := map[string]bool{}
	stack := []string{from}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == target {
			return true, nil
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		rows, err := query(`SELECT dep_id FROM task_deps WHERE task_id=?`, cur)
		if err != nil {
			return false, err
		}
		for rows.Next() {
			var d string
			if err := rows.Scan(&d); err != nil {
				rows.Close()
				return false, err
			}
			stack = append(stack, d)
		}
		rows.Close()
	}
	return false, nil
}

// allDeps returns task id -> sorted dependency ids for every task with deps.
func (s *Store) allDeps() (map[string][]string, error) {
	rows, err := s.db.Query(`SELECT task_id, dep_id FROM task_deps ORDER BY task_id, dep_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var t, d string
		if err := rows.Scan(&t, &d); err != nil {
			return nil, err
		}
		out[t] = append(out[t], d)
	}
	return out, rows.Err()
}

// TaskDeps lists the dependency ids of a task.
func (s *Store) TaskDeps(id string) ([]string, error) {
	rows, err := s.db.Query(`SELECT dep_id FROM task_deps WHERE task_id=? ORDER BY dep_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// WaitingOn lists the task's dependencies that are not yet merged or cancelled.
func (s *Store) WaitingOn(id string) ([]string, error) {
	rows, err := s.db.Query(`SELECT d.dep_id FROM task_deps d JOIN tasks t ON t.id=d.dep_id
WHERE d.task_id=? AND t.status NOT IN (?,?,?) ORDER BY d.dep_id`, id, StatusMerged, StatusCancelled, StatusReverted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) hydrateDeps(ts []Task) error {
	m, err := s.allDeps()
	if err != nil {
		return err
	}
	for i := range ts {
		ts[i].DependsOn = m[ts[i].ID]
	}
	return nil
}

// releaseDependents runs inside the status transaction that made dep merged or
// cancelled. Each blocked/assigned task whose dependencies are now all
// satisfied is moved to in_progress (if it was blocked) and its assignee gets
// one note. Because the dependency's terminal status can be set only once, and
// only the last dependency to finish finds the task fully satisfied, this fires
// exactly once per dependent.
func (s *Store) releaseDependents(tx *sql.Tx, dep, to string, notes map[string]string) error {
	rows, err := tx.Query(`SELECT t.id, t.assignee, t.status FROM task_deps d JOIN tasks t ON t.id=d.task_id
WHERE d.dep_id=? AND t.status IN (?,?) ORDER BY t.id`, dep, StatusBlocked, StatusAssigned)
	if err != nil {
		return err
	}
	type dependent struct{ id, assignee, status string }
	var ds []dependent
	for rows.Next() {
		var d dependent
		if err := rows.Scan(&d.id, &d.assignee, &d.status); err != nil {
			rows.Close()
			return err
		}
		ds = append(ds, d)
	}
	rows.Close()
	for _, d := range ds {
		var open int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM task_deps x JOIN tasks t ON t.id=x.dep_id
WHERE x.task_id=? AND t.status NOT IN (?,?,?)`, d.id, StatusMerged, StatusCancelled, StatusReverted).Scan(&open); err != nil {
			return err
		}
		if open > 0 {
			continue
		}
		if d.status == StatusBlocked {
			if _, err := tx.Exec(`UPDATE tasks SET status=?, updated_at=? WHERE id=?`, StatusInProgress, s.ts(), d.id); err != nil {
				return err
			}
		}
		if d.assignee == "" {
			continue
		}
		advice := "First bring your branch up to date with slai/integration (git rebase slai/integration) so you build on what just merged."
		if n := notes[d.id]; n != "" {
			advice = n
		}
		body := fmt.Sprintf("[%s] dependency %s %s; you can proceed. %s", d.id, dep, to, advice)
		if _, err := tx.Exec(`INSERT INTO messages(from_agent, to_agent, kind, task_id, reply_to, body, created_at) VALUES(?,?,?,?,?,?,?)`,
			"slai", d.assignee, "note", d.id, 0, body, s.ts()); err != nil {
			return err
		}
	}
	return nil
}

// Dependent is a task held back by a dependency.
type Dependent struct {
	ID, Assignee, Status, Branch string
}

// PendingReleases lists the tasks that the merge (or cancellation) of dep would release:
// held (assigned or blocked) with every other dependency already satisfied.
func (s *Store) PendingReleases(dep string) ([]Dependent, error) {
	rows, err := s.db.Query(`SELECT t.id, t.assignee, t.status, t.branch FROM task_deps d JOIN tasks t ON t.id=d.task_id
WHERE d.dep_id=? AND t.status IN (?,?) ORDER BY t.id`, dep, StatusBlocked, StatusAssigned)
	if err != nil {
		return nil, err
	}
	var all []Dependent
	for rows.Next() {
		var d Dependent
		if err := rows.Scan(&d.ID, &d.Assignee, &d.Status, &d.Branch); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, d)
	}
	rows.Close()
	var out []Dependent
	for _, d := range all {
		var open int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM task_deps x JOIN tasks t ON t.id=x.dep_id
WHERE x.task_id=? AND x.dep_id<>? AND t.status NOT IN (?,?,?)`, d.ID, dep, StatusMerged, StatusCancelled, StatusReverted).Scan(&open); err != nil {
			return nil, err
		}
		if open == 0 {
			out = append(out, d)
		}
	}
	return out, nil
}

// noteDependentsOfReview runs in the transaction that moved dep to in_review: every
// held dependent's assignee is told once which branch the work is on, so they can read
// it and prepare while it waits for review. The note is keyed on its text, so the task
// bouncing between review and changes_requested does not repeat it.
func (s *Store) noteDependentsOfReview(tx *sql.Tx, dep string) error {
	var depAssignee, depBranch string
	if err := tx.QueryRow(`SELECT assignee, branch FROM tasks WHERE id=?`, dep).Scan(&depAssignee, &depBranch); err != nil {
		return err
	}
	if depBranch == "" {
		depBranch = fmt.Sprintf("slai/%s/%s", depAssignee, dep)
	}
	rows, err := tx.Query(`SELECT t.id, t.assignee FROM task_deps d JOIN tasks t ON t.id=d.task_id
WHERE d.dep_id=? AND t.status IN (?,?) ORDER BY t.id`, dep, StatusBlocked, StatusAssigned)
	if err != nil {
		return err
	}
	type target struct{ id, assignee string }
	var ts []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.id, &t.assignee); err != nil {
			rows.Close()
			return err
		}
		ts = append(ts, t)
	}
	rows.Close()
	for _, t := range ts {
		if t.assignee == "" {
			continue
		}
		prefix := fmt.Sprintf("[%s] dependency %s is now in review", t.id, dep)
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM messages WHERE to_agent=? AND task_id=? AND from_agent='slai' AND body LIKE ?`,
			t.assignee, t.id, prefix+"%").Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		body := fmt.Sprintf("%s (branch %s, by %s). It is not merged yet, so keep waiting for the go-ahead note before you build on it; meanwhile you may read it (git diff slai/integration...%s) to prepare. Do not commit on top of it or push it.",
			prefix, depBranch, depAssignee, depBranch)
		if _, err := tx.Exec(`INSERT INTO messages(from_agent, to_agent, kind, task_id, reply_to, body, created_at) VALUES(?,?,?,?,?,?,?)`,
			"slai", t.assignee, "note", t.id, 0, body, s.ts()); err != nil {
			return err
		}
	}
	return nil
}
