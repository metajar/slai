-- Spend per task: each usage row records the task its agent was working on when it was
-- recorded ('' when none). Additive: one column and one index; old rows stay unattributed.
ALTER TABLE usage ADD COLUMN task_id TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_usage_task ON usage(task_id);
