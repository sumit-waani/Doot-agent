# Doot

A personal coding agent. Single user, internal use, driven from a phone.

Give it a repo and a goal, it plans, builds, reviews its own work, verifies the
result by actually driving the UI, and pushes to a branch for me to merge.

**Status: design locked, not yet implemented.**

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
