package store

import (
	"database/sql"
	"errors"
)

// GetMessage returns one message by id (ErrNotFound if missing).
func (s *Store) GetMessage(id int64) (Message, error) {
	var m Message
	err := s.db.QueryRow(`SELECT id, from_agent, to_agent, kind, task_id, reply_to, body, created_at, delivered_at, woken_at
FROM messages WHERE id=?`, id).Scan(&m.ID, &m.From, &m.To, &m.Kind, &m.TaskID, &m.ReplyTo, &m.Body, &m.CreatedAt, &m.DeliveredAt, &m.WokenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, ErrNotFound
	}
	return m, err
}

// TaskEvents returns events whose JSON data has task_id == taskID, oldest first
// (at most the newest limit rows).
func (s *Store) TaskEvents(taskID string, limit int) ([]Event, error) {
	rows, err := s.db.Query(`SELECT id, at, agent, kind, data FROM (
 SELECT id, at, agent, kind, data FROM events WHERE json_valid(data) AND json_extract(data,'$.task_id')=? ORDER BY id DESC LIMIT ?
) ORDER BY id`, taskID, limit)
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
