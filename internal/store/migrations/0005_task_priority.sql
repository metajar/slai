-- Task priority: higher runs first. 0 is the default, so a board where nobody has set
-- a priority orders exactly as before. Additive: one column.
ALTER TABLE tasks ADD COLUMN priority INTEGER NOT NULL DEFAULT 0;
