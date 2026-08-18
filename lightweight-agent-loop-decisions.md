# Lightweight Agent Loop — Decision Doc

*Locked: Aug 18, 2026 · Rest to be figured out during implementation*

## Why This Exists

`kaptaan` (existing 13-month-old agent) is battle-tested but heavy — built for
accuracy-first systems work (C, Rust, Erlang, OCaml, Perl, DSLs, storage
engines, web servers). Its cost profile is *intentional* and justified for
that class of work.

Problem: basic/light tasks were also being routed through kaptaan, which is
not cost-justified for them. Rather than touch a 13-month-old,
heavily-patched, framework-less FastAPI loop that's still working — **build a
new, separate, minimal loop for light work.** Kaptaan stays untouched.

## Non-Goals

- No rewriting or refactoring kaptaan.
- No multi-agent orchestrator complexity for the new loop (see Agent
  Architecture below — 1 primary + 2 focused subagents, not a swarm).
- No "sessions" concept. No conversation-per-session sprawl.

---

## Model & SDK Choices

| Concern | Decision | Reasoning |
|---|---|---|
| Primary LLM | **Muse Spark 1.2** (Meta Model API, standard tier) | Cheap ($1.25/M in, $4.25/M out), 1M context, coding-optimized, OpenAI-SDK compatible. Standard tier (not `contributor`) — contributor tier trains on prompts/completions, which is a data-governance call, not a cost call. Revisit only with explicit legal/data sign-off. |
| SDK for LLM calls | **OpenAI SDK** (pointed at Meta Model API base URL) | Avoids reinventing an Anthropic-specific integration for a model that already speaks OpenAI-compatible API. Chinese models intentionally avoided (trust concerns). |
| Subagent models | **Same model (Muse Spark 1.2) for everything** — reviewer, E2E/computer-use agent, compression calls | No benefit to juggling multiple providers' quirks to save marginal cost. Model is multimodal, handles computer-use images natively. Costs are acceptable — this replaces manual hours. |
| Anthropic routing | **None.** New loop never routes to Anthropic. | Keeps clean differentiation from kaptaan. Mixing them back together reintroduces the original "which agent do I even use" confusion. |

---

## Tech Stack

- **Language**: Go — single binary target philosophy (though see Deployment below — Fly needs Docker regardless).
- **Database**: **Turso** — generous free tier, cloud-hosted (avoids Fly volume headaches). One table used as an **audit log** (costs, timestamps, token counts, etc. — NOT full conversation/tool-call history — see Context Management).
- **Object storage**: **Cloudflare R2** — as/when needed.
- **Sandboxing**: **Daytona** — persistent, long-lived sandboxes (not ephemeral per-task).
- **Deployment**: **Fly.io**, via Docker (Fly doesn't take a static binary directly). Standard flow: Dockerfile + `fly launch`/dashboard repo connect. Nothing exotic here.
- **Config**: Secrets/credentials → environment variables. Everything else → Turso.

### SDKs Confirmed
1. **OpenAI SDK** — LLM calls (Muse Spark 1.2 via Meta Model API)
2. **Turso SDK** — DB
3. **Daytona SDK** — sandbox lifecycle, git, preview URLs, computer-use

---

## Onboarding Flow

Simple setup page, no wizard sprawl:
- Daytona credentials
- LLM model name, base URL, API key
- GitHub PAT + username
- System prompt

→ Creds in env vars. Everything else (projects, conversations, audit logs) in Turso.

**Create project flow**: name + repo URL → done. This provisions/assigns a
**persistent Daytona sandbox** tied to that project permanently.

---

## Core Product Model

**1 project = 1 conversation. No sessions.**

- Home screen: pick a project → its single ongoing conversation loads.
- One prominent, explicit **"Clear Conversation"** button — wipes conversation
  history, tool-call noise, etc. for that project **only**. On demand, not automatic.
  - **Does NOT touch the sandbox.** Sandbox state (repo, branch, filesystem)
    is never reset by this action.
  - Sandbox reset/reassignment lives only in a separate project
    settings/management screen, for worst-case use, and is a deliberate,
    separate action.

**Interface**: UI-first, not CLI-first.
- Scope is mostly small projects, used opportunistically (away from desk).
- Minimal UI shipped as part of the binary — plain HTML/CSS/JS, htmx via CDN
  or similar. No SPA framework needed.
- Streaming on UI: tool calls, diffs, ongoing progress — all visible live.
- One prominent **Pause** button — halts the entire agent loop immediately,
  for worst-case intervention.

---

## Agent Architecture

**1 primary agent** owns the full lifecycle. **2 focused subagents**, invoked
as tools by the primary agent (not a swarm/multi-orchestrator design):

- **A. Semantic codebase reviewer** — called after each phase/subtask completes.
- **B. End-to-end run verifier** — uses computer-use to actually drive the UI
  and validate real flows (Daytona supports this natively now — wasn't
  practically viable 13 months ago when kaptaan was built).

### Flow

1. User gives instructions in the single project conversation. Default mode:
   continue conversation / respond directly.
2. On explicit ask ("create a goal plan"), primary agent produces a
   **structured goal plan** with clear deliverables, phases/subtasks. Presented
   for **approval** before any work starts.
3. On approval: full autonomy kicks in (see Autonomy below).
   - Scratchpad-based planning, phases executed one by one.
   - Small, local commits within the sandbox as work progresses.
   - **Branch rule**: on sandbox init, always clone from `main`/`master` into
     a fresh branch named after the agent (e.g. `ada`). All work happens on
     this branch — agent-managed, never touches main directly.
   - On goal/task completion: push + create PR, all at once. Merge handled
     manually by the human afterward.
4. **Context management**: when context hits ~80% and a natural checkpoint
   arrives (e.g. "finished auth"), agent calls a **compress tool**:
   - Full conversation + tool-call history goes to a separate LLM call for summarization.
   - Conversation is cleared; the **summary is appended as the first message**;
     loop continues from there.
   - Keep this dead simple — no separate "memory store" abstraction.
   - **Audit log ≠ conversation history.** Audit log (Turso, one table) is a
     lightweight record: costs, timestamps, token counts. It is not a
     full transcript graveyard. Full history is not preserved post-compaction.
5. **Reviewer loop**: after each phase/subtask, semantic reviewer agent is
   invoked. Primary agent has full authority to decide: fix genuine issues
   and continue the fix loop, recognize reviewer false positives and move on,
   or escalate to the **ask-human tool** if it's stuck. No hardcoded retry cap —
   trusted to self-manage; this failure mode wasn't observed as a practical
   problem in extensive internal use.
6. **E2E/computer-use verification**: invoked mindfully, not on every step.
   - **Mid-task**, if a change is UI-related *and* critical.
   - Otherwise, **once before final goal completion** — full end-to-end flow
     including UI, via computer-use.
   - Cost is acceptable here — this is the highest-value part of the loop
     (replaces manual QA hours).
7. **Ask-human tool**: surfaces as a normal message in the conversation. Loop
   pauses there; visibly waiting — no separate notification system needed.
8. On completion: primary agent presents the **public preview URL** (via
   Daytona port exposure) + a summary.

### Autonomy & Human Control

- Full autonomy after plan approval — no forced mid-flow checkpoints.
- Human oversight via **live UI streaming** (tool calls, diffs, progress) — can
  check in anytime.
- **Pause button** is the safety valve for worst-case intervention, not
  periodic approval gates.

---

## Daytona Specifics (researched, verify further during implementation)

- **Sandbox persistence**: one sandbox per project, permanent. Standard
  recovery path if state ever needs to be blown away: re-clone repo + re-run
  setup scripts. (13 months of internal Daytona use — this has not been a
  real-world problem.)
- **Preview URLs**: format is `https://{port}-{sandboxId}.{daytonaProxyDomain}`
  — tied to sandbox ID, so effectively **stable per project+port** as long as
  the sandbox isn't destroyed.
  - **Standard preview URL**: needs a token; token resets on every sandbox
    restart (must re-fetch link after restart).
  - **Signed preview URL**: token embedded in the URL itself, persists across
    restarts until it expires (default expiry is short — set explicitly,
    e.g. 1hr+ for real use). Better fit for "give user a working link."
  - Public, unauthenticated access requires sandbox `public: true`.
  - **This area is under-explored — never used this Daytona feature in 13
    months of kaptaan. Do deeper hands-on testing during implementation
    before finalizing token/expiry strategy.**

---

## Open Items (deliberately deferred to implementation)

- Exact goal-plan schema/format.
- PR merge policy/flow (human handles manually for now).
- Daytona signed-URL expiry strategy in practice.
- HTMX/UI component structure specifics.
- Exact audit log table schema (fields: cost, timestamp, token counts, model, project_id — refine as built).
