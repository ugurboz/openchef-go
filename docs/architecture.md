# OpenChef Architecture

| | |
|---|---|
| **Status** | Accepted (v0.6: Claude hardening — `--restricted`, decisions D-006…D-011) |
| **Scope** | v1.0 (MVP) and the contracts later versions must keep |
| **Audience** | Maintainers and contributors |

This document is the source of truth for how OpenChef is built. Code that contradicts it
is a bug in the code or a missing amendment here. Amendments go through pull requests.

---

## 1. What OpenChef is

OpenChef is a local orchestrator for AI coding agents. It drives the agent CLIs a
developer already has installed and logged in to — **Claude Code**, **Codex CLI** and
**Antigravity (`agy`)** in v1 — and turns them into one workflow:

1. **Plan:** a strong model breaks a request into small, verifiable steps.
2. **Approve:** the human approves the plan.
3. **Implement:** each step goes to the best available agent, in an isolated git worktree.
4. **Verify:** OpenChef itself runs the step's verification command. Agent self-reports
   are never trusted.
5. **Review:** an agent from a *different* provider reviews the result.

When a subscription runs out of quota mid-step, OpenChef saves the partial work, moves the
step to an agent on another subscription, and continues. When every subscription is
exhausted, the task waits and resumes automatically after the earliest reset.

### 1.1 Goals

- **G1 — Correct before clever.** Every step ends in a state the kernel verified. No
  silent failure, no lost work, no orphaned process.
- **G2 — Familiar.** Anyone who uses Claude Code or Codex should feel at home: same
  permission modes and rule syntax, `~/.<tool>` config home, slash commands in the TUI,
  git branches instead of custom storage.
- **G3 — Safe by default.** The user's working tree is never modified by an agent.
  All agent work happens in worktrees and reaches the user's branch only through an
  explicit merge.
- **G4 — Subscription-first.** Use the plans people already pay for (Claude Pro/Max,
  ChatGPT Plus/Pro, Google AI Pro). API keys are supported but never used by accident.
- **G5 — Portable.** One static binary for macOS, Linux and Windows.
- **G6 — Walk away.** A user can approve a plan, leave the machine, and come back to a
  finished, verified result plus a report of everything that needs their attention (§21).

### 1.2 Non-goals (v1)

- Being an agent. OpenChef does not call model APIs itself and has no tool loop of its own.
- Replacing the agent CLIs' own UIs. Users can still open any session in its native CLI.
- Running agents concurrently in one shared working tree (the prototype's `shared` mode).
- Remote/cloud execution, multi-user servers, web UI.
- OpenCode support (planned for v1.1; findings are already in `docs/research`).

---

## 2. Principles

1. **The kernel decides.** Success is the exit code of a verification command run by
   OpenChef, never an agent's claim or a CLI's exit status (see `agy` findings).
2. **One owner per process.** Every agent process is a child of the daemon and dies with
   it. Recovery uses each CLI's native session resume, not process adoption.
3. **One session per attempt chain.** Retries of a step are new *turns* in the same agent
   session so the agent keeps its context. A new session is created only on failover.
4. **State is durable before it is acted on.** The task store (SQLite) is written before
   any side effect (spawning, committing, merging), so a crash at any point is resumable.
5. **Fail loudly at the boundary.** Adapters verify CLI versions and protocol shapes at
   startup and on every message they parse. Unknown shapes are errors, not guesses.
6. **Explicit over implicit.** No step commits files it did not change; no git command
   uses `add -A` on the user's tree; no mode is chosen silently.

---

## 3. System overview

```
 ┌──────────────┐  ┌──────────────┐
 │ openchef CLI │  │ openchef TUI │          clients (stateless)
 └──────┬───────┘  └──────┬───────┘
        └────── JSON-RPC 2.0 over local socket ───────┐
                                                      ▼
 ┌─────────────────────────────────────────────────────────────────────┐
 │ openchef daemon                                                      │
 │                                                                      │
 │  API ── Engine ── Router ── Quota ── Permissions ── Git workspace    │
 │           │                                                         │
 │           └── Store (SQLite, WAL)       Event log (JSONL per session)│
 │                                                                      │
 │  Runtime adapters:  claude │ codex │ agy                             │
 └───────────┬───────────────────┬───────────────┬──────────────────────┘
             │ stdio pipes       │ stdio pipes   │ stdio pipes
       ┌─────▼──────┐     ┌──────▼──────┐  ┌─────▼──────┐
       │ claude -p  │     │ codex       │  │ agy acp    │   agent processes
       │ stream-json│     │ app-server  │  │ (ACP)      │   (children of daemon)
       └────────────┘     └─────────────┘  └────────────┘
```

- **One binary.** `openchef` is the CLI, the TUI (`openchef` with no arguments, like
  `claude` and `codex`) and the daemon (`openchef daemon`). The CLI starts the daemon on
  demand if it is not running.
- **Why a daemon.** Tasks take hours, outlive terminal windows, wait for quota resets and
  share pool accounting. A client-only design cannot own those lifecycles.
- **Why the TUI is only a client.** Closing the TUI must never stop work.

---

## 4. Domain model

| Term | Meaning |
|---|---|
| **Provider** | A company/CLI family: `anthropic` (Claude Code), `openai` (Codex), `google` (agy). |
| **Pool** | One quota bucket the user owns, e.g. `claude-max`, `chatgpt-plus`, `google-ai-pro`. Quota is tracked per pool. |
| **Agent** | A named (runtime, model, pool) triple, e.g. `opus = claude / claude-opus-5-5 / claude-max`. Several agents can share one pool. |
| **Task** | One user request. Has a plan, a workspace and a final status. |
| **Step** | One unit of the plan with a prompt, optional file scope and a verification command. Max 7 per task (keeps plans reviewable). |
| **Attempt** | One agent working on one step inside one session. A failover starts a new attempt. |
| **Session** | One conversation inside one agent CLI, identified by the CLI's own id (Claude session id, Codex thread id, ACP session id). |
| **Turn** | One user message to a session and everything until the CLI says the turn is complete. |

Failover always moves to a **different pool** (moving to another model on the same
exhausted pool is pointless).

---

## 5. Process model and lifecycle

### 5.1 Ownership

- Every agent process is started by the daemon with stdin/stdout **pipes** and recorded in
  the store (PID, process start time, session id) *before* the first message is sent.
- Agent processes are placed in their own process group (Unix) or **Job Object** with
  `KILL_ON_JOB_CLOSE` (Windows) so the daemon can stop the whole tree.
- On Linux the child also gets `Pdeathsig = SIGTERM`.
- On macOS there is no parent-death signal. Children react to EOF on their stdin pipe
  (verified in M1): `codex app-server` and `agy acp` exit within ~50 ms even mid-turn;
  `claude -p` finishes the turn in flight and then exits. As a second line of defence, the daemon kills any recorded process group still
  alive from a previous daemon instance at startup (PID + start time must both match).

### 5.2 Why not detach-and-adopt

The Python prototype kept agents alive across daemon crashes (FIFO with `O_RDWR`, new
session groups, byte-offset log tailing, PID adoption). It worked in spikes and failed in
practice: processes leaked, stale output was replayed into new attempts, and adoption
gave a running process back to a daemon that no longer had the task loop driving it.

All three v1 runtimes support native resume. Losing at most the in-flight turn on a daemon
crash is an acceptable price for a lifecycle that cannot leak.

### 5.3 Recovery after a daemon restart

At startup the daemon:

1. Takes the single-instance lock (`~/.openchef/daemon.lock`).
2. Kills leftover process groups recorded as running (§5.1).
3. For every task in a non-terminal state, rebuilds the attempt and calls
   `Runtime.Resume(sessionRef)`. The interrupted turn is retried with a recovery note:
   *"The orchestrator restarted during your last turn. Check the workspace state and
   continue Step N."*
4. Tasks in `waiting_quota` are rescheduled from their stored `resume_at`.

### 5.4 Shutdown

- `openchef daemon stop` (default): stop accepting work, send each running session a
  graceful interrupt, wait up to 10 s, then terminate process groups. Tasks stay in their
  state and resume on next start.
- The TUI and CLI exiting never stop the daemon.

---

## 6. Runtime adapters

### 6.1 Contract

```go
// Runtime starts and resumes sessions for one agent CLI.
type Runtime interface {
    Name() string                                   // "claude", "codex", "agy"
    Probe(ctx context.Context) (RuntimeInfo, error) // version, auth state, capabilities
    Open(ctx context.Context, spec SessionSpec) (Session, error)
    Resume(ctx context.Context, ref SessionRef, spec SessionSpec) (Session, error)
}

// Session is one live conversation. It is not safe for concurrent turns.
type Session interface {
    Ref() SessionRef                         // CLI-native id, persisted by the engine
    // Turn sends one user message and streams events until the turn completes.
    // The channel is closed after exactly one terminal event (TurnCompleted or TurnFailed).
    Turn(ctx context.Context, msg string) (<-chan Event, error)
    Interrupt(ctx context.Context) error     // cancel the current turn, keep the session
    Close(ctx context.Context) error         // end the process; idempotent
}
```

`SessionSpec` carries the agent (model), working directory, permission mode and rules,
environment policy, the approval callback (§9) and the event log path.

Event types (normalized across runtimes): `Text`, `ToolCall`, `ToolResult`,
`ApprovalRequested`, `QuestionAsked`, `Usage`, `QuotaSignal`, `TurnCompleted`,
`TurnFailed{Kind}` where `Kind` is one of `quota_exhausted`, `rate_limited`, `auth`,
`context_overflow`, `crashed`, `timeout`, `interrupted`, `protocol`, `unknown`.

Rules every adapter must follow:

- `Turn` returns events of **this turn only**. Nothing from previous turns or previous
  processes may be replayed.
- Usage is reported once per turn from the CLI's turn-level totals, never by summing
  message-level counters as well.
- The final assistant text is taken from one source (the turn result), never by
  concatenating streamed text and the result.
- Every raw line received from the CLI is appended to the session's event log before it is
  parsed, so protocol bugs can be diagnosed and replayed in tests.
- `Close` is always called (the engine uses `defer`); the adapter must be leak-free even
  when the context is cancelled mid-turn.

### 6.2 Claude Code (`claude`)

- **Transport:** `claude -p --input-format stream-json --output-format stream-json
  --verbose` over stdin/stdout pipes. One long-lived process per session.
- **Identity:** OpenChef generates the UUID and passes `--session-id`; `Resume` uses
  `--resume <id>` in a new process.
- **Turn end:** the `result` event. Its `is_error`, `subtype` and `usage` define the
  terminal event.
- **Permissions** (M1 findings §3):
  - The permission mode is passed natively (`--permission-mode`).
  - **Deny rules are passed natively** (`--settings '{"permissions":{"deny":[…]}}'`, same
    syntax as OpenChef's). In `acceptEdits`, `auto` and `bypassPermissions` Claude never
    consults the host, so only native deny rules are enforced there — and they are, in all
    three modes.
  - Decisions that would prompt reach OpenChef through the Agent SDK control protocol:
    `--permission-prompt-tool stdio` plus an `initialize` control request; requests arrive
    as `control_request`/`can_use_tool` (with `description`, `blocked_path`,
    `permission_suggestions`) and are answered with `control_response`. The MCP
    `--permission-prompt-tool mcp__openchef__approve` path is the fallback.
  - `AskUserQuestion` is disabled and replaced by OpenChef's `ask_user` MCP tool, which is
    auto-allowed.
  - Planner and reviewer sessions run in `plan` mode and OpenChef denies `ExitPlanMode`,
    so they cannot leave read-only mode.
- **Usage and results:** only `result.usage` counts (per-message usage sums to exactly
  twice the turn total). `result.structured_output` carries `--json-schema` output;
  `result.permission_denials` and `total_cost_usd` feed the report.
- **Isolation (D-006):** always `--restricted` (no settings files, hence no repository
  hooks; file tools confined to the working directory; writes to settings/git/tool-config
  files always reach the host) with an explicit `--tools` allowlist and
  `--strict-mcp-config`. Web tools are off unless the agent enables them. OpenChef's MCP
  server exposes only `ask_user`.
- **Turn correlation (D-009, D-012):** one `result` per user message; a turn ends on the
  result whose per-process `result_index` matches it. Interrupt waits for the owed
  result; if the message was still queued, or no result comes, the process is replaced.
  Close kills fast when a turn is in flight (D-010). A per-turn output schema re-spawns
  the process with `--json-schema` between turns (D-011); replacements use `--resume`
  once the session has a transcript and `--session-id` before (D-013).
- **Quota:** L1 from `rate_limit_event` (fractions, epoch resets); L2 probe (§10).

### 6.3 Codex CLI (`codex`)

- **Transport: `codex app-server`** (newline-delimited JSON-RPC over stdio), adopted
  after the M1 spike (M1 findings §1). It provides live approvals
  (`item/commandExecution/requestApproval`, `item/fileChange/requestApproval`, answered
  `accept`/`acceptForSession`/`decline`/`cancel`), turn lifecycle (`turn/completed` with
  `status`), `turn/interrupt`, per-turn `effort` and `outputSchema`, `thread/resume` after
  a crash, typed turn errors (`codexErrorInfo`: `usageLimitExceeded`,
  `rateLimitExceeded`, `unauthorized`, …) and first-party rate limits
  (`account/rateLimits/read`, `account/rateLimits/updated`).
- **Questions:** Codex's own `item/tool/requestUserInput` is unavailable in the default
  collaboration mode, so OpenChef's `ask_user` MCP server is attached per thread through
  `thread/start.config.mcp_servers`. Codex asks before each MCP tool call
  (`mcpServer/elicitation/request`, `codex_approval_kind: "mcp_tool_call"`); the approval
  service auto-accepts it for OpenChef's own tool and evaluates any other MCP tool.
- The app-server protocol is marked **experimental** upstream. The adapter pins the
  protocol shapes it uses (generated with `codex app-server generate-json-schema`, stored
  under `internal/runtime/codex/schema/`) and refuses to run on a mismatch.
- **Fallback transport: `codex exec --json` + `codex exec resume --json <thread_id>`**,
  one process per turn, verified in Phase 0. It has no live approvals, so it only runs in
  modes where no prompt is needed (`plan` → `--sandbox read-only`, `acceptEdits`/`auto`
  → `--sandbox workspace-write`). It is used only when the app-server handshake fails,
  and `openchef doctor` reports it.

### 6.4 Antigravity (`agy`)

- **Transport:** `agy acp` (Agent Client Protocol, JSON-RPC over stdio), never `agy -p`,
  which runs sandboxed commands without asking and reports success on failure.
- **Flags:** `--sandbox --disable-telemetry --include-default-customizations=false
  --cli_dir_name openchef`. OpenChef owns `settings.json` in that directory
  (`toolPermission: proceed-in-sandbox`, `enableTerminalSandbox: true`); the user's agy
  configuration is never read.
- **Session:** `session/new` (with OpenChef's `ask_user` MCP server), then
  `session/set_config_option` for `model`, `thought_level` (effort, options per model)
  and `mode` (always `default`, D-015); `session/prompt` per turn; `session/load` for
  `Resume`, dropping the replayed history (D-016); `session/cancel` to interrupt.
- **Permissions (D-015):** agy ignores native deny rules and its `auto`/`accept-edits`
  modes bypass the host, so every OpenChef mode is applied by the approver. Commands run
  in the sandbox unasked only while they read the workspace (no writes, network or
  outside reads); anything else escapes with `BypassSandbox` and reaches
  `session/request_permission`, answered by option `kind`. File edits always ask.
- **Structured output (D-017):** schema appended to the prompt, the answer extracted and
  validated, one repair prompt; the final answer is the text after the last tool call.
  Turn end is the `session/prompt` response, after a barrier on preceding updates.
- **Quota:** no L1 or L2 source. L3 estimation only (§10).

---

## 7. Engine

### 7.1 Task state machine

```
          ┌──────────┐ plan ok ┌───────────────────┐ approved ┌─────────┐
 new ────►│ planning ├────────►│ awaiting_approval ├─────────►│ running │
          └────┬─────┘         └─────────┬─────────┘          └──┬──┬───┘
               │ fail                    │ rejected              │  │ all steps verified
               ▼                         ▼                       │  ▼
            failed                   cancelled                   │ reviewing ──► done
                                                                 │
            waiting_quota ◄──── every eligible pool exhausted ───┤
            (auto-resumes at earliest reset)                     │
            needs_attention ◄── retries exhausted / unrecoverable ┘
```

`run` (single step, no plan) enters `running` directly. `done`, `failed` and `cancelled`
are terminal. `needs_attention` waits for the user (`openchef retry`, `openchef cancel`,
or opening the session in its native CLI).

### 7.2 Step execution

```
for each step not yet verified:
    attempt := open session on routed agent (new pool)          // persisted first
    msg := step prompt (+ handoff note if this is a failover)
    loop:
        outcome := run one turn (timeouts, loop detection)
        switch outcome:
        case completed:
            ok, output := run verification command (kernel, with timeout)
            if ok:  commit step in workspace; mark verified; break
            if retries left: msg = verification failure + output; continue  // same session
            else: needs_attention
        case rate_limited:   backoff, same message, same session (bounded)
        case quota_exhausted:
            capture partial work as patch; reset workspace to last verified commit
            cool down pool until its reset; route to an agent on another pool
            attempt := new session; msg = handoff prompt; continue
            (no eligible pool → waiting_quota)
        case crashed/protocol: resume session once; then treat as failure
        case auth: needs_attention (user must log in again)
    close session (always)
```

- **Retry message** always restates the step (title, prompt, files, verify command) and
  appends the failure output, so even a resumed or fresh session has full context.
- **Limits** (configurable): 2 retries per step, 30 min per turn, 10 min idle (no event),
  5 identical consecutive tool calls = loop → interrupt and retry with a note.
- **Verification** runs in the workspace with a minimal environment and a timeout; output
  is truncated (head and tail kept) before it is fed back.
- **Handoff prompt** contains the step, the last verified commit, the path of the saved
  patch (`.openchef/patches/step-N-<agent>.patch`, excluded from git) and an optional
  summary written by a cheap helper agent from a different pool.

### 7.3 Routing

Per phase (`plan`, `implement`, `review`, `helper`) the config lists the agents allowed
for that phase. The router first filters them: pool healthy (§10.3), not cooling down,
not excluded for this attempt, under its concurrency limit, and of a tier the step's
importance permits (§7.4). It then picks among the remaining agents according to the
selection strategy (§7.4). `review` prefers a provider different from the step's author.
A user can pin an agent for a task (`--agent opus`) or
for the TUI session (`/agent opus`), which disables routing but not failover (failover
asks first when an agent is pinned).

### 7.4 Model selection: weights, importance and effort

Three independent knobs decide *which* model does a step and *how hard* it thinks. Each
has an automatic default and a user override.

**Tier (per agent).** Every agent declares a capability tier: `frontier` (e.g. Opus,
the strongest GPT, Gemini Pro-class), `standard`, or `fast` (Haiku, Flash, free models).
`openchef init` proposes tiers for known models; users can change them.

**Importance (per task and per step).** `low | normal | high | critical`.

- *Automatic:* the planner assigns each step an importance with a one-line reason.
  OpenChef then raises it to at least `high` for steps whose `files` match the
  configured sensitive patterns (defaults: authentication, crypto, payments, database
  migrations, CI/CD and infrastructure config, dependency manifests).
- *User:* `--importance` sets a floor for the whole task; any step's importance can be
  changed during plan approval.

Default effect of importance (configurable in `[importance.<level>]`). Effort here is
the `balanced` profile; see the effort profiles below.

| Importance | Allowed tiers | Effort (balanced) | Review | Extra checks |
|---|---|---|---|---|
| `low` | fast, standard | low | none | — |
| `normal` | standard, frontier | medium | 1 reviewer, other provider | — |
| `high` | frontier | high | 1 reviewer, other provider | plan approval warns if the step has no verify command |
| `critical` | frontier | high | 1 frontier reviewer, other provider (a second reviewer is opt-in: `reviewers = 2`) | same warning; highlighted in plan approval and in the report |

If no allowed agent is available (all exhausted), the step waits (`waiting_quota`); it is
**never silently downgraded** to a lower tier. The user can allow a downgrade per task
(`--allow-downgrade`).

**Effort.** One normalized scale — `low | medium | high | max` — mapped by each adapter
to its CLI: Claude `--effort` (`low…max`), Codex the reasoning-effort values the model
advertises (`model/list`), agy the model variant (`…-low|medium|high`). When a value is
not supported the adapter uses the nearest lower one and reports it.

Importance and effort are deliberately separate: importance describes the *work* (how
much a mistake would hurt) and never changes with quota; effort describes how many tokens
the model may spend thinking, and that is what OpenChef adapts. Higher effort costs many
more tokens, and most users are on entry plans (Claude Pro, ChatGPT Plus) where that
matters. Effort is resolved in three steps:

1. **Base** from importance (table above).
2. **Profile** shift, per pool or globally:

   | Profile | Shift | Intended for |
   |---|---|---|
   | `minimal` | −2 | Tight quota; cheapest possible runs |
   | `economy` | −1 | Entry plans (Free, Claude Pro, ChatGPT Plus) |
   | `balanced` | 0 | Large plans (Max, Ultra) and budgeted API pools |
   | `quality` | +1 | Users who prefer quality over quota |
   | `auto` (default) | plan-based start, then adaptive | Everyone who does not want to think about it |

   `auto` starts at `economy` on entry plans and `balanced` on large ones (from the plan
   tier recorded by `openchef init`), then adapts continuously to quota (step 3).
3. **Quota adaptation** (only in `auto`): the pool's state moves effort one level:
   - **pressure** — pool `yellow` or `red` (§10.3): −1, so more of the remaining work fits
     before the reset;
   - **surplus** — pool `green` and the quota model projects that at least 40 % of the
     current window will go unused at its reset: +1, so paid capacity is not wasted.

The result is clamped to `low…max`. `max` is reached automatically only through
`quality` or a surplus on a pool whose start is `balanced`; on entry plans automatic
effort never exceeds `high`. An explicit pin (agent, task or `--effort`) bypasses steps 2
and 3. Every effective effort, and why it was chosen, is visible in plan approval,
`openchef status` and the report.

| Importance | minimal | economy | balanced | quality |
|---|---|---|---|---|
| `low` | low | low | low | medium |
| `normal` | low | low | medium | high |
| `high` | low | medium | high | max |
| `critical` | low | medium | high | max |

In the sandbox backend (§21.3) `critical` gets no special handling beyond `high`: the
container is the safety boundary, so the extra warnings and highlighting are dropped.

**Selection strategy (per phase).** How the router chooses among the filtered agents:

- `auto` (default): spend the subscriptions the user already pays for evenly, before
  their windows reset. Each candidate gets a score from its pool's remaining headroom,
  how soon that headroom resets ("use it or lose it"), recent failure rate, and cost
  (API pools rank below subscriptions). The highest score wins; ties break by config
  order.
- `weighted`: the user sets target shares, e.g. 50 % Opus, 30 % GPT, 20 % Gemini. The
  router uses smooth weighted round-robin (deterministic, exact shares over time, no
  randomness). Shares of unavailable agents are redistributed proportionally among the
  available ones; shares never override tier or health filters.
- `ordered`: the first available agent in the list (the simple fallback chain).

```toml
[agents.opus]
runtime = "claude"
model   = "claude-opus-5-5"
pool    = "claude-max"
tier    = "frontier"
# effort = "high"   # optional: pin effort for this agent

[routing.implement]
strategy = "weighted"            # auto | weighted | ordered
agents   = ["opus", "gpt", "gemini"]
weights  = { opus = 50, gpt = 30, gemini = 20 }

[pools.claude-pro]
effort_profile = "auto"          # auto | minimal | economy | balanced | quality
plan_tier      = "pro"           # recorded by init; drives the auto start level

[importance]
sensitive = ["**/auth/**", "**/migrations/**", ".github/workflows/**"]
```

Weight counters are persisted in the store so shares hold across daemon restarts.
`openchef quota` and the report show the actual distribution next to the target. Users
override per run with `--agent <name>` (pin), `--strategy <s>`, `--importance <level>`,
`--effort <level>` and `--effort-profile <p>`, or in the TUI with `/agent`, `/strategy`,
`/importance`, `/effort`, `/effort-profile`.

### 7.5 Pipeline phases

- **Plan:** the planner runs in **`plan` permission mode** (read-only by construction,
  not by prompt). It must return JSON matching a published schema
  (`title`, `steps[]{title, prompt, files[], verify, size, importance, reason}`), requested through the CLI's
  structured-output option where available (Claude `--json-schema`, Codex
  `--output-schema` or its app-server equivalent) and validated strictly in every case. One repair turn is allowed on invalid output; then the task fails with the
  raw output attached.
- **Approve:** the plan is shown in the TUI/CLI with each step's importance, the agent
  and effort it will get, and the reason. The user can approve, reject or edit it
  (`openchef plan edit <task>` opens it in `$EDITOR`), including importance per step.
- **Review:** the reviewer runs in `plan` mode on the diff between the task's base commit
  and its head. Findings are stored and shown; v1 does not auto-fix review findings.

---

## 8. Workspaces and git

### 8.1 Modes

The user picks the mode per task (`--mode isolated|team`, default from config:
`isolated`). Independently, the task picks an execution backend: `local` (default,
worktrees on the host as described here) or `sandbox` (a container, §21.3).

- **`isolated`** — one git worktree per task on branch `openchef/<task-id>`, created from
  the user's current `HEAD`. All steps run sequentially in it. When the task is done the
  branch is left for the user, exactly like a pull request branch. `openchef merge <task>`
  fast-forwards or merges it into the user's branch on request.
- **`team`** — the task gets an integration branch `openchef/<task-id>`. Independent steps
  (steps whose `files` scopes do not overlap) run in parallel, each in its own worker
  worktree on `openchef/<task-id>/step-N`. A merge queue integrates finished steps one at
  a time: rebase onto the integration branch → run verification → fast-forward. A rebase
  conflict sends the step back to its agent with the conflict as the next turn message.

### 8.2 Rules

- Worktrees live under `~/.openchef/worktrees/<repo-id>/<task-id>[/step-N]`, outside the
  repository, so they never appear in the user's `git status`.
- The user's working tree and current branch are **never** written by OpenChef except by
  `openchef merge`, which refuses to run on a dirty tree.
- Step commits stage only the paths the agent changed (`git status --porcelain -z` in the
  worktree, minus ignored and `.openchef/` paths). Commits carry trailers
  `OpenChef-Task`, `OpenChef-Step`, `OpenChef-Agent`, `OpenChef-Pool`.
- Patch capture on failover uses `git add -A` + `git diff --cached --binary` **inside the
  worktree only**, then `git reset --hard` + `git clean -fd` there.
- Agents may not run git commands that rewrite history or move refs (`commit`, `reset`,
  `rebase`, `checkout`/`switch`, `push`, `merge`, `branch -D`, `tag`); OpenChef owns git.
  Read-only git commands are allowed.
- Dependency bootstrap for new worktrees is explicit per project config (e.g.
  `setup = ["npm ci"]`); OpenChef does not guess package managers.

---

## 9. Permissions

### 9.1 Same model as Claude Code

OpenChef uses Claude Code's permission vocabulary unchanged, so existing knowledge and
existing rules carry over.

**Modes:** `manual` (default; Claude Code's former `default`, accepted as an alias),
`acceptEdits`, `plan`, `auto`, `dontAsk`, `bypassPermissions`.

**Rules:** `allow`, `ask`, `deny` lists with Claude Code syntax, e.g.
`Bash(npm run test *)`, `Bash(git diff:*)`, `Read(./.env)`, `Edit(src/**)`,
`WebFetch(domain:github.com)`, `mcp__server__tool`. Precedence: **deny > ask > allow**;
deny rules can never be overridden by a mode, by session memory or by an agent.

**Files** (TOML, same keys as Claude Code's JSON settings):

```toml
# ~/.openchef/settings.toml         (user)
# <repo>/.openchef/settings.toml    (project, committed)
# <repo>/.openchef/settings.local.toml (project, git-ignored)
[permissions]
defaultMode = "manual"
allow = ["Bash(npm run test *)", "Bash(git diff:*)"]
ask   = ["Bash(git push:*)"]
deny  = ["Read(./.env)", "Read(./.env.*)", "Read(~/.ssh/**)"]
```

Optionally, `openchef init` offers to import the user's existing Claude Code permission
rules.

### 9.2 Evaluation

One evaluator in the daemon decides every request from every runtime:

1. Built-in invariants (cannot be disabled): no writes outside the workspace; no reads of
   OpenChef's own secrets; no history-rewriting git (§8.2).
2. `deny` rules → deny.
3. `ask` rules → ask.
4. `allow` rules and mode defaults → allow.
5. Otherwise → ask (or deny in `dontAsk`).

**Shell commands are parsed, not pattern-matched.** A command is parsed with a real POSIX
shell parser (`mvdan.cc/sh/v3/syntax`) into simple commands; `;`, `&&`, `||`, pipes,
subshells, command substitutions and redirections are all expanded. **Every** simple
command must be allowed for the whole command to be allowed; any denied one denies it.
Unparseable commands fall through to `ask`. (The prototype's `fnmatch("pytest*")` allowed
`pytest; curl … | sh`.)

Questions (`ask_user`) and approvals go to one queue, shown in the TUI, answerable from
the CLI (`openchef approve|deny|answer <id>`), with an OS notification when no TUI is
attached. Unanswered requests time out after 15 min as **deny**.

### 9.3 Mapping to runtimes

| OpenChef mode | Claude Code | Codex (app-server) | agy (ACP) |
|---|---|---|---|
| `manual` | native (`--restricted`); every prompt → evaluator | `untrusted` + `workspace-write`; approvals → evaluator | `default` + sandbox; edits and sandbox escapes → evaluator (D-015) |
| `acceptEdits` | native; host not consulted | file changes accepted, commands → evaluator | `default`; evaluator accepts edits (invariants apply), escapes → evaluator |
| `plan` | `dontAsk` + read-only tools `Read,Glob,Grep` (Claude's plan mode switches models, D-008) | `read-only` sandbox | `default`; evaluator denies every request (sandboxed reads only) |
| `auto` | native (Claude's classifier); host not consulted | evaluator with `acceptEdits` behaviour | `default`; evaluator with `acceptEdits` behaviour (agy `auto` bypasses the host) |
| `dontAsk` | native (prompts refused) | evaluator, ask → deny | `default`; evaluator, ask → deny |
| `bypassPermissions` | `acceptEdits` + approver allows all but deny rules/invariants (`--restricted` refuses bypass, D-008) | evaluator: invariants and deny rules only | `default`; evaluator: invariants and deny rules only |

**Deny rules and invariants are enforced in every mode.** For Claude, they are passed as
native deny rules because the host is not consulted outside `manual` (M1 findings §3);
for Codex and agy, the evaluator sees every request. Built-in invariants that cannot be
expressed as Claude rules (e.g. "no writes outside the workspace") rely on Claude's own
working-directory boundary in `acceptEdits`, which is why `bypassPermissions` is only
allowed in worktrees or the sandbox; in `auto` they rely on Claude's classifier. This is
a known limitation of driving Claude in non-`manual` modes, to be measured in M2 with
out-of-workspace write attempts and documented for users.

Where a runtime has no equivalent, OpenChef chooses the **stricter** behaviour and
documents it. `bypassPermissions` requires an explicit flag per task and is refused when
the task runs outside a worktree.

---

## 10. Quota

### 10.1 Sources

| Level | Claude | Codex | agy |
|---|---|---|---|
| **L1** in-stream | `rate_limit_event` | `account/rateLimits/updated` (app-server) | — |
| **L2** probe | `api/oauth/usage` (undocumented) | `account/rateLimits/read` (app-server); fallback `wham/usage` (undocumented) | — |
| **L3** estimate | per-step cost history | per-step cost history | per-step cost history |

All sources are normalized to utilization in `[0,1]` with an absolute reset time, per
window (5 h, weekly). Within one window utilization only increases; the newest observation
wins across sources.

### 10.2 L2 probe policy

L2 probes are **on by default**, by maintainer decision, because without them quota-aware
routing is guesswork for Claude. Because the Claude (and fallback ChatGPT) endpoints are
undocumented and read the CLI's stored OAuth token, the following are mandatory:

- Read-only token access (keychain / credential file), never refreshed, never logged,
  never stored, never sent anywhere except the provider's own host (host allowlist).
- Strict response validation of the fields used; any shape change disables that probe and
  surfaces an alert (`openchef doctor`).
- Backoff on 429, mute after repeated 5xx, minimum 30 s between manual refreshes.
- Kill switches: `openchef quota probes off`, config `quota.probes = false`, and a
  per-provider switch. `openchef init` explains the probes and asks before enabling them.
- The README documents the risk plainly: the endpoints may change or be disallowed by the
  provider's terms at any time, and use is at the user's own risk.

The Codex app-server's rate-limit methods are part of the CLI's own protocol and are
preferred over the undocumented ChatGPT endpoint.

### 10.3 Health and admission

- Pool health: `green` < 60 % ≤ `yellow` < 85 % ≤ `red` < 99 % ≤ `exhausted`, using the
  worse of the 5 h and weekly windows; stale data (> 15 min) degrades one level.
- `red` pools take only small steps; `exhausted` pools take nothing until reset.
- A step is admitted to a pool only if the pool's expected headroom covers the step's
  estimated cost (moving percentile of past costs by phase and size) with a 30 % margin.
- API-key pools have daily and monthly USD budgets instead of percentages.

---

## 11. Persistence

- **Store:** one SQLite database (`~/.openchef/state.db`, WAL, `modernc.org/sqlite`, pure
  Go). Migrations are versioned and forward-only.
- **Tables (v1):** `tasks`, `steps`, `attempts`, `sessions`, `turns`, `approvals`,
  `quota_observations`, `usage`, `schema_migrations`.
- **Event logs:** each session appends raw CLI lines and normalized events to
  `~/.openchef/sessions/<session-id>/{raw.jsonl,events.jsonl}`. Logs are for display,
  diagnosis and test fixtures; the store is the source of truth.
- **Write-ahead rule:** every state transition is committed before the side effect it
  authorizes (spawn, send, commit, merge). Side effects are idempotent or detectable on
  replay (e.g. a commit carries the step trailer, so a duplicate is detected).
- **Retention:** event logs of terminal tasks are pruned after 30 days (configurable).

---

## 12. Interfaces

### 12.1 CLI

Familiar verbs, consistent nouns. Every command supports `--json` for scripting.

```
openchef                          # open the TUI (like `claude`, `codex`)
openchef "add rate limiting"      # TUI with this request prefilled as a pipeline
openchef pipeline "<request>"     # plan → approve → implement → verify → review
openchef run "<prompt>" --verify "go test ./..."   # single step, no plan; streams live
openchef status [task]            # tasks and their steps
openchef logs <task|session> -f   # follow normalized events
openchef approve|deny <id> / openchef answer <id> "<text>"
openchef retry|cancel <task>
openchef merge <task>             # bring a finished branch into the current branch
openchef pipeline "<request>" --unattended [--sandbox]   # walk-away run (§21)
openchef export <task> [--patch | --to <dir>]          # results out of a sandbox
openchef deferred [approve|reject <id>]                # critical requests held back
openchef report <task>                                 # what happened while away
openchef attach <session>         # open the session in its native CLI (resume)
openchef quota [--refresh]        # pools, windows, resets
openchef init                     # detect installed CLIs, create ~/.openchef/config.toml
openchef doctor                   # versions, auth, protocol checks, probe health
openchef daemon start|stop|status
```

**Live by default (D-014).** `run` and `pipeline` without the TUI stay attached to the
task and stream its normalized events to the terminal as they happen: streamed text,
each tool call on one line (`● Bash  go test ./...`) with a short result summary,
approval requests, quota warnings, step and verification outcomes. `-q/--quiet` prints
only the final summary, `--detach` returns immediately (follow later with
`openchef logs <task> -f`), `--json` emits one event per line. Ctrl-C detaches the view;
it does not cancel the task (`openchef cancel` does). The renderer is the same one
`logs -f` uses, so both always show the same thing.

### 12.2 TUI (v1, second milestone)

Bubble Tea. Layout borrows from Claude Code and Codex rather than inventing one: a task
list, a focused task view with a readable event stream (tool calls collapsed, diffs
rendered), a bottom input with slash commands (`/agent`, `/mode`, `/permissions`,
`/quota`, `/help`), and approvals shown as a modal that must be focused to answer (no
global single-key approvals).

### 12.3 Daemon API

JSON-RPC 2.0 over a local socket: a Unix domain socket at `~/.openchef/daemon.sock`
(mode 0600) on macOS/Linux; on Windows an AF_UNIX socket in the same location with an ACL
restricted to the current user (named pipes are the fallback if AF_UNIX proves unreliable,
see §17). The API is versioned (`openchef.v1`) and streaming uses server notifications
(`task/updated`, `event/appended`, `approval/requested`). The API is internal in v1; it is
documented but not promised stable.

---

## 13. Configuration

- **Home:** `~/.openchef/` on every OS (same convention as `~/.claude` and `~/.codex`),
  overridable with `OPENCHEF_HOME`.
- **Files:** `config.toml` (providers, pools, agents, routing, quota, telemetry),
  `settings.toml` (permissions, as in §9), project-level `.openchef/settings.toml`.
- `openchef init` detects installed CLIs (`claude`, `codex`, `agy`), their versions and
  login state, asks which plan each account has, and writes pools and agents accordingly.
  Example profiles live in `docs/examples/`.

```toml
[pools.claude-max]
provider = "anthropic"
billing  = "subscription"
max_concurrent = 2

[agents.opus]
runtime = "claude"
model   = "claude-opus-5-5"
pool    = "claude-max"
tier    = "frontier"

[routing.implement]
strategy = "auto"
agents   = ["opus", "gpt", "gemini-opus"]
```

Model selection keys (`tier`, `strategy`, `weights`, `[importance]`) are described in §7.4.

- **Environment isolation:** subscription-backed sessions get `ANTHROPIC_API_KEY` /
  `OPENAI_API_KEY` / `GEMINI_API_KEY` removed from their environment so they cannot
  silently bill an API account. API pools must name their key variable explicitly.

---

## 14. Observability, privacy and telemetry

- Structured logs via `log/slog` to `~/.openchef/logs/daemon.log` (rotated).
- **Telemetry is opt-in.** It is off until the user agrees during `openchef init` or with
  `openchef telemetry on`. When on, it sends anonymous, aggregate events only (version,
  OS, runtimes used, task outcome counts, failover counts). It never sends prompts, code,
  file paths, repository names, model output, tokens or account identifiers. The exact
  payload is documented and printable with `openchef telemetry show`. Collection
  infrastructure is out of scope for v1.0; the switch and payload contract ship first.

---

## 15. Security

- Secrets: OpenChef stores no credentials. Probe tokens are read in memory per request.
- Local API socket is user-only (§12.3). No network listener in v1.
- Agents run with the runtime's own sandbox enabled where one exists (`agy --sandbox`,
  Codex sandbox modes); OpenChef's evaluator is an additional layer, not a replacement.
- Child processes inherit a filtered environment (allowlist + explicit additions).
- Supply chain: dependencies pinned in `go.sum`, `govulncheck` in CI, releases built by
  GoReleaser with checksums, SBOM and signatures (cosign keyless).
- Threat model notes live in `docs/security.md` (to be written with M2).

---

## 16. Repository layout and toolchain

```
cmd/openchef/            main package (CLI, TUI entry, daemon entry)
internal/
  api/                   JSON-RPC types and server/client
  daemon/                lifecycle, single-instance lock, recovery
  task/                  domain model: tasks, steps, attempts, sessions, turns, state machines
  engine/                step execution, retries, failover (drives package task)
  router/                agent selection
  quota/                 sources (L1/L2/L3), normalization, health, admission
  permissions/           rule parser, shell parser integration, evaluator, approval queue
  runtime/               Runtime/Session contract and shared helpers
    claude/ codex/ agy/  adapters
    fake/                scriptable fake CLI used by tests
  gitws/                 worktrees, commits, patches, merge queue
  store/                 SQLite schema, migrations, repositories
  procs/                 cross-platform process groups / job objects
  config/                TOML loading, validation, init/doctor
  tui/                   Bubble Tea UI
docs/                    architecture, research, examples, security
testdata/                recorded CLI transcripts (replay fixtures)
```

- **Go:** latest stable release at M0, pinned via `go`/`toolchain` in `go.mod`.
- **Dependencies (intended):** `spf13/cobra` (CLI), `charmbracelet/bubbletea`,
  `bubbles`, `lipgloss` (TUI), `modernc.org/sqlite` (store), `pelletier/go-toml/v2`
  (config), `mvdan.cc/sh/v3` (shell parsing), `golang.org/x/sys` (process control).
  Everything else is standard library. New dependencies need a reason in the PR.
- **Quality gates:** `gofmt`, `go vet`, `golangci-lint` (strict preset), `go test -race`,
  `govulncheck`. Errors are wrapped with context and typed where callers branch on them;
  no ignored errors, no panics outside `main`.

---

## 17. Verification before code (spikes)

These questions decide details above and must be answered first, each with a short
findings note in `docs/research/` and recorded transcripts in `testdata/`. Status after
M1 (`docs/research/m1-findings.md`): 1, 2 (macOS), 3 and 5 answered; 4 deferred until a
logged-in Windows machine is available.

1. **Codex app-server:** start/resume a thread, run a turn, receive and answer a command
   approval and a file-change approval, receive `requestUserInput`, read and receive rate
   limits, observe the quota-exhausted error shape. Decides §6.3.
2. **Pipe EOF behaviour:** do `claude -p`, `codex app-server` and `agy acp` exit promptly
   when stdin closes, including mid-turn? Decides the macOS part of §5.1.
3. **Claude permission modes with the prompt tool:** in each mode, which decisions reach
   `--permission-prompt-tool`? Decides §9.3 for Claude.
4. **Windows:** availability and behaviour of all three CLIs on Windows, Job Object tree
   termination, AF_UNIX socket with ACL. Decides §5.1 and §12.3 on Windows.
5. **Structured plan output:** schema-valid JSON from each planner (Claude
   `--json-schema`, Codex `--output-schema`; agy prompt + validation + one repair turn).

Spikes for §21, run at the start of M7 rather than in M1:

6. **Headless auth in a container:** Claude (`claude setup-token`), Codex
   (`codex login --device-auth` or a copied `auth.json`), agy (no headless login flag is
   documented in `agy --help`; find a supported way or exclude agy from the sandbox).
7. **Agents through `docker exec -i`:** stdin EOF, signal and exit semantics through the
   container runtime, and container cleanup when the daemon dies.
8. **Keep-awake:** `caffeinate -i`, `systemd-inhibit`, `SetThreadExecutionState`
   behaviour on battery and with the lid closed.

---

## 18. Testing strategy

- **Unit tests** for every pure component (rule parser, evaluator, normalizers, router,
  state machines) with table-driven cases, including the prototype's known failure cases
  (chained shell commands, duplicated plan text, double-counted usage, stale replay).
- **Replay tests:** recorded raw transcripts from real CLIs (`testdata/<runtime>/*.jsonl`)
  are fed through each adapter; normalized output is compared with golden files.
- **Fake CLIs:** `internal/runtime/fake` builds a test binary that speaks each runtime's
  protocol from a script (turns, approvals, quota errors, crashes, hangs). Engine and
  daemon tests run against it end to end, including daemon kill/restart recovery.
- **Live tests** against real CLIs run only with `OPENCHEF_LIVE=1`, never in CI, and use
  the cheapest models.
- **CI matrix:** macOS, Linux, Windows on every pull request.

---

## 19. Delivery plan

Each milestone is one or more pull requests; each PR is reviewable on its own and leaves
`main` green.

| Milestone | Content | Exit criteria |
|---|---|---|
| **M0 Foundation** | Go module, CI matrix, lint/test/release skeleton, `procs`, `store` + migrations, `config` + `init`/`doctor` skeleton | CI green on 3 OSes; `openchef doctor` detects installed CLIs |
| **M1 Spikes** | §17 spikes, recorded transcripts | Findings merged; §6.3/§9.3 finalized |
| **M2 Runtimes** | `Runtime`/`Session` contract, `fake`, Claude, Codex, agy adapters, replay tests | One turn + resume works live on each runtime |
| **M3 Engine** | `run` end to end: worktree, turns, verify, retries, commit, failover, waiting_quota, recovery; live event stream for `run`, `logs -f`, `status` (D-014) | Kill-the-daemon test passes; no leaked processes; a running task is visible live from the first turn |
| **M4 Permissions, quota & selection** | Evaluator, shell parsing, approval queue, MCP server for Claude, L1/L2/L3, health, admission; tiers, effort mapping, `auto`/`weighted`/`ordered` strategies (§7.4) | Known bypass cases rejected; routing follows quota; weighted shares hold within ±1 step |
| **M5 Pipeline** | Plan (structured, per-step importance), approval with edits, implement, review (by importance); `isolated` and `team` modes; merge queue; `merge` | Pipeline completes a multi-step task in both modes; importance and effort profile change agent, effort and review as specified |
| **M6 TUI** | Bubble Tea client | Feature parity with CLI for daily use |
| **M7 Unattended & sandbox** | §21: unattended mode (risk classes, deferred approvals, keep-awake, report), sandbox backend (container runtime, devcontainer, export) | An overnight pipeline finishes with no human input, locally and in a sandbox, and its result is exported as a branch |
| **M8 Release** | Docs, examples, security notes, GoReleaser + Homebrew tap, v1.0.0 | Installable via `brew install` and release archives |

---

## 20. Risks

| Risk | Mitigation |
|---|---|
| Upstream CLIs change their protocols | Version + shape checks at startup, recorded transcripts, `doctor` reports exact mismatch |
| Codex app-server stays experimental or changes | Schema pinning; `exec` fallback transport |
| Providers restrict automated or undocumented use | Only official CLIs drive models; L2 probes documented, switchable, fail closed |
| Windows behaviour differs (sandboxes, process trees) | Spike before commitment; Windows in CI from M0 |
| "Chef" trademark confusion | Positioned as an AI agent orchestrator; no visual or naming overlap with Chef/Progress |
| Scope creep | Non-goals in §1.2; new features need an amendment to this document |
| Unattended runs approve something harmful | Deterministic risk classes; critical requests are deferred, never auto-approved outside a sandbox; deny rules always win |
| Sandbox auth unsupported by a CLI (agy) | Spike §17.6 first; a runtime without headless auth is excluded from sandbox routing, not faked |

---

## 21. Unattended mode and sandbox

Scenario: the user starts a pipeline, approves the plan, and leaves (or the machine is
left overnight). OpenChef finishes the task without blocking on the human, and on return
the user finds a verified result and a report. Both features are **optional**.

### 21.1 Unattended mode (`--unattended`)

Plan approval still happens up front, while the user is present. After that, nothing may
block on a human:

- **Keep-awake** while the task is active: `caffeinate -i` (macOS),
  `systemd-inhibit --what=idle:sleep` (Linux), `SetThreadExecutionState` (Windows).
  `openchef doctor` warns about conditions that still sleep the machine (e.g. a closed
  laptop lid on battery).
- **Questions** (`ask_user`) get an immediate answer telling the agent the user is away,
  to make the most reasonable documented assumption, and to note it; each assumption is
  recorded for the report.
- **Approvals** are decided by OpenChef using deterministic risk classes (§21.2) instead
  of waiting. Deny rules and built-in invariants (§9.2) apply unchanged.
- **Quota exhaustion** keeps the existing behaviour: failover, else `waiting_quota` with
  automatic resume at reset.
- **Verification failures** past the retry limit put only that step in
  `needs_attention`; independent steps (team mode) continue.

### 21.2 Risk classes and deferred approvals

Every request that would otherwise `ask` is classified without an LLM, using the parsed
command (§9.2) and resolved paths:

| Class | Examples | Unattended decision |
|---|---|---|
| **routine** | reads and edits inside the workspace; build, test, lint, format commands; read-only git; file utilities confined to the workspace | **approved by OpenChef** and logged |
| **critical** | network access (`curl`, `wget`, `ssh`, `git push`), package installs and lockfile changes, writes or deletes outside the workspace, recursive deletes, `sudo`/privilege changes, environment or credential access, anything unparseable | **deferred** |

A **deferred** request is denied for now with a message to the agent ("this needs the
user; it has been deferred — continue with the remaining work and leave a clear TODO"),
and stored with its full context (step, session, command, reason). On return the user
reviews the deferred queue: `openchef deferred` lists it; approving an item makes OpenChef
resume that step's session with the approval and a turn that completes the deferred part,
then re-verify. Rejecting it closes the item.

Users tune the classes with the same rule syntax as §9.1:

```toml
[unattended]
routine  = ["Bash(make *)", "Bash(npm run *)"]
critical = ["Bash(docker *)"]
```

For Claude sessions in `auto` mode, Claude's own classifier still runs first; OpenChef's
classes apply to whatever reaches the permission tool.

### 21.3 Sandbox backend (`--sandbox`)

- **What:** each task runs in its own container. A full VM is not used: on macOS the
  supported container runtimes already run inside a lightweight VM.
- **Runtimes:** any Docker-compatible CLI (Docker, Podman, OrbStack, Colima), detected by
  `openchef doctor`.
- **Environment:** the project's `.devcontainer/devcontainer.json` if present (the format
  VS Code and Codespaces use), otherwise the OpenChef base image with `claude`, `codex`,
  `agy` and common toolchains.
- **Isolation:** the repository is **cloned** into a container volume, never bind-mounted.
  Nothing on the host is writable from inside.
- **Permissions:** default mode inside the sandbox is `bypassPermissions`; isolation is the
  safety boundary. Deny rules still apply.
- **Network:** open by default (agents need model APIs and package registries). An egress
  allowlist is a later option.
- **Auth:** subscription credentials enter as headless tokens (§17.6), passed as secrets
  at container start, never baked into images or written to the volume.
- **Process model:** the daemon stays on the host; agents run via `docker exec -i` and are
  supervised by `internal/procs` like local ones. Containers carry an `openchef.task`
  label; the daemon removes containers of terminal tasks and sweeps orphans at startup.
- **Export:** `openchef export <task>` fetches the task's branch from the container into
  the user's repository as `openchef/<task-id>` (then `openchef merge` as usual).
  `--patch` and `--to <dir>` export a patch file or a plain copy instead.

### 21.4 Report

When an unattended task finishes or stops, OpenChef sends an OS notification and writes
a report (`openchef report <task>`, also shown in the TUI): steps and their verification,
failovers and quota waits, approvals OpenChef granted, deferred items, assumptions agents
made, review findings, and how to export or merge.
