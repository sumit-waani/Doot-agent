-- 0001_init.sql
-- Initial schema. See docs/02-database.md.
-- Table order matters: foreign keys may only reference tables created earlier.

-- ---------------------------------------------------------------- auth

CREATE TABLE users (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  username      TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  updated_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE sessions (
  token_hash   TEXT PRIMARY KEY,
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  user_agent   TEXT,
  created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  last_seen_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  expires_at   TEXT NOT NULL
);

CREATE INDEX idx_sessions_expires ON sessions(expires_at);

-- ---------------------------------------------------------------- config

CREATE TABLE settings (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,
  updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE secrets (
  name       TEXT PRIMARY KEY,
  ciphertext BLOB NOT NULL,
  nonce      BLOB NOT NULL,
  updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

-- ---------------------------------------------------------------- project

-- CHECK (id = 1) is how "one project, strictly" is enforced. A second project
-- is a database error, not a UI validation message.
CREATE TABLE project (
  id               INTEGER PRIMARY KEY CHECK (id = 1),
  name             TEXT NOT NULL,
  repo_url         TEXT NOT NULL,
  repo_owner       TEXT NOT NULL,
  repo_name        TEXT NOT NULL,
  base_branch      TEXT NOT NULL DEFAULT 'main',
  work_branch      TEXT NOT NULL DEFAULT 'doot',
  setup_script     TEXT,
  dev_command      TEXT,
  dev_port         INTEGER,
  sandbox_id       TEXT,
  sandbox_state    TEXT,
  sandbox_snapshot TEXT NOT NULL DEFAULT 'daytona-medium',
  vnc_resolution   TEXT NOT NULL DEFAULT '1280x800',
  current_epoch    INTEGER NOT NULL DEFAULT 1,
  created_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  updated_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

-- ---------------------------------------------------------------- conversation

-- Clearing or compacting ends an epoch and opens a new one. Messages are never
-- deleted; history is retained across both operations.
CREATE TABLE conversation_epochs (
  epoch         INTEGER PRIMARY KEY,
  reason        TEXT CHECK (reason IN ('clear','compact')),
  summary       TEXT,
  message_count INTEGER NOT NULL DEFAULT 0,
  total_tokens  INTEGER NOT NULL DEFAULT 0,
  started_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  ended_at      TEXT
);

CREATE TABLE runs (
  id                 INTEGER PRIMARY KEY AUTOINCREMENT,
  epoch              INTEGER NOT NULL REFERENCES conversation_epochs(epoch),
  kind               TEXT NOT NULL CHECK (kind IN ('chat','plan','execute')),
  status             TEXT NOT NULL CHECK (status IN (
                       'running','awaiting_approval','awaiting_human',
                       'paused','done','error','interrupted','cancelled')),
  active             INTEGER NOT NULL DEFAULT 1,
  trigger_message_id INTEGER,
  error              TEXT,
  started_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  heartbeat_at       TEXT,
  ended_at           TEXT
);

-- At most one active run, ever. Enforced by the database rather than a
-- process-memory mutex, which would not survive a machine restart.
CREATE UNIQUE INDEX idx_runs_one_active ON runs(active) WHERE active = 1;

CREATE INDEX idx_runs_epoch ON runs(epoch, id);

CREATE TABLE goal_plans (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  epoch        INTEGER NOT NULL REFERENCES conversation_epochs(epoch),
  run_id       INTEGER REFERENCES runs(id),
  title        TEXT NOT NULL,
  goal         TEXT NOT NULL,
  deliverables TEXT,
  raw          TEXT,
  status       TEXT NOT NULL CHECK (status IN (
                 'awaiting_approval','approved','rejected',
                 'in_progress','completed','abandoned')),
  approved_at  TEXT,
  created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  updated_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE INDEX idx_goal_plans_epoch ON goal_plans(epoch, id);

CREATE TABLE plan_tasks (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  plan_id        INTEGER NOT NULL REFERENCES goal_plans(id) ON DELETE CASCADE,
  seq            INTEGER NOT NULL,
  title          TEXT NOT NULL,
  detail         TEXT,
  status         TEXT NOT NULL DEFAULT 'pending' CHECK (status IN (
                   'pending','in_progress','review','done','skipped','failed')),
  review_verdict TEXT CHECK (review_verdict IN ('clean','issues','dismissed')),
  review_notes   TEXT,
  commit_sha     TEXT,
  started_at     TEXT,
  ended_at       TEXT
);

CREATE UNIQUE INDEX idx_plan_tasks_seq ON plan_tasks(plan_id, seq);

-- Append-only. The full transcript, including tool results, across all epochs.
CREATE TABLE messages (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  epoch             INTEGER NOT NULL REFERENCES conversation_epochs(epoch),
  seq               INTEGER NOT NULL,
  role              TEXT NOT NULL CHECK (role IN ('system','user','assistant','tool')),
  content           TEXT,
  tool_calls        TEXT,
  tool_call_id      TEXT,
  name              TEXT,
  run_id            INTEGER REFERENCES runs(id),
  prompt_tokens     INTEGER,
  completion_tokens INTEGER,
  is_summary        INTEGER NOT NULL DEFAULT 0,
  created_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE UNIQUE INDEX idx_messages_epoch_seq ON messages(epoch, seq);

CREATE TABLE tool_calls (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id         INTEGER REFERENCES runs(id),
  message_id     INTEGER REFERENCES messages(id),
  tool_call_id   TEXT,
  name           TEXT NOT NULL,
  args           TEXT,
  result_preview TEXT,
  status         TEXT NOT NULL CHECK (status IN ('running','ok','error','cancelled')),
  error          TEXT,
  duration_ms    INTEGER,
  created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE INDEX idx_tool_calls_run ON tool_calls(run_id, id);

-- ---------------------------------------------------------------- audit

CREATE TABLE llm_calls (
  id                   INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id               INTEGER REFERENCES runs(id),
  epoch                INTEGER,
  purpose              TEXT NOT NULL CHECK (purpose IN
                         ('primary','reviewer','e2e','compaction')),
  model                TEXT NOT NULL,
  prompt_tokens        INTEGER NOT NULL DEFAULT 0,
  cached_prompt_tokens INTEGER NOT NULL DEFAULT 0,
  completion_tokens    INTEGER NOT NULL DEFAULT 0,
  cost_usd             REAL NOT NULL DEFAULT 0,
  latency_ms           INTEGER,
  finish_reason        TEXT,
  error                TEXT,
  created_at           TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE INDEX idx_llm_calls_created ON llm_calls(created_at);
CREATE INDEX idx_llm_calls_run ON llm_calls(run_id, id);

-- The SSE id is this table's primary key, so a dropped mobile connection can
-- reconnect with Last-Event-ID and replay. This is the only table ever pruned.
CREATE TABLE events (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id     INTEGER,
  type       TEXT NOT NULL,
  payload    TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE INDEX idx_events_run ON events(run_id, id);

CREATE TABLE artifacts (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id       INTEGER REFERENCES runs(id),
  tool_call_id TEXT,
  kind         TEXT NOT NULL CHECK (kind IN ('screenshot','recording','diff','log')),
  r2_key       TEXT NOT NULL,
  content_type TEXT,
  size_bytes   INTEGER,
  width        INTEGER,
  height       INTEGER,
  created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE INDEX idx_artifacts_run ON artifacts(run_id, id);

-- pr_status includes 'failed' and 'skipped' as normal outcomes: PR creation is
-- optional by design and never fails a run.
CREATE TABLE pushes (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id     INTEGER REFERENCES runs(id),
  branch     TEXT NOT NULL,
  head_sha   TEXT NOT NULL,
  pr_number  INTEGER,
  pr_url     TEXT,
  pr_status  TEXT CHECK (pr_status IN ('created','exists','failed','skipped')),
  pr_error   TEXT,
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

-- ---------------------------------------------------------------- seed

-- Epoch 1 must exist before any message can reference it, and it exists
-- independently of whether a project has been created yet.
INSERT OR IGNORE INTO conversation_epochs (epoch, started_at)
VALUES (1, strftime('%Y-%m-%dT%H:%M:%fZ','now'));

-- INSERT OR IGNORE so re-seeding never clobbers a value changed from the UI.
INSERT OR IGNORE INTO settings (key, value) VALUES ('llm.model', 'muse-spark-1.2');
INSERT OR IGNORE INTO settings (key, value) VALUES ('llm.base_url', '');
INSERT OR IGNORE INTO settings (key, value) VALUES ('llm.context_window', '200000');
INSERT OR IGNORE INTO settings (key, value) VALUES ('llm.max_output_tokens', '8192');
INSERT OR IGNORE INTO settings (key, value) VALUES ('agent.compact_threshold_pct', '80');
INSERT OR IGNORE INTO settings (key, value) VALUES ('agent.system_prompt', 'You are Doot, a coding agent operating inside a persistent Daytona sandbox. You work on a single project, on a single branch named doot. Read cgroup values (/sys/fs/cgroup/cpu.max, /sys/fs/cgroup/memory.max) rather than nproc or free, which report host values and not sandbox limits.');
INSERT OR IGNORE INTO settings (key, value) VALUES ('agent.reviewer_enabled', '1');
INSERT OR IGNORE INTO settings (key, value) VALUES ('agent.e2e_enabled', '1');
INSERT OR IGNORE INTO settings (key, value) VALUES ('sandbox.snapshot', 'daytona-medium');
INSERT OR IGNORE INTO settings (key, value) VALUES ('sandbox.auto_stop_minutes', '30');
INSERT OR IGNORE INTO settings (key, value) VALUES ('sandbox.vnc_resolution', '1280x800');
INSERT OR IGNORE INTO settings (key, value) VALUES ('git.work_branch', 'doot');
INSERT OR IGNORE INTO settings (key, value) VALUES ('git.author_name', 'doot');
INSERT OR IGNORE INTO settings (key, value) VALUES ('git.author_email', 'doot@local');
INSERT OR IGNORE INTO settings (key, value) VALUES ('github.username', '');
INSERT OR IGNORE INTO settings (key, value) VALUES ('github.create_pr', '1');
INSERT OR IGNORE INTO settings (key, value) VALUES ('pricing.input_per_mtok', '0');
INSERT OR IGNORE INTO settings (key, value) VALUES ('pricing.cached_input_per_mtok', '0');
INSERT OR IGNORE INTO settings (key, value) VALUES ('pricing.output_per_mtok', '0');
