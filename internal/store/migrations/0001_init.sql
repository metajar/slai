CREATE TABLE agents (
  id TEXT PRIMARY KEY, role TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'idle',
  status_at INTEGER NOT NULL DEFAULT 0, tmux_session TEXT NOT NULL DEFAULT '', worktree TEXT NOT NULL DEFAULT '',
  current_task TEXT NOT NULL DEFAULT '', turns INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE tasks (
  id TEXT PRIMARY KEY,
  parent_id TEXT NOT NULL DEFAULT '', title TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '',
  assignee TEXT NOT NULL DEFAULT '', created_by TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'assigned',
  spec_path TEXT NOT NULL DEFAULT '', branch TEXT NOT NULL DEFAULT '', files TEXT NOT NULL DEFAULT '[]',
  review_rounds INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE messages (
  id INTEGER PRIMARY KEY, from_agent TEXT NOT NULL DEFAULT '', to_agent TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL DEFAULT 'note',
  task_id TEXT NOT NULL DEFAULT '', reply_to INTEGER NOT NULL DEFAULT 0, body TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL DEFAULT 0, delivered_at INTEGER NOT NULL DEFAULT 0, woken_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE events (
  id INTEGER PRIMARY KEY, at INTEGER NOT NULL DEFAULT 0, agent TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL DEFAULT '', data TEXT NOT NULL DEFAULT '{}'
);
CREATE TABLE usage (
  agent TEXT NOT NULL, at INTEGER NOT NULL, input_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0, cost_usd REAL NOT NULL DEFAULT 0
);
CREATE INDEX idx_tasks_status ON tasks(status);
CREATE INDEX idx_tasks_assignee ON tasks(assignee);
CREATE INDEX idx_messages_inbox ON messages(to_agent, delivered_at);
CREATE INDEX idx_events_at ON events(at);
CREATE INDEX idx_events_agent ON events(agent);
CREATE INDEX idx_usage_at ON usage(at);
CREATE INDEX idx_usage_agent ON usage(agent);
