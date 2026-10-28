// Package store is the shared SQLite state (WAL) used by every slai process.
package store

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql migrations/down/*.sql
var migrationsFS embed.FS

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("store: not found")

// ErrBadTransition is returned for illegal task status changes.
var ErrBadTransition = errors.New("store: illegal task transition")

type Store struct {
	db  *sql.DB
	now func() time.Time
}

type Agent struct {
	ID, Role, Model, Status string
	StatusAt                int64
	TmuxSession, Worktree   string
	CurrentTask             string
	Turns                   int
}

type Task struct {
	ID, ParentID, Title, Description string
	Assignee, CreatedBy, Status      string
	SpecPath, Branch                 string
	Files                            string // JSON array
	ReviewRounds                     int
	Priority                         int      // higher first; 0 is the default
	DependsOn                        []string // task ids that must merge (or be cancelled) first
	// ACClaims and ACVerdicts are the engineer's claims and the reviewer's verdicts for
	// the latest review request; filled by GetTask only.
	ACClaims             []ACClaim
	ACVerdicts           []ACVerdict
	CreatedAt, UpdatedAt int64
}

type Message struct {
	ID                     int64
	From, To, Kind, TaskID string
	ReplyTo                int64
	Body                   string
	CreatedAt, DeliveredAt int64
	WokenAt                int64
}

type Event struct {
	ID    int64
	At    int64
	Agent string
	Kind  string
	Data  string // JSON
}

// Open opens (creating if needed) the database at path and applies migrations.
func Open(path string) (*Store, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	s := &Store{db: db, now: time.Now}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	files, err := listMigrations(migrationsFS)
	if err != nil {
		return err
	}
	for _, f := range files {
		body, err := migrationsFS.ReadFile("migrations/" + f.name)
		if err != nil {
			return err
		}
		if err := s.applyMigration(f.ver, string(body)); err != nil {
			return fmt.Errorf("migration %s: %w", f.name, err)
		}
	}
	return nil
}

type migrationFile struct {
	ver  int
	name string
}

// listMigrations returns the migrations/*.sql files in version order. Two files
// sharing a version prefix are an error naming both: applyMigration keys on the
// version, so the second would otherwise be skipped silently.
func listMigrations(fsys fs.ReadDirFS) ([]migrationFile, error) {
	entries, err := fsys.ReadDir("migrations")
	if err != nil {
		return nil, err
	}
	var files []migrationFile
	byVer := map[int]string{}
	for _, e := range entries {
		if e.IsDir() { // migrations/down holds the reverse scripts
			continue
		}
		name := e.Name()
		ver, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil {
			return nil, fmt.Errorf("bad migration name %q", name)
		}
		if other, dup := byVer[ver]; dup {
			first, second := other, name
			if second < first {
				first, second = second, first
			}
			return nil, fmt.Errorf("duplicate migration version %04d: %s and %s (renumber one of them)", ver, first, second)
		}
		byVer[ver] = name
		files = append(files, migrationFile{ver, name})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].ver < files[j].ver })
	return files, nil
}

// MigrateDown reverts applied migrations newer than version to, newest first,
// using migrations/down/<same name>. It stops with an error at the first
// migration that has no down script (none of 0001/0002 do: reverting those would
// discard the task and agent tables), leaving everything above it reverted.
func (s *Store) MigrateDown(to int) error {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	byVer := map[int]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if ver, err := strconv.Atoi(strings.SplitN(e.Name(), "_", 2)[0]); err == nil {
			byVer[ver] = e.Name()
		}
	}
	for {
		cur, err := s.SchemaVersion()
		if err != nil {
			return err
		}
		if cur <= to {
			return nil
		}
		name, ok := byVer[cur]
		if !ok {
			return fmt.Errorf("no migration file for applied version %d", cur)
		}
		body, err := migrationsFS.ReadFile("migrations/down/" + name)
		if err != nil {
			return fmt.Errorf("migration %s has no down migration", name)
		}
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("down %s: %w", name, err)
		}
		if _, err := tx.Exec(`DELETE FROM schema_migrations WHERE version=?`, cur); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
}

func (s *Store) applyMigration(ver int, body string) error {
	tx, err := s.db.Begin() // immediate: serializes concurrent migrators
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=?`, ver).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	if _, err := tx.Exec(body); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES(?,?)`, ver, s.now().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ts() int64 { return s.now().Unix() }

// UpsertAgent inserts or updates an agent's static fields (status is kept on update).
func (s *Store) UpsertAgent(a Agent) error {
	if a.Status == "" {
		a.Status = "idle"
	}
	_, err := s.db.Exec(`INSERT INTO agents(id, role, model, status, status_at, tmux_session, worktree, current_task, turns)
VALUES(?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET role=excluded.role, model=excluded.model,
 tmux_session=excluded.tmux_session, worktree=excluded.worktree, current_task=COALESCE(NULLIF(excluded.current_task,''), agents.current_task)`,
		a.ID, a.Role, a.Model, a.Status, s.ts(), a.TmuxSession, a.Worktree, a.CurrentTask, a.Turns)
	return err
}

func (s *Store) SetAgentStatus(id, status string) error {
	res, err := s.db.Exec(`UPDATE agents SET status=?, status_at=? WHERE id=?`, status, s.ts(), id)
	return affected(res, err)
}

func affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetAgents lists the agents. CurrentTask is the task each is actually on
// (WorkingTask), not the stored most-recent assignment.
func (s *Store) GetAgents() ([]Agent, error) {
	rows, err := s.db.Query(`SELECT id, role, model, status, status_at, tmux_session, worktree, current_task, turns FROM agents ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	var out []Agent
	for rows.Next() {
		var a Agent
		if err := rows.Scan(&a.ID, &a.Role, &a.Model, &a.Status, &a.StatusAt, &a.TmuxSession, &a.Worktree, &a.CurrentTask, &a.Turns); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for i := range out {
		if t, err := s.WorkingTask(out[i].ID); err == nil {
			out[i].CurrentTask = t
		}
	}
	return out, nil
}

const taskCols = `id, parent_id, title, description, assignee, created_by, status, spec_path, branch, files, review_rounds, created_at, updated_at, priority`

type scanner interface{ Scan(...any) error }

func scanTask(r scanner) (Task, error) {
	var t Task
	err := r.Scan(&t.ID, &t.ParentID, &t.Title, &t.Description, &t.Assignee, &t.CreatedBy, &t.Status, &t.SpecPath, &t.Branch, &t.Files, &t.ReviewRounds, &t.CreatedAt, &t.UpdatedAt, &t.Priority)
	return t, err
}

// CreateTask assigns the next sequential id (T-0001...), status "assigned"
// unless t.Status is set, and returns the stored task.
func (s *Store) CreateTask(t Task) (Task, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback()
	var max sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(CAST(SUBSTR(id,3) AS INTEGER)) FROM tasks`).Scan(&max); err != nil {
		return Task{}, err
	}
	t.ID = fmt.Sprintf("T-%04d", max.Int64+1)
	if t.Status == "" {
		t.Status = StatusAssigned
	}
	if t.Files == "" {
		t.Files = "[]"
	}
	t.CreatedAt = s.ts()
	t.UpdatedAt = t.CreatedAt
	_, err = tx.Exec(`INSERT INTO tasks(`+taskCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.ParentID, t.Title, t.Description, t.Assignee, t.CreatedBy, t.Status, t.SpecPath, t.Branch, t.Files, t.ReviewRounds, t.CreatedAt, t.UpdatedAt, t.Priority)
	if err != nil {
		return Task{}, err
	}
	return t, tx.Commit()
}

func (s *Store) GetTask(id string) (Task, error) {
	t, err := scanTask(s.db.QueryRow(`SELECT `+taskCols+` FROM tasks WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrNotFound
	}
	if err == nil {
		t.DependsOn, err = s.TaskDeps(id)
	}
	if err == nil { // the per-AC handoff (backlog #6); GetTask only, ListTasks stays light
		_, t.ACClaims, err = s.LatestACClaims(id)
	}
	if err == nil {
		t.ACVerdicts, err = s.LatestACVerdicts(id)
	}
	return t, err
}

// ListTasks returns tasks ordered by id; status "" means all.
func (s *Store) ListTasks(status string) ([]Task, error) {
	q := `SELECT ` + taskCols + ` FROM tasks`
	var args []any
	if status != "" {
		q += ` WHERE status=?`
		args = append(args, status)
	}
	rows, err := s.db.Query(q+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, s.hydrateDeps(out)
}

// SetTaskPriority sets a task's priority (higher runs first).
func (s *Store) SetTaskPriority(id string, priority int) error {
	res, err := s.db.Exec(`UPDATE tasks SET priority=?, updated_at=? WHERE id=?`, priority, s.ts(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkReverted records that a merged task was reverted off the integration branch. It
// is the only way out of merged (CanTransition treats merged as terminal).
func (s *Store) MarkReverted(id string) error {
	res, err := s.db.Exec(`UPDATE tasks SET status=?, updated_at=? WHERE id=? AND status=?`, StatusReverted, s.ts(), id, StatusMerged)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("task %s is not merged", id)
	}
	return nil
}

// ApplyPriority changes a task's priority and returns the new value. spec is an
// integer ("3", "-1"), "up" or "down" (one step), or "top" (above every other open
// task; unchanged if it already is).
func (s *Store) ApplyPriority(id, spec string) (int, error) {
	t, err := s.GetTask(id)
	if err != nil {
		return 0, err
	}
	next := t.Priority
	switch spec {
	case "up":
		next++
	case "down":
		next--
	case "top":
		var maxOther sql.NullInt64
		_ = s.db.QueryRow(`SELECT MAX(priority) FROM tasks WHERE id<>? AND status NOT IN (?,?,?)`, id, StatusMerged, StatusDone, StatusCancelled).Scan(&maxOther)
		if maxOther.Valid && int(maxOther.Int64) >= next {
			next = int(maxOther.Int64) + 1
		}
	default:
		n, err := strconv.Atoi(spec)
		if err != nil {
			return 0, fmt.Errorf("priority must be a number, up, down or top, got %q", spec)
		}
		next = n
	}
	return next, s.SetTaskPriority(id, next)
}

// TaskPriorities maps every task id to its priority.
func (s *Store) TaskPriorities() map[string]int {
	out := map[string]int{}
	rows, err := s.db.Query(`SELECT id, priority FROM tasks`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var p int
		if rows.Scan(&id, &p) == nil {
			out[id] = p
		}
	}
	return out
}

// UpdateTaskStatus validates the transition with CanTransition and applies it
// atomically. Moving to changes_requested increments review_rounds.
func (s *Store) UpdateTaskStatus(id, to string) error {
	return s.UpdateTaskStatusNotes(id, to, nil)
}

// UpdateTaskStatusNotes is UpdateTaskStatus where the note sent to a released
// dependent (when id merges or is cancelled) carries notes[dependent id] in place of
// the generic "bring your branch up to date" advice: the supervisor rebases first and
// says what happened.
func (s *Store) UpdateTaskStatusNotes(id, to string, notes map[string]string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var from string
	if err := tx.QueryRow(`SELECT status FROM tasks WHERE id=?`, id).Scan(&from); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if !CanTransition(from, to) {
		return fmt.Errorf("%w: %s -> %s", ErrBadTransition, from, to)
	}
	inc := 0
	if to == StatusChangesRequested {
		inc = 1
	}
	if _, err := tx.Exec(`UPDATE tasks SET status=?, updated_at=?, review_rounds=review_rounds+? WHERE id=?`, to, s.ts(), inc, id); err != nil {
		return err
	}
	if DepSatisfied(to) {
		if err := s.releaseDependents(tx, id, to, notes); err != nil {
			return err
		}
	}
	if to == StatusInReview {
		if err := s.noteDependentsOfReview(tx, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UpdateTaskFields sets mutable non-status fields (title, description, assignee,
// files, spec_path, branch); empty strings are left unchanged.
func (s *Store) UpdateTaskFields(id string, f Task) error {
	res, err := s.db.Exec(`UPDATE tasks SET
 title=COALESCE(NULLIF(?,''),title), description=COALESCE(NULLIF(?,''),description),
 assignee=COALESCE(NULLIF(?,''),assignee), files=COALESCE(NULLIF(?,''),files),
 spec_path=COALESCE(NULLIF(?,''),spec_path), branch=COALESCE(NULLIF(?,''),branch), updated_at=? WHERE id=?`,
		f.Title, f.Description, f.Assignee, f.Files, f.SpecPath, f.Branch, s.ts(), id)
	return affected(res, err)
}

// AddMessage stores a message and returns its id.
func (s *Store) AddMessage(m Message) (int64, error) {
	if m.Kind == "" {
		m.Kind = "note"
	}
	// An engineer must not claim a fresh assignment before the supervisor has
	// prepared its worktree. Insert the message and its inbox hold in one write
	// transaction so concurrent get_inbox calls never see an unheld assignment.
	if m.Kind == "assignment" && m.TaskID != "" {
		tx, err := s.db.Begin()
		if err != nil {
			return 0, err
		}
		defer tx.Rollback()
		res, err := tx.Exec(`INSERT INTO messages(from_agent, to_agent, kind, task_id, reply_to, body, created_at) VALUES(?,?,?,?,?,?,?)`,
			m.From, m.To, m.Kind, m.TaskID, m.ReplyTo, m.Body, s.ts())
		if err != nil {
			return 0, err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`INSERT INTO kv(key, value) SELECT ?, '1' WHERE EXISTS (SELECT 1 FROM agents WHERE id=? AND role='engineer')`,
			KVInboxHold+strconv.FormatInt(id, 10), m.To); err != nil {
			return 0, err
		}
		return id, tx.Commit()
	}
	res, err := s.db.Exec(`INSERT INTO messages(from_agent, to_agent, kind, task_id, reply_to, body, created_at) VALUES(?,?,?,?,?,?,?)`,
		m.From, m.To, m.Kind, m.TaskID, m.ReplyTo, m.Body, s.ts())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UndeliveredFor returns undelivered messages addressed to agent (or "all").
func (s *Store) UndeliveredFor(agent string) ([]Message, error) {
	rows, err := s.db.Query(`SELECT id, from_agent, to_agent, kind, task_id, reply_to, body, created_at, delivered_at, woken_at
FROM messages WHERE to_agent=? AND delivered_at=0 ORDER BY id`, agent)
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

// MarkDelivered marks messages delivered (idempotent).
func (s *Store) MarkDelivered(ids ...int64) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.Exec(`UPDATE messages SET delivered_at=? WHERE id=? AND delivered_at=0`, s.ts(), id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MarkWoken records that a wake-up nudge was sent for these messages.
func (s *Store) MarkWoken(ids ...int64) error {
	for len(ids) > 0 {
		chunk := ids
		if len(chunk) > 500 {
			chunk = ids[:500]
		}
		args := make([]any, 0, len(chunk)+1)
		args = append(args, s.ts())
		for _, id := range chunk {
			args = append(args, id)
		}
		if _, err := s.db.Exec(`UPDATE messages SET woken_at=? WHERE id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")+`)`, args...); err != nil {
			return err
		}
		ids = ids[len(chunk):]
	}
	return nil
}

// ClaimInbox atomically returns and marks delivered all undelivered messages for agent.
func (s *Store) ClaimInbox(agent string) ([]Message, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id, from_agent, to_agent, kind, task_id, reply_to, body, created_at, delivered_at, woken_at
FROM messages WHERE to_agent=? AND delivered_at=0 AND NOT EXISTS (SELECT 1 FROM kv WHERE key = 'inbox.hold.' || messages.id) ORDER BY id`, agent)
	if err != nil {
		return nil, err
	}
	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.From, &m.To, &m.Kind, &m.TaskID, &m.ReplyTo, &m.Body, &m.CreatedAt, &m.DeliveredAt, &m.WokenAt); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	now := s.ts()
	for i := range out {
		if _, err := tx.Exec(`UPDATE messages SET delivered_at=? WHERE id=?`, now, out[i].ID); err != nil {
			return nil, err
		}
		out[i].DeliveredAt = now
	}
	return out, tx.Commit()
}

// AddEvent appends an activity-log row; data should be JSON ("" becomes "{}").
func (s *Store) AddEvent(agent, kind, data string) error {
	if data == "" {
		data = "{}"
	}
	_, err := s.db.Exec(`INSERT INTO events(at, agent, kind, data) VALUES(?,?,?,?)`, s.ts(), agent, kind, data)
	return err
}

// LastEventID is the id of the newest event (0 when there are none).
func (s *Store) LastEventID() int64 {
	var id int64
	_ = s.db.QueryRow(`SELECT COALESCE(MAX(id),0) FROM events`).Scan(&id)
	return id
}

// RecentEvents returns the newest events first.
func (s *Store) RecentEvents(limit int) ([]Event, error) {
	rows, err := s.db.Query(`SELECT id, at, agent, kind, data FROM events ORDER BY id DESC LIMIT ?`, limit)
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

// AddUsage records spend for an agent, attributed to the task the agent is working on
// (agents.current_task) at that moment; ” when it has none.
func (s *Store) AddUsage(agent string, inputTokens, outputTokens int64, costUSD float64) error {
	_, err := s.db.Exec(`INSERT INTO usage(agent, at, input_tokens, output_tokens, cost_usd, task_id)
 VALUES(?,?,?,?,?, COALESCE((SELECT current_task FROM agents WHERE id=?), ''))`,
		agent, s.ts(), inputTokens, outputTokens, costUSD, agent)
	return err
}

// MonthSpend sums cost since local midnight on the first of the month.
func (s *Store) MonthSpend() (float64, error) {
	n := s.now()
	start := time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, n.Location()).Unix()
	var v float64
	err := s.db.QueryRow(`SELECT COALESCE(SUM(cost_usd),0) FROM usage WHERE at>=?`, start).Scan(&v)
	return v, err
}

// TodaySpend sums cost since local midnight.
func (s *Store) TodaySpend() (float64, error) {
	n := s.now()
	start := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, n.Location()).Unix()
	var v float64
	err := s.db.QueryRow(`SELECT COALESCE(SUM(cost_usd),0) FROM usage WHERE at>=?`, start).Scan(&v)
	return v, err
}
