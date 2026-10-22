-- Team memory: a derived, rebuildable index of the Markdown facts under .slai/memory/.
-- The files are the source of truth; `slai memory reindex` recreates every column except
-- the volatile counters (uses, last_used_at). Additive: one table.
CREATE TABLE facts_index (
  id              TEXT PRIMARY KEY,
  kind            TEXT NOT NULL DEFAULT '',
  status          TEXT NOT NULL,            -- proposed | verified | invalid
  author          TEXT NOT NULL DEFAULT '',
  verified_by     TEXT NOT NULL DEFAULT '',
  integration_sha TEXT NOT NULL DEFAULT '',
  created         TEXT NOT NULL DEFAULT '',
  paths_json      TEXT NOT NULL DEFAULT '[]',
  decision        TEXT NOT NULL DEFAULT '',
  check_cmd       TEXT NOT NULL DEFAULT '',
  file_hash       TEXT NOT NULL DEFAULT '',  -- sha256 of the file's bytes
  invalid_reason  TEXT NOT NULL DEFAULT '',
  stale           INTEGER NOT NULL DEFAULT 0,
  stale_reason    TEXT NOT NULL DEFAULT '',
  checked_sha     TEXT NOT NULL DEFAULT '',  -- slai/integration commit the staleness was computed against
  checked_at      INTEGER NOT NULL DEFAULT 0,
  uses            INTEGER NOT NULL DEFAULT 0,
  last_used_at    INTEGER NOT NULL DEFAULT 0
);
