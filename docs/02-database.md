# Database

Turso (libSQL — SQLite dialect). **All durable state lives here.** Fly machines
are ephemeral and hold nothing of value; there are no volumes.

Conventions:

- Timestamps are TEXT, ISO-8601 UTC: `strftime('%Y-%m-%dT%H:%M:%fZ','now')`.
- Booleans are `INTEGER` 0/1.
- JSON is stored as TEXT.
- `PRAGMA foreign_keys = ON` is set on every connection.

---

## Migrations

Numbered SQL files embedded in the binary via `go:embed`
(`0001_init.sql`, `0002_....sql`, …), applied on startup.

**Rules:**

1. **Runs automatically on every boot.** Not a deploy step, not a manual
   command. Deploying is the only action required.
2. **Idempotent.** Already-applied versions are skipped by version number.
   Re-running the binary is always safe.
3. **Transactional per file.** Each migration runs inside `BEGIN IMMEDIATE` …
   `COMMIT`. A failure rolls that file back and aborts startup loudly — the
   server does not serve on a half-migrated schema.
4. **`BEGIN IMMEDIATE` takes a write lock up front**, so two machines booting
   at once cannot double-apply. (There should only ever be one machine, but
   deploys briefly overlap.)
5. **Forward-only.** No down migrations. To undo, write a new migration.
6. **Checksummed.** The runner stores a hash of each applied file and refuses
   to start if a previously applied file was edited — that means someone
   changed history, which is a bug worth crashing over.

The tracking table is created by the runner itself, not by a migration:

```sql
CREATE TABLE IF NOT EXISTS schema_migrations (
  version    INTEGER PRIMARY KEY,
  name       TEXT NOT NULL,
  checksum   TEXT NOT NULL,
  applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
```

## Startup Sequence

Fixed order. Each step must succeed before the next:

1. Connect to Turso, `PRAGMA foreign_keys = ON`.
2. Run migrations.
3. **Ensure default user** — see below.
4. Reconcile interrupted runs: any run with `active = 1` and
   `status = 'running'` becomes `status = 'interrupted', active = 0`.
5. Prune the `events` table (retention below).
6. Start serving.

### Ensure Default User

```
count = SELECT COUNT(*) FROM users
if count > 0 -> do nothing, continue startup
if count = 0 -> INSERT username 'doot', password_hash = argon2id('doot')
```

Idempotent by construction. Credentials are changeable from the Settings
screen afterwards; there is no forced password change, just a dismissible
banner while the default password is still in use.

If `DOOT_RESET_ADMIN=1` is set, the password for `doot` is reset to `doot` on
boot regardless of existing users. Break-glass only.

---

## Schema

Table creation order matters (foreign keys reference earlier tables).

### Auth

```sql
CREATE TABLE users (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  username      TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  updated_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE sessions (
  token_hash   TEXT PRIMARY KEY,          -- sha256 of the cookie value
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  user_agent   TEXT,
  created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  last_seen_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  expires_at   TEXT NOT NULL
);

CREATE INDEX idx_sessions_expires ON sessions(expires_at);
```

Only the hash of the session token is stored, so a database leak doesn't hand
over live sessions. Expired rows are pruned on startup and lazily on use.

### Config

```sql
CREATE TABLE settings (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,
  updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE secrets (
  name       TEXT PRIMARY KEY,
  ciphertext BLOB NOT NULL,   -- AES-256-GCM under DOOT_MASTER_KEY
  nonce      BLOB NOT NULL,
  updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
```

Secret names: `llm.api_key`, `daytona.api_key`, `github.pat`, `r2.access_key_id`,
`r2.secret_access_key`.

Settings are seeded with `INSERT OR IGNORE` in the migration, so seeding is
idempotent and never clobbers a value I've changed:

| Key | Default |
|---|---|
| `llm.model` | `muse-spark-1.2` |
| `llm.base_url` | *(Meta Model API base URL)* |
| `llm.context_window` | `200000` |
| `llm.max_output_tokens` | `8192` |
| `agent.compact_threshold_pct` | `80` |
| `agent.system_prompt` | *(seed prompt)* |
| `agent.reviewer_enabled` | `1` |
| `agent.e2e_enabled` | `1` |
| `sandbox.snapshot` | `daytona-medium` |
| `sandbox.auto_stop_minutes` | `30` |
| `sandbox.vnc_resolution` | `1280x800` |
| `git.work_branch` | `doot` |
| `git.author_name` | `doot` |
| `git.author_email` | `doot@local` |
| `github.create_pr` | `1` |
| `pricing.input_per_mtok` | *(USD)* |
| `pricing.cached_input_per_mtok` | *(USD)* |
| `pricing.output_per_mtok` | *(USD)* |

Pricing lives in settings so cost math is correctable without a redeploy.

### Project — exactly one row

```sql
CREATE TABLE project (
  id               INTEGER PRIMARY KEY CHECK (id = 1),
  name             TEXT NOT NULL,
  repo_url         TEXT NOT NULL,
  repo_owner       TEXT NOT NULL,
  repo_name        TEXT NOT NULL,
  base_branch      TEXT NOT NULL DEFAULT 'main',
  work_branch      TEXT NOT NULL DEFAULT 'doot',
  setup_script     TEXT,          -- installs deps inside the sandbox
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
```

`CHECK (id = 1)` is how "one project, strictly" is enforced. Inserting a second
project fails at the database, so no application bug can produce a second one.
Since there is only ever one project, other tables **do not carry a
`project_id`** — it would be dead weight on every row and every query.

`vnc_resolution` is recorded because Daytona fixes the framebuffer at sandbox
creation and it cannot be changed afterwards; the stored value is what the
running sandbox actually has.

### Conversation Epochs

An **epoch** is one continuous stretch of conversation. Clearing or compacting
**ends the current epoch and opens a new one — it never deletes messages.**
This is a deliberate override of the earlier draft, which discarded history at
compaction.

```sql
CREATE TABLE conversation_epochs (
  epoch         INTEGER PRIMARY KEY,
  reason        TEXT CHECK (reason IN ('clear','compact')), -- NULL for the first
  summary       TEXT,          -- compaction summary; NULL when reason='clear'
  message_count INTEGER NOT NULL DEFAULT 0,
  total_tokens  INTEGER NOT NULL DEFAULT 0,
  started_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  ended_at      TEXT
);
```

- **Live context** = `WHERE epoch = project.current_epoch ORDER BY seq`.
- **History** = everything else, still on disk, queryable forever.
- Clear: stamp `ended_at` + `reason='clear'`, bump `project.current_epoch`.
- Compact: same, but `reason='compact'`, store the summary, and write it as the
  first message of the new epoch with `is_summary = 1`.

The win is that "preserve history" and "keep the live query trivial" stop being
in tension — one integer column separates them.

### Runs

A **run** is one execution of the agent loop.

```sql
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

-- At most one active run, ever. Enforced by the database, not by a mutex.
CREATE UNIQUE INDEX idx_runs_one_active ON runs(active) WHERE active = 1;
```

The partial unique index is the whole concurrency-control story: a second
concurrent run is an insert conflict. A process-memory mutex would not survive
a machine restart; this does.

`heartbeat_at` is written periodically while running — it drives both the
"is this run actually alive" check after a restart and the Daytona activity
heartbeat.

### Goal Plans

```sql
CREATE TABLE goal_plans (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  epoch        INTEGER NOT NULL REFERENCES conversation_epochs(epoch),
  run_id       INTEGER REFERENCES runs(id),
  title        TEXT NOT NULL,
  goal         TEXT NOT NULL,
  deliverables TEXT,           -- JSON array
  raw          TEXT,           -- full JSON exactly as the model emitted it
  status       TEXT NOT NULL CHECK (status IN (
                 'awaiting_approval','approved','rejected',
                 'in_progress','completed','abandoned')),
  approved_at  TEXT,
  created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  updated_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

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
```

`raw` keeps the model's original JSON so the prompt contract can evolve without
a migration; `plan_tasks` is a real table because the UI renders live progress
per task and JSON blobs make that awkward to update.

`review_verdict = 'dismissed'` records that the primary agent judged the
reviewer's finding a false positive — worth keeping, since that judgement is
exactly what I'd want to audit later.

### Messages

```sql
CREATE TABLE messages (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  epoch             INTEGER NOT NULL REFERENCES conversation_epochs(epoch),
  seq               INTEGER NOT NULL,
  role              TEXT NOT NULL CHECK (role IN ('system','user','assistant','tool')),
  content           TEXT,
  tool_calls        TEXT,      -- JSON array, when the assistant requests tools
  tool_call_id      TEXT,      -- set when role='tool'
  name              TEXT,
  run_id            INTEGER REFERENCES runs(id),
  prompt_tokens     INTEGER,
  completion_tokens INTEGER,
  is_summary        INTEGER NOT NULL DEFAULT 0,
  created_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE UNIQUE INDEX idx_messages_epoch_seq ON messages(epoch, seq);
```

Rows here are **append-only and never deleted.** This table is the full
transcript, including tool results, across all epochs.

### Tool Calls

```sql
CREATE TABLE tool_calls (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id         INTEGER REFERENCES runs(id),
  message_id     INTEGER REFERENCES messages(id),
  tool_call_id   TEXT,
  name           TEXT NOT NULL,
  args           TEXT,        -- JSON
  result_preview TEXT,        -- truncated, for the timeline UI
  status         TEXT NOT NULL CHECK (status IN ('running','ok','error','cancelled')),
  error          TEXT,
  duration_ms    INTEGER,
  created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE INDEX idx_tool_calls_run ON tool_calls(run_id, id);
```

Full results live in `messages`; this table exists so the UI can render a
compact, scannable timeline (and durations) without parsing the transcript.

### LLM Audit Log

```sql
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
```

`purpose` is what makes this worth having — it answers "is the E2E verifier
actually where my money goes", which is the one cost question I care about.

### Events — durable SSE log

```sql
CREATE TABLE events (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,  -- doubles as the SSE event id
  run_id     INTEGER,
  type       TEXT NOT NULL,
  payload    TEXT NOT NULL,   -- JSON
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE INDEX idx_events_run ON events(run_id, id);
```

This table is what makes SSE trustworthy on a phone. Mobile connections drop
constantly — backgrounding the PWA kills the stream. On reconnect the client
sends `Last-Event-ID` and the server replays from `id >` that value, so nothing
is missed and the UI doesn't need a full reload.

**Retention:** keep 7 days or the most recent 5,000 rows, whichever is larger.
Pruned on startup and hourly. This is the only table that is ever deleted from.

### Artifacts

```sql
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
```

This is what R2 is for. Computer-use screenshots and screen recordings must not
live in the sandbox filesystem (destroyed on reset) or in Turso (wrong tool for
blobs). Recordings in particular are the highest-value debugging artifact when
an E2E verification fails, so they outlive the sandbox.

### Pushes & PRs

```sql
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
```

`pr_status` explicitly includes `failed` and `skipped` as *normal* outcomes —
PR creation is optional by design, so a failure here is recorded and moved past,
never surfaced as a broken run.

---

## Deleting a Project

Deleting the single project is destructive and deliberate:

1. Delete the Daytona sandbox.
2. End the current epoch (`reason='clear'`).
3. Delete the `project` row.

Conversation history, audit logs, and artifacts **survive** — they are not
scoped to the project row. A fresh project starts at a new epoch with the old
transcript still on disk. Given that "delete and recreate" is the intended way
to switch projects, silently destroying the record of prior work would be the
wrong default.
