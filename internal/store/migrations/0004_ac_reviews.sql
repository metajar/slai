-- Per-acceptance-criterion handoff between an engineer and the reviewer (backlog #6):
-- the engineer's claims at each request_review, the reviewer's verdict per AC.
-- review_no counts the task's review requests (1, 2, ...); verdicts attach to the
-- latest request. Additive: no existing table changes.
CREATE TABLE IF NOT EXISTS task_ac_claims (
  task_id   TEXT    NOT NULL,
  review_no INTEGER NOT NULL,
  ac        TEXT    NOT NULL,
  met       INTEGER NOT NULL,
  note      TEXT    NOT NULL DEFAULT '',
  PRIMARY KEY (task_id, review_no, ac)
);
CREATE TABLE IF NOT EXISTS task_ac_verdicts (
  task_id   TEXT    NOT NULL,
  review_no INTEGER NOT NULL,
  ac        TEXT    NOT NULL,
  pass      INTEGER NOT NULL,
  note      TEXT    NOT NULL DEFAULT '',
  by        TEXT    NOT NULL DEFAULT '',
  at        INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (task_id, review_no, ac)
);
