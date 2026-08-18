# Sandbox & Git

Daytona for the sandbox, one branch for git. Details below were verified against
Daytona's current docs ([Sandboxes](https://www.daytona.io/docs/en/sandboxes/),
[Computer Use](https://www.daytona.io/docs/en/computer-use/),
[VNC Access](https://www.daytona.io/docs/en/vnc-access/)) — anything marked
**unverified** still needs hands-on testing.

---

## One Sandbox, Shared

**The project and computer-use run on the same sandbox. Always.** One sandbox
per project, created with the project, persistent for its lifetime.

This is a requirement, and it has a non-obvious consequence worth stating up
front, because getting it wrong would waste a lot of time:

> Daytona's VNC and computer-use support ships **only in its default sandbox
> image**. Sandboxes built from a custom image have no VNC unless you install
> the desktop stack yourself.

So: **no custom Docker image for the sandbox.** The sandbox is created from a
stock Daytona snapshot, and project dependencies are installed at runtime via
the project's setup script (stored in the `project` table). A custom image would
look like the tidier choice and would silently break the E2E verifier — the most
valuable part of the loop.

If a custom image ever becomes necessary, these packages must be installed for
computer-use to work: `xvfb`, `xfce4`, `xfce4-terminal`, `x11vnc`, `novnc`,
`dbus-x11`, plus `libx11-6`, `libxrandr2`, `libxext6`, `libxrender1`,
`libxfixes3`, `libxss1`, `libxtst6`, `libxi6`.

**Linked sandboxes are not an option** even though they're co-located and share
a network — Daytona requires them to be ephemeral with auto-delete of exactly 0.
That's disqualifying for a persistent project sandbox.

---

## Size

**2 vCPU / 4 GiB.** This maps exactly onto Daytona's stock **`daytona-medium`**
snapshot (2 vCPU / 4 GiB RAM / 8 GiB disk), so the requirement is satisfied by a
default snapshot — no custom resource request, and the default image (and
therefore computer-use) comes with it.

For reference, Daytona's stock container snapshots and limits:

| Snapshot | vCPU | Memory | Disk |
|---|---|---|---|
| `daytona-small` | 1 | 1 GiB | 3 GiB |
| **`daytona-medium`** | **2** | **4 GiB** | **8 GiB** |
| `daytona-large` | 4 | 8 GiB | 10 GiB |

Bare defaults are 1 vCPU / 1 GiB / 3 GiB, and the per-organization ceiling is
4 vCPU / 8 GiB / 10 GiB — so `daytona-large` is the maximum available and there
is headroom to resize into if 2/4 proves tight.

**Resizing:** CPU and memory can be increased on a *running* sandbox. Decreasing
either, or increasing disk, requires stopping first. Disk can only grow, never
shrink.

**Verifying limits from inside the sandbox:** `nproc`, `free`, `top`, and
`/proc/*` report *host* values, not sandbox limits. The agent must read cgroups
instead:

```
cat /sys/fs/cgroup/cpu.max      # "<quota> <period>"; cores = quota / period
cat /sys/fs/cgroup/memory.max   # bytes
df -h /                         # disk
```

Worth baking into the system prompt — otherwise the agent will confidently
misreport available resources and size builds wrongly.

---

## Lifecycle

Daytona's inactivity handling is the single biggest operational trap here, so
these values are locked explicitly rather than left to defaults.

| Setting | Value | Why |
|---|---|---|
| `auto_stop_interval` | `30` (minutes) | Stop paying while idle |
| `auto_delete_interval` | `-1` (never) | **Critical.** A deleted sandbox loses the working tree |
| `auto_archive_interval` | `0` (max, 30 days) | Delay archiving; archived sandboxes are slower to start |
| `ttl_minutes` | `0` (disabled) | Wall-clock TTL destroys sandboxes *in any state* |

### The auto-stop trap

Daytona's inactivity timer is reset only by **external** interaction: lifecycle
changes, network requests through sandbox previews, SSH connections, and Toolbox
SDK calls. It is explicitly **not** reset by background scripts, long-running
processes, or work happening inside the sandbox.

A long build, a test suite, or an agent thinking for 20 minutes therefore counts
as *idle*, and the sandbox can be stopped mid-run.

**Locked mitigation:** while a run is active, the loop calls
`sandbox.refresh_activity()` on a **5-minute heartbeat**, written alongside
`runs.heartbeat_at`. Auto-stop stays at 30 minutes so an idle sandbox still
shuts down.

The alternative — `auto_stop_interval = 0` for an always-on sandbox — was
rejected as needlessly expensive for a tool used opportunistically from a phone.

### Wake on demand

Container sandboxes support stop/start (filesystem preserved, memory not) but
**not** pause/resume or fork. So:

1. Any user message or run start checks sandbox state first.
2. If `stopped` or `archived`, call `start()` and stream a "waking sandbox"
   status to the UI. Archived sandboxes restore from object storage and take
   longer.
3. If `error`, check `recoverable` and call `recover()`.
4. Only then proceed with the run.

Because memory state is lost on stop, **nothing may be assumed to be running**
after a wake — dev servers must be restarted from the project's dev command.

---

## Computer Use

`computer_use.start()` launches Xvfb, xfce4, x11vnc, and noVNC **inside** the
sandbox. These are processes, not state — they do **not** survive a sandbox
stop.

**Locked rule:** every computer-use operation goes through an
`ensureComputerUse()` helper that checks `get_status()` and starts the processes
if needed. Never assume they're up because they were up an hour ago.

### Resolution

`VNC_RESOLUTION` is set as an env var **at sandbox creation only**. The X
framebuffer is allocated when the X server starts, so the resolution is
immutable — restarting display processes or stopping/starting the sandbox keeps
the original geometry. Changing it requires creating a new sandbox.

**Locked: `1280x800`**, recorded in `project.vnc_resolution`.

This matters more than it looks: agents that emit normalized coordinates scale
them by an assumed screen size, so a mismatch between the real framebuffer and
the model's assumption displaces *every single click*. The E2E verifier must
read actual geometry via `display.get_info()` at the start of a session and use
those numbers, rather than trusting the configured value.

### Driving the UI

Available primitives: mouse (click, move, drag, scroll, position), keyboard
(type, press, hotkey), screenshot (full, region, compressed, compressed region),
display (info, window list), recording (start/stop/list/get/delete/download),
and accessibility — an AT-SPI tree with `get_tree`, `find_nodes`, `focus_node`,
`invoke_node`, `set_node_value`.

**Preference order for the E2E verifier: accessibility tree first, pixel
coordinates as fallback.** Finding a button by role and accessible name and
invoking it is dramatically more reliable than guessing coordinates from a
screenshot, and it doesn't burn vision tokens on every step. *Unverified in
practice — validate early, since it changes the verifier's whole design.*

**Screenshots must be compressed** before going to the model — full-resolution
PNGs at 1280x800 are expensive per step, and E2E is already the costliest part
of the loop. Use JPEG with quality around 80, scaled down where the detail isn't
needed.

**Recordings:** start a recording for each E2E verification run and upload it to
R2 on completion, tracked in the `artifacts` table. When a verification fails,
the recording is by far the fastest way to see why. Recordings default to
`~/.daytona/recordings` and the directory is configurable at creation via
`DAYTONA_RECORDINGS_DIR` — but they live on the sandbox filesystem, so they must
be moved to R2 to survive a reset.

### Go SDK gaps

Daytona's docs show language tabs per method, and **several computer-use methods
have no Go tab**:

- `screenshot.take_compressed` and `take_compressed_region`
- `get_process_status`, `restart_process`, `get_process_logs`,
  `get_process_errors`

The compressed-screenshot gap is the painful one, since compression is exactly
what keeps E2E affordable. Every one of these is documented as available via the
REST API.

**Locked approach:** wrap Daytona access in a thin internal client. Use the Go
SDK where it covers the surface, and call the REST API directly for the gaps,
behind the same interface. Verify the actual state of the Go SDK before writing
the fallbacks — the docs may simply be lagging the SDK.

---

## Preview URLs

Format: `https://{port}-{sandboxId}.{daytonaProxyDomain}` — tied to the sandbox
ID, so effectively stable per project + port for as long as the sandbox lives.

Two variants:

- **Standard** — requires a token that resets on every sandbox restart, so the
  link must be re-fetched after each wake.
- **Signed** — token embedded in the URL, survives restarts until expiry. The
  default expiry is short and should be set explicitly.

**Locked: signed URLs**, since the whole point is handing myself a working link
from a phone, and a link that dies on restart defeats that. Expiry is set
explicitly and the URL is re-issued on demand rather than cached indefinitely.

Fully public access additionally requires the sandbox to be created with
`public: true`.

**This area is under-explored** — it was never used across 13 months of prior
Daytona use. Expiry strategy and whether `public: true` is wanted at all need
hands-on testing before finalizing.

---

## Git Strategy

### One branch: `doot`

**Strictly one branch, always named `doot`.** No naming derived from task,
plan, date, ticket, or agent. `main`/`master` is never committed to.

On sandbox init:

```
git clone <repo_url> .
git checkout -B doot
git config user.name  doot
git config user.email doot@local
```

- Small local commits as each subtask completes; the SHA is recorded in
  `plan_tasks.commit_sha`.
- On goal completion, push to `origin doot`.
- Since `doot` is exclusively agent-owned and no human ever commits to it,
  pushes use **`--force-with-lease`**. This keeps history clean after rebases
  without risking someone else's work — there is no someone else.
- `plan_tasks.commit_sha` plus the `pushes` table mean the branch is
  reconstructible even if the sandbox is reset.

### After I merge

When a PR is merged and the base branch moves ahead, `doot` is stale. On the
next run start:

1. `git fetch origin`
2. If `origin/<base>` has moved and the working tree is clean, rebase `doot`
   onto it.
3. On conflict, **stop and ask** via the ask-human tool. Never auto-resolve
   conflicts — a silently botched rebase is much worse than a paused run.

### Pull requests — best-effort, never blocking

PR creation is **optional by design**. I merge at my own pace and am happy to
open PRs myself.

- Attempt `POST /repos/{owner}/{repo}/pulls` after a successful push.
- Success → record `pr_number` and `pr_url`, post the link in the conversation.
- Already exists (GitHub returns 422) → record `pr_status = 'exists'` and reuse
  the existing PR. This is the *normal* case for the second and later pushes to
  the same branch.
- Any other failure → record `pr_status = 'failed'` with the error, post the
  branch link instead, and **continue**. A run is never marked failed because
  PR creation failed.
- `github.create_pr = 0` in settings → `pr_status = 'skipped'`, push only.

The agent always reports the branch URL regardless of PR outcome, so there is
always a way to review the work.

---

## Sandbox Reset

Available on the Project screen, individually confirmed, and completely separate
from Clear Conversation.

Reset means: delete the sandbox, create a new one from `daytona-medium`, clone
the repo, recreate the `doot` branch, and re-run the setup script.

**Uncommitted work in the old sandbox is lost.** That's acceptable because
commits happen per subtask and pushes happen per goal — so the exposure is at
most one subtask. Recovery has never been a real problem across 13 months of
prior Daytona use.

Conversation history is untouched by a reset.
