package store

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// Escalation is one open, user- or orchestrator-facing problem about an agent
// (KV "esc.<kind>.<agent>", JSON value). It exists while the problem does:
// RaiseEscalation is idempotent, which is what makes escalations fire once.
type Escalation struct {
	Agent string `json:"agent"`
	Kind  string `json:"kind"`
	Text  string `json:"text"`
	Since int64  `json:"since"`
	User  bool   `json:"user"` // the user has been told (needs a TUI banner)
}

const escPrefix = "esc."

func escKey(kind, agent string) string { return escPrefix + kind + "." + agent }

// RaiseEscalation opens e and reports raised=true, or returns false without
// touching the stored one if the same agent+kind is already open. Since
// defaults to now.
func (s *Store) RaiseEscalation(e Escalation) (raised bool, err error) {
	if e.Since == 0 {
		e.Since = s.now().Unix()
	}
	b, err := json.Marshal(e)
	if err != nil {
		return false, err
	}
	// INSERT OR IGNORE keeps the check-and-set atomic across processes.
	res, err := s.db.Exec(`INSERT OR IGNORE INTO kv(key, value) VALUES(?, ?)`, escKey(e.Kind, e.Agent), string(b))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ClearEscalation closes the escalation so it can fire again.
func (s *Store) ClearEscalation(agent, kind string) error {
	return s.DeleteKV(escKey(kind, agent))
}

// OpenEscalations lists every open escalation, oldest first.
func (s *Store) OpenEscalations() ([]Escalation, error) {
	rows, err := s.db.Query(`SELECT value FROM kv WHERE key LIKE 'esc.%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Escalation
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		var e Escalation
		if json.Unmarshal([]byte(v), &e) == nil {
			out = append(out, e)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Since != out[j].Since {
			return out[i].Since < out[j].Since
		}
		return out[i].Kind+out[i].Agent < out[j].Kind+out[j].Agent
	})
	return out, nil
}

const heartbeatKey = "supervisor.heartbeat"

func (s *Store) SetSupervisorHeartbeat(unix int64) error {
	return s.SetKV(heartbeatKey, strconv.FormatInt(unix, 10))
}

// SupervisorHeartbeat is the supervisor's last tick time; ok=false if it has never ticked.
func (s *Store) SupervisorHeartbeat() (unix int64, ok bool, err error) {
	v, ok, err := s.GetKV(heartbeatKey)
	if err != nil || !ok {
		return 0, false, err
	}
	n, perr := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if perr != nil {
		return 0, false, nil
	}
	return n, true, nil
}

// UnreadCount is how many messages to agent have not been claimed yet.
func (s *Store) UnreadCount(agent string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE to_agent=? AND delivered_at=0`, agent).Scan(&n)
	return n, err
}
