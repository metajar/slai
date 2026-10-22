package store

import (
	"strings"
)

// EventFilter selects events for the web log. Zero fields match everything.
type EventFilter struct {
	Agent    string // events by this agent
	HasAgent bool   // Agent is set ("" then means the supervisor, whose events have no agent)
	Kind     string
	Task     string // data.task_id (or data.task) equals this
	Text     string // the event's data contains this (case-insensitive)
	Noise    bool   // include hook:* and usage:* events (hidden by default, as in the TUI)
	Limit    int
	Offset   int
}

// likeEscape escapes LIKE wildcards so user text matches literally (with ESCAPE '\').
func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func (f EventFilter) where() (string, []any) {
	var cond []string
	var args []any
	if f.HasAgent {
		cond = append(cond, "agent=?")
		args = append(args, f.Agent)
	}
	if f.Kind != "" {
		cond = append(cond, "kind=?")
		args = append(args, f.Kind)
	}
	if f.Task != "" {
		cond = append(cond, "(json_valid(data) AND (json_extract(data,'$.task_id')=? OR json_extract(data,'$.task')=?))")
		args = append(args, f.Task, f.Task)
	}
	if f.Text != "" {
		cond = append(cond, `(data LIKE ? ESCAPE '\' OR kind LIKE ? ESCAPE '\' OR agent LIKE ? ESCAPE '\')`)
		p := "%" + likeEscape(f.Text) + "%"
		args = append(args, p, p, p)
	}
	if !f.Noise {
		cond = append(cond, `kind NOT LIKE 'hook:%' AND kind NOT LIKE 'usage:%'`)
	}
	if len(cond) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(cond, " AND "), args
}

// QueryEvents returns the matching events newest first (Limit/Offset page through them)
// and the total number that match. Read-only.
func (s *Store) QueryEvents(f EventFilter) ([]Event, int, error) {
	where, args := f.where()
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM events`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id, at, agent, kind, data FROM events`+where+` ORDER BY id DESC LIMIT ? OFFSET ?`,
		append(append([]any{}, args...), limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.At, &e.Agent, &e.Kind, &e.Data); err != nil {
			return nil, 0, err
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}

// EventAgents and EventKinds list the distinct values present in the log, for the web
// filter menus. Agent "" (the supervisor) is included.
func (s *Store) EventAgents() ([]string, error) { return s.distinct("agent") }
func (s *Store) EventKinds() ([]string, error)  { return s.distinct("kind") }

func (s *Store) distinct(col string) ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT ` + col + ` FROM events ORDER BY ` + col + ` LIMIT 500`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
