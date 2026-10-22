package store

import (
	"database/sql"
	"errors"
)

// AgentByID returns one agent or ErrNotFound.
func (s *Store) AgentByID(id string) (Agent, error) {
	var a Agent
	err := s.db.QueryRow(`SELECT id, role, model, status, status_at, tmux_session, worktree, current_task, turns FROM agents WHERE id=?`, id).
		Scan(&a.ID, &a.Role, &a.Model, &a.Status, &a.StatusAt, &a.TmuxSession, &a.Worktree, &a.CurrentTask, &a.Turns)
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, ErrNotFound
	}
	return a, err
}

// BumpTurns increments agents.turns and returns the new value.
func (s *Store) BumpTurns(id string) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE agents SET turns=turns+1 WHERE id=?`, id)
	if err := affected(res, err); err != nil {
		return 0, err
	}
	var n int
	if err := tx.QueryRow(`SELECT turns FROM agents WHERE id=?`, id).Scan(&n); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// EventsByKind returns an agent's newest events of the given kind first.
func (s *Store) EventsByKind(agent, kind string, limit int) ([]Event, error) {
	rows, err := s.db.Query(`SELECT id, at, agent, kind, data FROM events WHERE agent=? AND kind=? ORDER BY id DESC LIMIT ?`, agent, kind, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.At, &e.Agent, &e.Kind, &e.Data); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// LastHookEventAt is the unix time of agent's latest hook event (0 if none).
func (s *Store) LastHookEventAt(agent string) (int64, error) {
	var at int64
	err := s.db.QueryRow(`SELECT COALESCE(MAX(at),0) FROM events WHERE agent=? AND kind LIKE 'hook:%'`, agent).Scan(&at)
	return at, err
}

// LaneDenialCounts returns, per agent, how many lane_denied events happened at or
// after since (unix seconds).
func (s *Store) LaneDenialCounts(since int64) (map[string]int, error) {
	rows, err := s.db.Query(`SELECT agent, COUNT(*) FROM events WHERE kind='lane_denied' AND at>=? GROUP BY agent`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var a string
		var n int
		if err := rows.Scan(&a, &n); err != nil {
			return nil, err
		}
		out[a] = n
	}
	return out, rows.Err()
}

// GetEvent returns one event by id (ErrNotFound if there is none).
func (s *Store) GetEvent(id int64) (Event, error) {
	var e Event
	err := s.db.QueryRow(`SELECT id, at, agent, kind, data FROM events WHERE id=?`, id).Scan(&e.ID, &e.At, &e.Agent, &e.Kind, &e.Data)
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	return e, err
}
