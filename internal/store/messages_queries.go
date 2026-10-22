package store

import "database/sql"

const messageCols = `id, from_agent, to_agent, kind, task_id, reply_to, body, created_at, delivered_at, woken_at`

func scanMessages(rows *sql.Rows) ([]Message, error) {
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.From, &m.To, &m.Kind, &m.TaskID, &m.ReplyTo, &m.Body, &m.CreatedAt, &m.DeliveredAt, &m.WokenAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MessagesForTask returns every message attached to the task, oldest first.
// Replies to those messages (which carry no task_id of their own) are included.
func (s *Store) MessagesForTask(taskID string) ([]Message, error) {
	rows, err := s.db.Query(`SELECT `+messageCols+` FROM messages
WHERE task_id=? OR reply_to IN (SELECT id FROM messages WHERE task_id=?) ORDER BY id`, taskID, taskID)
	if err != nil {
		return nil, err
	}
	return scanMessages(rows)
}

// RecentMessages returns the newest messages first.
func (s *Store) RecentMessages(limit int) ([]Message, error) {
	rows, err := s.db.Query(`SELECT `+messageCols+` FROM messages ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	return scanMessages(rows)
}

// OpenAsks lists `ask` messages created at or after since that nobody has replied to yet,
// oldest first.
func (s *Store) OpenAsks(since int64) ([]Message, error) {
	rows, err := s.db.Query(`SELECT `+messageCols+` FROM messages m WHERE kind='ask' AND created_at>=?
 AND NOT EXISTS (SELECT 1 FROM messages r WHERE r.reply_to=m.id) ORDER BY id`, since)
	if err != nil {
		return nil, err
	}
	return scanMessages(rows)
}
