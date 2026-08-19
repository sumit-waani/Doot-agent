# Doot

A personal coding agent. Single user, internal use, driven from a phone.

Give it a repo and a goal, it plans, builds, reviews its own work, verifies the
result by actually driving the UI, and pushes to a branch for me to merge.

**Status:** end to end in place — database, auth, UI, Daytona sandbox, LLM client
and the agent loop. Not yet run against a real project; R2 artifact upload is the
one deliberate gap.

## Docs

| Doc | Contents |
|---|---|
| [01-decisions.md](./docs/01-decisions.md) | Scope, stack, agent loop, config, deployment |
| [02-database.md](./docs/02-database.md) | Full schema, migrations, startup, history retention |
| [03-ui.md](./docs/03-ui.md) | The 5 screens, SSE, PWA |
| [04-sandbox-and-git.md](./docs/04-sandbox-and-git.md) | Daytona sandbox, computer-use, branch strategy |

## Shape

- **Go** single binary → Docker → **Fly.io**, one always-on machine, no volumes
- **Turso** holds all state; the machine is treated as disposable
- **Daytona** sandbox, one per project, running both the project and computer-use
- **Muse Spark 1.2** via the OpenAI SDK, for the primary agent and both subagents
- **htmx + SSE**, server-rendered, installable PWA — no build step, no SPA

## Rules it's built around

- One project at a time, enforced in the database
- One conversation, no sessions
- One branch, always named `doot`
- Max 5 screens
- Nothing important stored on the machine that runs it

## Running

Three environment variables, and that's the lot. Everything else is configured
from the Settings screen.

```sh
export DOOT_MASTER_KEY=$(go run ./cmd/doot genkey)
export TURSO_DATABASE_URL='libsql://your-db.turso.io'
export TURSO_AUTH_TOKEN='...'

go run ./cmd/doot serve      # http://localhost:8080
```

For a local run without a Turso account, point the URL at a file instead. The
driver is chosen by URL scheme, so there is no way to mix the two up:

```sh
TURSO_DATABASE_URL=./local.db DOOT_MASTER_KEY=$(go run ./cmd/doot genkey) \
  DOOT_DEV=1 go run ./cmd/doot serve
```

First boot creates the login `doot` / `doot` and nags until you change it.

| Command | |
|---|---|
| `doot serve` | Start the server (migrations run automatically) |
| `doot migrate` | Apply migrations and exit |
| `doot genkey` | Print a new `DOOT_MASTER_KEY` |

| Env var | |
|---|---|
| `TURSO_DATABASE_URL` | Required. Turso URL, or a local file path |
| `TURSO_AUTH_TOKEN` | Required for Turso |
| `DOOT_MASTER_KEY` | Required. Encrypts stored credentials |
| `PORT` | Default `8080` |
| `DOOT_DEV` | Text logs, live template reload, non-Secure cookie |
| `DOOT_LOG_LEVEL` | `debug` \| `info` \| `warn` \| `error` |
| `DOOT_RESET_ADMIN` | Break-glass: resets login to `doot`/`doot` on boot |

Credentials can be seeded on first boot with `DOOT_LLM_API_KEY`,
`DOOT_DAYTONA_API_KEY`, `DOOT_GITHUB_PAT`, `DOOT_R2_ACCESS_KEY_ID` and
`DOOT_R2_SECRET_ACCESS_KEY`. After that the encrypted database copy wins, so a
stale env var can't undo a rotation done from your phone.

Deploy: `fly secrets set` those three, then `fly deploy`. Migrations run on
startup, so deploying is the only step.

Icons are generated, not committed by hand: `go run ./tools/genicons`.

## Local testing

Two stub servers, because the two things Doot depends on are the two things you
cannot exercise offline.

### The database, over the real wire protocol

`tools/hranastub` is a minimal Hrana v2 server backed by a local SQLite file.

Point `TURSO_DATABASE_URL` at it and the **production driver** is used —
`libsql-client-go` over HTTP, with the baton and stream semantics Turso really
has. A local file path uses a different driver entirely, so it cannot catch bugs
in that seam. One already reached production this way:

```sh
python3 tools/hranastub/stub.py 8301 ./local.db
TURSO_DATABASE_URL=http://127.0.0.1:8301 TURSO_AUTH_TOKEN=x \
  DOOT_MASTER_KEY=$(go run ./cmd/doot genkey) go run ./cmd/doot serve
```

**Anything touching transactions should be checked this way before deploying.**
In particular, never pin a `sql.Conn` and issue `BEGIN` by hand: over Hrana a
request that leaves no transaction open returns no baton, the driver marks that
connection dead, and a pinned connection gets no retry. Use `db.BeginTx`.

### The model

`tools/stubllm` is a scripted OpenAI-compatible server. It streams responses from
a JSON scenario in the same SSE shape the real API uses, so the loop's control
flow — tool calls, plan approval, pausing, compaction — can be driven over real
HTTP.

```sh
python3 tools/stubllm/stub.py 8188 scenario.json
```

Then point the model at it in Settings: base URL `http://127.0.0.1:8188/v1`, any
API key. `GET /__calls` on the stub returns what the agent actually sent, which
is how the prompt, the tool schemas and the usage request get verified.
