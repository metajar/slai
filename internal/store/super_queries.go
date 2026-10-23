package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

// GetKV returns the value for key; ok is false when unset.
func (s *Store) GetKV(key string) (val string, ok bool, err error) {
	err = s.db.QueryRow(`SELECT value FROM kv WHERE key=?`, key).Scan(&val)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return val, err == nil, err
}

// SetKV upserts a key.
func (s *Store) SetKV(key, val string) error {
	_, err := s.db.Exec(`INSERT INTO kv(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, val)
	return err
}

// DeleteKV removes a key (no error if missing).
func (s *Store) DeleteKV(key string) error {
	_, err := s.db.Exec(`DELETE FROM kv WHERE key=?`, key)
	return err
}

// SetAgentPaused sets the per-agent paused flag.
func (s *Store) SetAgentPaused(id string, paused bool) error {
	v := 0
	if paused {
		v = 1
	}
	res, err := s.db.Exec(`UPDATE agents SET paused=? WHERE id=?`, v, id)
	return affected(res, err)
}

// PausedAgents returns the set of paused agent ids.
func (s *Store) PausedAgents() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT id FROM agents WHERE paused=1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// CompareAndSetStatus sets status to `to` only if the current status is one of from.
func (s *Store) CompareAndSetStatus(id, to string, from ...string) (bool, error) {
	if len(from) == 0 {
		return false, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(from)), ",")
	args := []any{to, s.ts(), id}
	for _, f := range from {
		args = append(args, f)
	}
	res, err := s.db.Exec(`UPDATE agents SET status=?, status_at=? WHERE id=? AND status IN (`+ph+`)`, args...)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetAgentStatusKeepAge changes status without refreshing status_at, so the
// fallback detector keeps treating hook data as stale.
func (s *Store) SetAgentStatusKeepAge(id, status string) error {
	res, err := s.db.Exec(`UPDATE agents SET status=? WHERE id=?`, status, id)
	return affected(res, err)
}

// PendingWake returns undelivered messages never woken, or last woken before wokenBefore (unix).
func (s *Store) PendingWake(wokenBefore int64) ([]Message, error) {
	rows, err := s.db.Query(`SELECT id, from_agent, to_agent, kind, task_id, reply_to, body, created_at, delivered_at, woken_at
FROM messages WHERE delivered_at=0 AND (woken_at=0 OR woken_at<?) ORDER BY id`, wokenBefore)
	if err != nil {
		return nil, err
	}
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

// EventsAfter returns events of kind (any kind if "") with id > afterID, oldest first.
func (s *Store) EventsAfter(afterID int64, kind string, limit int) ([]Event, error) {
	q := `SELECT id, at, agent, kind, data FROM events WHERE id>?`
	args := []any{afterID}
	if kind != "" {
		q += ` AND kind=?`
		args = append(args, kind)
	}
	q += ` ORDER BY id LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
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

// PairCount is the number of messages exchanged (either direction) between A and B (A < B).
type PairCount struct {
	A, B  string
	Count int
}

// MessagePairCounts counts messages created at/after since between pairs of
// distinct non-empty agents, ignoring the given pseudo-senders.
func (s *Store) MessagePairCounts(since int64, ignore ...string) ([]PairCount, error) {
	rows, err := s.db.Query(`SELECT MIN(from_agent,to_agent), MAX(from_agent,to_agent), COUNT(*) FROM messages
WHERE created_at>=? AND from_agent!=to_agent AND from_agent!='' AND to_agent!=''
AND task_id='' AND kind IN ('note','ask','reply')
AND (SELECT COUNT(DISTINCT m2.to_agent) FROM messages m2 WHERE m2.from_agent=messages.from_agent AND m2.body=messages.body AND m2.created_at=messages.created_at)<3
GROUP BY 1,2`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	skip := map[string]bool{}
	for _, i := range ignore {
		skip[i] = true
	}
	var out []PairCount
	for rows.Next() {
		var p PairCount
		if err := rows.Scan(&p.A, &p.B, &p.Count); err != nil {
			return nil, err
		}
		if !skip[p.A] && !skip[p.B] {
			out = append(out, p)
		}
	}
	return out, rows.Err()
}

func (s *Store) dayStart() int64 {
	n := s.now()
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, n.Location()).Unix()
}

// AgentSpendToday sums one agent's cost since local midnight.
func (s *Store) AgentSpendToday(agent string) (float64, error) {
	var v float64
	err := s.db.QueryRow(`SELECT COALESCE(SUM(cost_usd),0) FROM usage WHERE agent=? AND at>=?`, agent, s.dayStart()).Scan(&v)
	return v, err
}

// SpendByAgentToday returns today's cost per agent.
func (s *Store) SpendByAgentToday() (map[string]float64, error) {
	rows, err := s.db.Query(`SELECT agent, SUM(cost_usd) FROM usage WHERE at>=? GROUP BY agent`, s.dayStart())
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

// Today returns the local date string (YYYY-MM-DD) per the store clock.
func (s *Store) Today() string { return s.now().Format("2006-01-02") }

// SetAgentTask records the agent's current task and restarts its turn counter
// (the max-turns guard counts turns per task).
func (s *Store) SetAgentTask(id, taskID string) error {
	_, err := s.db.Exec(`UPDATE agents SET current_task = ?, turns = 0 WHERE id = ?`, taskID, id)
	return err
}

// ChargeAgentTo points the agent's usage at taskID ("" stops it) without touching its turn
// counter, unlike SetAgentTask. A reviewer uses it so its review tokens count toward the task.
func (s *Store) ChargeAgentTo(id, taskID string) error {
	_, err := s.db.Exec(`UPDATE agents SET current_task = ? WHERE id = ?`, taskID, id)
	return err
}

// KVReviewEndCharge + agent holds the task whose review charge approve/request_changes
// ended. The charge is cleared only after the turn's usage row is written (Stop hook, or the
// supervisor's idle fallback), so the review turn's tokens still carry the task.
const KVReviewEndCharge = "review.endcharge."

// FinishReviewCharge applies a pending end-of-review: it clears the agent's current task when
// it still equals the task recorded under KVReviewEndCharge, and removes the key. It reports
// whether a key was pending.
func (s *Store) FinishReviewCharge(agent string) bool {
	task, ok, _ := s.GetKV(KVReviewEndCharge + agent)
	if !ok || task == "" {
		return false
	}
	if s.AgentTask(agent) == task {
		_ = s.ChargeAgentTo(agent, "")
	}
	_ = s.DeleteKV(KVReviewEndCharge + agent)
	return true
}

// KVInboxHold + message id marks a message get_inbox must not hand out yet: an assignment
// whose worktree switch failed (the agent would start it on the wrong branch). The supervisor
// retries the switch every tick and releases the hold when it succeeds.
const KVInboxHold = "inbox.hold."

// HoldInbox keeps message id out of its recipient's get_inbox until ReleaseInbox.
func (s *Store) HoldInbox(id int64) error { return s.SetKV(KVInboxHold+strconv.FormatInt(id, 10), "1") }

// ReleaseInbox lets a held message through again.
func (s *Store) ReleaseInbox(id int64) error {
	return s.DeleteKV(KVInboxHold + strconv.FormatInt(id, 10))
}

// InboxHeld reports whether message id is held back.
func (s *Store) InboxHeld(id int64) bool {
	_, ok, _ := s.GetKV(KVInboxHold + strconv.FormatInt(id, 10))
	return ok
}

// AgentTask returns the agent's current task id ("" when none or no such agent).
func (s *Store) AgentTask(id string) string {
	var t string
	_ = s.db.QueryRow(`SELECT current_task FROM agents WHERE id = ?`, id).Scan(&t)
	return t
}

// FocusWindow is how long a TUI focus heartbeat keeps wake-ups away from an agent.
const FocusWindow = 5 * time.Second

// FocusKey is the kv key the TUI refreshes while the user types into agent id's pane.
func FocusKey(id string) string { return "focus:" + id }

// TouchFocus records "the user is typing into this agent right now".
func (s *Store) TouchFocus(id string, now time.Time) error {
	return s.SetKV(FocusKey(id), strconv.FormatInt(now.Unix(), 10))
}

// ClearFocus removes the focus heartbeat.
func (s *Store) ClearFocus(id string) error { return s.DeleteKV(FocusKey(id)) }

// Focused reports whether the user focused agent id within FocusWindow of now.
func (s *Store) Focused(id string, now time.Time) bool {
	v, ok, err := s.GetKV(FocusKey(id))
	if err != nil || !ok {
		return false
	}
	at, err := strconv.ParseInt(v, 10, 64)
	return err == nil && now.Sub(time.Unix(at, 0)) < FocusWindow
}

// workingRank orders the statuses that count as "being on a task".
var workingRank = map[string]int{StatusInProgress: 0, StatusChangesRequested: 1, StatusInReview: 2, StatusAssigned: 3}

// WorkingTask is the task the agent is actually on: among its tasks, in_progress,
// then changes_requested, then in_review, then assigned (never blocked, merged,
// done or cancelled), unless the agent's stored current_task (see SetAgentTask) is one
// of those, which wins; ties go to the most recently updated. A task the agent opened
// with get_task more recently than that pick was updated, if it is in one of those
// statuses and the agent's own, wins. "" when nothing qualifies.
func (s *Store) WorkingTask(agent string) (string, error) {
	rows, err := s.db.Query(`SELECT id, status, updated_at FROM tasks WHERE assignee=? AND status IN (?,?,?,?)`,
		agent, StatusInProgress, StatusChangesRequested, StatusInReview, StatusAssigned)
	if err != nil {
		return "", err
	}
	type cand struct {
		id   string
		rank int
		at   int64
	}
	var best *cand
	have := map[string]bool{}
	for rows.Next() {
		var c cand
		var st string
		if err := rows.Scan(&c.id, &st, &c.at); err != nil {
			rows.Close()
			return "", err
		}
		c.rank = workingRank[st]
		have[c.id] = true
		if best == nil || c.rank < best.rank ||
			(c.rank == best.rank && (c.at > best.at || (c.at == best.at && c.id > best.id))) {
			cp := c
			best = &cp
		}
	}
	rows.Close()
	if best == nil {
		return "", nil
	}
	// The task slai put the agent on (current_task = the branch checked out in an
	// engineer's worktree) wins while it is one of the agent's live tasks: a queued
	// task that became startable behind it must not take the board over early.
	var stored string
	if s.db.QueryRow(`SELECT current_task FROM agents WHERE id=?`, agent).Scan(&stored) == nil && have[stored] {
		return stored, nil
	}
	evs, err := s.EventsByKind(agent, "get_task", 10)
	if err != nil {
		return best.id, nil
	}
	for _, e := range evs { // newest first
		var d struct {
			TaskID string `json:"task_id"`
		}
		if json.Unmarshal([]byte(e.Data), &d) != nil || !have[d.TaskID] {
			continue
		}
		if e.At > best.at {
			return d.TaskID, nil
		}
		break // the newest qualifying get_task is no newer than the pick
	}
	return best.id, nil
}
