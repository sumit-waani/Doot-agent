# Doot — Locked Decisions

*Locked: Aug 18, 2026. Supersedes the earlier root-level draft.*

This is the master decision doc. Anything not stated here or in the sibling docs
is a deliberate implementation-time choice.

- [02-database.md](./02-database.md) — schema, migrations, bootstrap
- [03-ui.md](./03-ui.md) — the 5 screens, PWA, SSE
- [04-sandbox-and-git.md](./04-sandbox-and-git.md) — Daytona + git strategy

---

## 1. What Doot Is

A **personal, single-user, internal** coding agent. One operator: me. Runs
mostly from a phone.

**Explicitly not:** an MVP, a product, a public tool, multi-tenant, an agency
job runner, or enterprise software. There is no "user growth" path. Every
tradeoff resolves toward *fewer moving parts and fewer taps*, not toward
generality.

Design constraints that follow from this, treated as hard rules:

| Rule | Consequence |
|---|---|
| Single user, ever | No orgs, teams, roles, permissions, invites, or sharing |
| ADHD operator, hates clutter | **Max 5 screens.** No screen-hopping to do one thing |
| Used from a phone | Mobile-first UI, installable PWA. Desktop is the afterthought |
| Exactly one project | Enforced in the DB, not just the UI |
| Fly machines are ephemeral | **Zero trust in local disk.** All state in Turso |

### Non-Goals

- No multi-agent swarm. 1 primary + 2 focused subagents, invoked as tools.
- No "sessions" concept. No conversation-per-session sprawl.
- No multi-project support, not even hidden behind a flag.
- No notifications, email, push, or webhooks.
- No feature flags, A/B infra, analytics, or telemetry.
- No horizontal scaling. One machine, one loop.

---

## 2. Single Project, Strictly

**One project at a time. Enforced, not encouraged.**

- The `project` table holds **at most one row**, enforced by `CHECK (id = 1)`.
  A second project is a *database error*, not a UI validation message.
- To switch projects: delete the current one, create a new one. That is the
  entire workflow. Deleting destroys the sandbox and archives the conversation.
- **1 project = 1 conversation.** No sessions, no threads, no branching chats.
- There is no project picker screen, because there is nothing to pick.

### Clear Conversation

One prominent button. Wipes the *visible* conversation so the agent starts
fresh.

- **Never deletes history.** History is retained in the database — see
  [Conversation Epochs](./02-database.md#conversation-epochs). This overrides
  the earlier draft, which said history was not preserved post-compaction.
- **Never touches the sandbox.** Repo, branch, filesystem, running processes
  are untouched.
- Sandbox reset is a separate, deliberate action on the Project screen.

---

## 3. Stack

| Concern | Decision | Why |
|---|---|---|
| Language | **Go** | Single binary, cheap concurrency for the loop + SSE fanout |
| LLM | **Muse Spark 1.2** (Meta Model API) | One model for everything |
| LLM SDK | **OpenAI SDK**, custom base URL | Meta Model API is OpenAI-compatible |
| Database | **Turso** (libSQL/SQLite) | Cloud-hosted, so Fly disk is irrelevant |
| Object storage | **Cloudflare R2** | Screenshots + screen recordings from computer-use |
| Sandbox | **Daytona** | Persistent sandbox + native computer-use |
| Hosting | **Fly.io**, single always-on machine | See [Deployment](#7-deployment) |
| UI | Go `html/template` + htmx + minimal vanilla JS | No build step, no SPA |
| Live updates | **SSE** | One-way, single user. WebSockets buy nothing here |

**Subagent models:** same model (Muse Spark 1.2) for reviewer, E2E verifier,
and compaction. No model routing, no cheap-model tier. One model to reason
about, one price to track.

**No volumes.** Not one. If the machine dies mid-run, nothing of value is on
it. This is a load-bearing property, not an optimization.

---

## 4. Configuration & Secrets

Two stores, split by whether the value is a secret:

**Non-secret config → `settings` table in Turso.** Model name, base URL,
system prompt, sandbox defaults, compaction threshold, token pricing, work
branch, PR toggle. All editable from the Settings screen, no redeploy.

**Secrets → `secrets` table in Turso, AES-256-GCM encrypted at rest** under a
single `DOOT_MASTER_KEY` env var. Env vars act as bootstrap/fallback values on
first boot.

Why encrypted-in-DB rather than env-vars-only: I'm on a phone. When a GitHub
PAT expires, I need to paste a new one from the Settings screen — not run
`fly secrets set` from a device that has no terminal. Env-only config would
make the mobile-first premise a lie.

The only true env vars are the ones needed to reach the database and decrypt:

| Env var | Purpose |
|---|---|
| `TURSO_DATABASE_URL` | DB connection |
| `TURSO_AUTH_TOKEN` | DB auth |
| `DOOT_MASTER_KEY` | 32-byte base64 key for secret encryption |
| `DOOT_RESET_ADMIN` | Optional break-glass; resets login to `doot`/`doot` on boot |

Everything else lives in the database. There is no onboarding wizard — the
Settings screen *is* the setup screen, and it's usable while half-configured.

---

## 5. Authentication

Single user, but a real `users` table (see [02-database.md](./02-database.md)).

- Username + password → Argon2id hash.
- **On every startup:** check whether any user exists. If yes, do nothing. If
  no, create `doot` / `doot`. Changeable later from the Settings screen.
- Session cookie: `HttpOnly`, `Secure`, `SameSite=Lax`, **90-day expiry** —
  long, because an installed PWA that logs me out weekly is unusable.
- Login attempts are rate-limited. No 2FA, no email reset, no password
  recovery flow. Break-glass is the `DOOT_RESET_ADMIN` env var.

---

## 6. Agent Architecture

**1 primary agent** owns the full lifecycle. **2 subagents**, exposed to the
primary as ordinary tools — not an orchestrator, not a swarm:

- **Semantic codebase reviewer** — invoked after each phase/subtask completes.
- **E2E run verifier** — drives the real UI via Daytona computer-use.

### Loop

1. **Default mode is conversation.** I send a message, the agent replies or
   does small work directly. No ceremony, no plan required.
2. **On explicit ask** ("create a goal plan"), the primary agent produces a
   structured goal plan — deliverables, phases, subtasks — and presents it for
   **approval**. Nothing executes before approval.
3. **On approval: full autonomy.** Phases execute one by one. Small local
   commits as work progresses. No forced mid-flow checkpoints.
4. **Context compaction at ~80%**, and only at a natural checkpoint (e.g.
   "auth is done"). The agent calls a compress tool: full transcript goes to a
   summarization call, the conversation rolls to a new epoch, and the summary
   becomes the first message of that epoch. Old epoch rows stay in the
   database. No separate "memory store" abstraction.
5. **Reviewer loop.** After each phase, the reviewer runs. The primary agent
   has full authority to fix genuine issues, dismiss false positives, or
   escalate. **No hardcoded retry cap** — in extensive prior internal use,
   runaway fix loops were not a real problem.
6. **E2E verification, used mindfully.** Once before final goal completion,
   plus mid-task only when a change is both UI-related *and* critical. Cost is
   acceptable here; this replaces manual QA.
7. **Ask-human tool** surfaces as a normal message in the conversation. The run
   parks in `awaiting_human` and visibly waits. No notification system.
8. **On completion:** preview URL + summary, posted in the conversation.

### Pause

One prominent Pause button. Cooperative cancellation checked between steps and
between tool calls, so it takes effect within one tool call rather than
instantly mid-call.

Pause state lives in the `runs` table, not in memory — so a Fly machine
restart cannot lose it, and a paused run survives redeploys.

### Run Durability

Because the machine is ephemeral and Fly restarts hosts for maintenance:

- **At most one active run**, enforced by a unique index — not by a mutex in
  process memory.
- Every meaningful step writes to the database before proceeding.
- **On boot,** any run still marked `running` is marked `interrupted` and
  offered for resume. It is never silently resurrected.

---

## 7. Deployment

Fly.io, via Docker (Fly won't take a bare binary).

- **Exactly one machine.** `min_machines_running = 1`,
  `auto_stop_machines = false`. Auto-stop would kill a long agent run
  mid-flight, and two machines would mean two loops racing over one project.
- **No volumes.**
- **Migrations run on startup**, inside the binary, idempotently. Not a
  separate deploy step, not a manual action. See
  [Migrations](./02-database.md#migrations).
- Startup order is fixed: connect DB → migrate → ensure default user →
  reconcile interrupted runs → serve.

---

## 8. Git Strategy

Full detail in [04-sandbox-and-git.md](./04-sandbox-and-git.md). The locked
core:

- **One branch, always, named `doot`.** No branch naming derived from task,
  date, plan, or ticket. Never commits to `main`/`master`.
- Pushed to `origin/doot`, the same branch, every time.
- **PR creation is best-effort.** If the API call works, great. If it fails or
  a PR already exists, that is logged and ignored — not an error. I open and
  merge PRs at my own pace.

---

## 9. Sandbox

Full detail in [04-sandbox-and-git.md](./04-sandbox-and-git.md). The locked
core:

- **One persistent sandbox per project**, created with the project.
- **Default size: 2 vCPU / 4 GiB.** This maps exactly onto Daytona's stock
  `daytona-medium` snapshot (2 vCPU / 4 GiB / 8 GiB disk).
- **Computer-use and the project share the one sandbox.** No second sandbox,
  no linked sandbox.
- This forces one non-obvious constraint: Daytona ships VNC/computer-use
  support **only in its default image**. So the sandbox is created from the
  stock snapshot and project dependencies are installed at runtime. A custom
  Docker image for the sandbox is off the table — it would silently break
  computer-use.

---

## 10. Deliberately Deferred

These stay open until implementation, on purpose:

- Exact goal-plan JSON schema the model emits (the DB shape is locked; the
  prompt contract is not).
- System prompt wording and tool descriptions.
- Daytona signed-preview-URL expiry strategy — needs hands-on testing.
- Whether the accessibility (AT-SPI) API beats pixel coordinates for E2E
  driving. Suspect yes; unverified.
- Go SDK gaps in Daytona's computer-use surface (compressed screenshots in
  particular) and whether a thin REST fallback is needed. See
  [04-sandbox-and-git.md](./04-sandbox-and-git.md#go-sdk-gaps).
