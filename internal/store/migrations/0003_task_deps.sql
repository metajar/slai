CREATE TABLE IF NOT EXISTS task_deps (
  task_id TEXT NOT NULL,
  dep_id  TEXT NOT NULL,
  PRIMARY KEY (task_id, dep_id)
);
CREATE INDEX IF NOT EXISTS idx_task_deps_dep ON task_deps(dep_id);
