# Decision Log

Engineering decisions taken while implementing `docs/architecture.md`, newest last.
Architecture-level decisions are amended into the architecture document itself; this log
records the reasoning behind implementation choices and how each one was challenged.

From D-006 on, every non-trivial decision is challenged by an independent reviewer agent
before it is taken; the entry records the strongest objection and why the final choice
survives it.

Format: **Context** → **Options** → **Challenge** → **Decision** → **Consequences**.

---

## D-001 — JSON-RPC notifications use an unbounded ordered queue
*2026-11-25 · M2.1 · `internal/runtime/linerpc`*

- **Context:** the spike client used a fixed 4096-slot channel and dropped notifications
  when it was full. A dropped `turn/completed` hangs a turn forever.
- **Options:** (a) bounded channel and block the reader; (b) bounded and drop;
  (c) unbounded queue between reader and consumer.
- **Challenge:** an unbounded queue can grow without limit if the consumer stalls.
- **Decision:** (c). Blocking the reader (a) also blocks responses to our own calls and
  can deadlock with approval handlers; dropping (b) is a correctness bug. Growth is
  bounded in practice by the agent's own output rate and the session's lifetime, and the
  message size limit (64 MiB) bounds each entry.
- **Consequences:** consumers must drain `Notifications()` until it closes (documented).

## D-002 — Turn events are delivered through a non-blocking queue
*2026-11-25 · M2.2 · `internal/runtime/codex`*

- **Context:** first implementation sent events on the turn channel while holding the
  turn lock. A consumer that cancels and stops reading filled the buffer and wedged the
  router, `Interrupt` and `Close` (found in self-review before any test).
- **Decision:** `emit`/`finish` append to a queue and never block; a dedicated goroutine
  delivers to the channel and closes it after the terminal event.
- **Consequences:** regression test `TestCloseWithUnreadEventsDoesNotBlock` (2000 unread
  events, `Close` must return).

## D-003 — Codex: one sandbox/approval setting for all modes except `plan`
*2026-11-25 · M2.2*

- **Context:** Codex has `approvalPolicy` × `sandbox`, not Claude's six modes.
- **Decision:** every mode except `plan` uses `approvalPolicy: "untrusted"` +
  `sandbox: "workspace-write"`, so **every** request reaches OpenChef's approver, which
  applies the mode semantics and the deny rules. `plan` uses `read-only`.
- **Why:** deny rules must hold in every mode (architecture §9.3); letting Codex
  auto-approve in permissive modes would bypass them, exactly the hole found in Claude.
- **Consequences:** more approval round-trips in permissive modes; the approver answers
  them automatically, so no human is involved.

## D-004 — Codex usage = difference of thread totals, never sums of parts
*2026-11-25 · M2.2*

- **Context:** the prototype double-counted tokens by summing message-level usage.
- **Decision:** per turn, usage = `total` at the last `thread/tokenUsage/updated` minus
  (`total` − `last`) of the first one. Cached input is split out so fields are additive.
- **Consequences:** correct even after `thread/resume`, when the thread total already
  includes earlier turns.

## D-005 — A turn id is learned only from `turn/start` or `turn/started`
*2026-11-25 · M2.2 · found by the live test*

- **Context:** a live run hung: right after `thread/resume`, Codex sent a
  `thread/tokenUsage/updated` for the *previous* turn; the router, slightly behind,
  processed it after the new turn was registered and adopted the old turn id, so every
  real notification of the new turn was discarded.
- **Decision:** the id comes only from the `turn/start` response or `turn/started`.
  Turn-scoped notifications that arrive while it is unknown are held back in order and
  replayed under the router lock once it is known; those of other turns are dropped.
- **Consequences:** regression tests `TestStaleNotificationsAreIgnored` and
  `TestImmediateFailureBeforeTurnStarted` (each repeated 20× per run); live test passed
  3/3 afterwards.

## D-006 — Claude sessions always run with `--restricted` and an explicit tool list
*2026-11-25 · M2.3 · reviewed in two rounds, evidence: m1-findings §9–§10*

- **Context:** without it, a Claude session loads the repository's
  `.claude/settings.local.json` (hooks run arbitrary shell, extra allow rules, env), hooks
  outrank the host, and the default tool set includes tools that act outside the turn
  (`CronCreate`, `RemoteTrigger`, `ScheduleWakeup`, `Monitor`, `Task`, …). In permissive
  modes none of that reaches OpenChef.
- **Options:** (a) `--setting-sources local` as before; (b) an empty setting source
  plus a long `--disallowedTools` list; (c) `--restricted` + `--tools` allowlist.
- **Challenge (reviewer):** "isolation leaks"; `--tools` might strip `StructuredOutput`
  and MCP tools; `--restricted` was unverified in combination.
- **Decision:** (c). Spikes showed `--restricted` ignores settings files (no hook events
  in restricted sessions), confines file tools to the working directory, and sends
  writes to settings/git/tool-config files to the host even in `acceptEdits`; it works
  with `auto`, `dontAsk`, `--resume`, `--mcp-config` and `--json-schema`, and
  `StructuredOutput` survives `--tools`. Allowlist: `Bash, Read, Edit, Write, Glob,
  Grep, NotebookEdit, TodoWrite`. Web tools are off by default and enabled per agent
  (reviewer: exfiltration risk under prompt injection).
- **Consequences:** `Probe` must fail loudly if a CLI version rejects `--restricted`.

## D-007 — Claude rules: native deny *and* ask; invariants as deny rules; approver re-checks
*2026-11-25 · M2.3*

- **Context:** outside `manual`, Claude never consults the host, so OpenChef-side rules
  silently stop applying (reviewer); Bash prefix rules are easy to evade
  (`git -C . push`, `sh -c '…'`).
- **Decision:** pass `permissions.deny` (spec deny rules + invariants such as
  `Write(**/.claude/**)`, `Edit(**/.git/**)`, history-rewriting git) and
  `permissions.ask` (spec ask rules) natively, plus `permissions.allow` for OpenChef's own
  `ask_user` tool. The approver applies the §9.2(1) invariants to every request that does
  reach it (`blocked_path`, `input.file_path`), in every mode. Bash deny rules are
  documented as best-effort; the kernel keeps authority over git (it owns commits and
  checks refs before merging, §8.2).

## D-008 — Claude `bypassPermissions` → `acceptEdits` + approver; `plan` → read-only tools
*2026-11-25 · M2.3*

- **Context:** `--restricted` refuses `bypassPermissions`. Claude's own `plan` mode
  switched `haiku` to `claude-sonnet-5-5` in every test (~25× cost) and writes plan files
  under `$HOME/.claude/plans`.
- **Decision:** `bypassPermissions` runs as `acceptEdits` with the approver allowing
  whatever is not denied by rules or invariants. OpenChef's `plan` mode runs Claude in
  `dontAsk` with `--tools Read,Glob,Grep` — read-only by construction, no model switch.
- **Consequences:** `TurnResult.Models` (from `result.modelUsage`) is reported so any
  future model drift is visible to routing and cost accounting.

## D-009 — Claude turns are correlated by per-process result counting
*2026-11-25 · M2.3*

- **Context:** Claude messages carry no turn id; an interrupted turn's late `result`
  could be credited to the next turn (the D-005 failure mode). Spikes: exactly one
  `result` per user message, `result_index` increments per turn and restarts at 0 in a
  resumed process; interrupt returns `still_queued`.
- **Decision:** the adapter counts user messages written to the current process; a turn
  ends only on the `result` whose `result_index` equals its ordinal. A new turn is
  refused while a result is owed. `Interrupt` blocks until the owed result arrives (or
  10 s pass, then the process is killed and transparently resumed on the next turn).
  Messages reported in `still_queued` are subtracted.

## D-010 — Claude Close kills fast when a turn is in flight
*2026-11-25 · M2.3*

- **Context:** after stdin EOF Claude finishes the turn in flight (8.8 s measured), i.e.
  keeps writing to the worktree while the engine may be committing.
- **Decision:** idle → close stdin, normal grace. In flight → interrupt, wait ≤ 3 s for the
  result, then kill the process group without further grace.

## D-011 — Per-turn output schema by transparent re-spawn
*2026-11-25 · M2.3*

- **Context:** Claude only accepts `--json-schema` at start; the engine sets a schema per
  turn (planner, reviewer).
- **Options:** (a) a session-level schema field; (b) re-spawn with `--resume` when the
  schema changes.
- **Decision:** (b), only between turns, and back without the schema when a later turn
  has none. Cost: one process start and likely a prompt-cache miss on that turn.
