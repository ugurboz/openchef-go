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

## D-012 — Claude: an interrupt that finds the message still queued replaces the process
*2026-11-25 · M2.3 · amends D-009*

- **Context:** D-009 said messages reported in `still_queued` are subtracted from the
  owed-result count. The protocol does not say whether a still-queued message is
  dropped or will still run after the interrupt; if it runs, its late `result` would be
  credited to the next turn (the D-005 failure mode).
- **Options:** (a) subtract and trust that no result follows; (b) wait for a result
  that may never come; (c) kill the process and let the next turn re-spawn it.
- **Challenge (self-review):** killing discards a message the user wanted interrupted
  anyway, so nothing is lost; the only cost is latency on the next turn.
- **Decision:** (c). It is correct under both readings, costs one process start, and
  only happens when an interrupt lands before Claude picked up the message.
- **Consequences:** test `TestInterruptOfQueuedMessageReplacesProcess`.

## D-013 — Claude: `--resume` only after the first result; one fallback on start failure
*2026-11-25 · M2.3 · live check with claude 2.1.325*

- **Context:** a replacement process (D-011 schema change, D-009/D-012 kill, crash)
  continues the conversation with `--resume`. Live check: `--resume` of a session id
  that has no messages yet fails with `No conversation found with session ID`, while
  starting again with the same `--session-id` works. A first turn with an output schema
  hits exactly this case.
- **Challenge (self-review):** "retry with the other flag" could mask real start
  failures. It does not: the retry happens at most once, only for replacement
  processes (never for `Open`/`Resume`), and both errors are returned joined.
- **Decision:** the session counts as started once any process returned a `result`
  (or when the engine calls `Resume`); before that a replacement reuses
  `--session-id`. A process that died mid-turn may or may not have written a
  transcript, so a failed replacement start is retried once with the other flag.
- **Also:** `error_max_budget_usd` is classified `unknown`, not `quota_exhausted`: it
  is a `--max-budget-usd` cap (not passed by OpenChef today), and treating it as quota
  would park the task in `waiting_quota`.
- **Consequences:** tests `TestStructuredOutputRespawnsAndBack`,
  `TestCrashOnFirstTurnRecoversWithFallback`; `TestLive` covers the schema-first path
  against the real CLI. Start failures carry the CLI's stderr tail.

## D-014 — `run` streams what the agents do, live, from M3
*2026-11-25 · plan · requested by the user*

- **Context:** the TUI (M6) is the main place to watch agents, but M3–M5 would otherwise
  run tasks that are visible only through `status` and log files. Watching what an agent
  does is a core need, not a polish item.
- **Options:** (a) wait for the TUI; (b) a separate `watch` command; (c) `run` and
  `pipeline` stream the task's events by default, like `claude` and `codex` do.
- **Challenge (self-review):** streaming in the foreground could tie the task's lifetime
  to the terminal. It does not: the daemon owns the task; the terminal only renders, and
  Ctrl-C detaches the view instead of cancelling.
- **Decision:** (c), with `-q/--quiet`, `--detach` and `--json`. `logs -f` and `run` share
  one renderer over the normalized events (§6.1), so the TUI later reuses the same event
  model.
- **Consequences:** M3 exit criteria include seeing a running task live from its first
  turn.

## D-015 — agy: sandbox always on, `proceed-in-sandbox`, ACP mode always `default`
*2026-11-25 · M2.4 · evidence: m1-findings §11*

- **Context:** agy ignores `permissions.deny` entirely, and its `auto`/`accept-edits`
  modes act without consulting the host, so OpenChef rules would silently stop
  applying. The terminal sandbox, measured, allows only reads inside the workspace: no
  writes, no network, no reads outside.
- **Options:** (a) sandbox on, `proceed-in-sandbox`; (b) sandbox off, `request-review`
  (every command asks once); (c) use agy's own modes for OpenChef's modes.
- **Challenge (self-review):** (a) lets commands run unasked, so deny rules do not see
  them; and builds need writes, so every one escapes and costs a round-trip.
- **Decision:** (a). Unasked commands are contained to reading the workspace, which
  every mode allows anyway; anything that writes, reaches the network or leaves the
  workspace must escape with `BypassSandbox` and therefore reaches the approver, which
  applies the mode, deny rules and invariants. (b) would run approved commands with
  full user rights, weaker than Codex's `workspace-write`. (c) is ruled out by `auto`
  and `accept-edits` bypassing the host. OpenChef's modes are therefore implemented by
  the approver (as for Codex, D-003): agy always runs in ACP mode `default`; `plan`
  denies every request; `acceptEdits` auto-allows edits subject to invariants.
- **Consequences:** settings are written by OpenChef to its own `--cli_dir_name`
  directory (the user's agy configuration is never read). A repository under the temp
  tree loses containment (the sandbox allows `$TMPDIR`): the adapter refuses to start
  there. Approval requests carry `BypassSandbox` so the approver and the UI can show
  that a command leaves the sandbox. agy's `plan` mode is never used (it writes outside
  the workspace).

## D-016 — agy protocol rules: options by kind, swallow load replay, cancel by notification
*2026-11-25 · M2.4*

- **Decision:** permission answers select the option whose `kind` is `allow_once` /
  `reject_once` (ids changed between versions); if none matches, the request is
  cancelled. Updates received while `session/load` runs are history and are dropped
  (the D-005 failure mode). `Interrupt` sends `session/cancel` and waits for the prompt
  to return `cancelled` (kill and reload after 10 s, as for Claude). Usage is the
  prompt result's per-turn `usage`; cached reads are reported separately.
- **Consequences:** replay tests on `m18-load-resumed`, `m18-cancel`, `m18-bypass`.

## D-017 — agy: structured output by prompt, validation and one repair; response barrier
*2026-11-25 · M2.4*

- **Context:** agy has no native output schema (m1-findings §4), and ACP ends a turn with
  the `session/prompt` response, which the JSON-RPC peer delivers on a different path
  than the `session/update` notifications that precede it.
- **Decision:** the adapter appends the schema to the prompt, extracts the JSON value
  from the final answer (whole text, last fenced block, or outermost object/array) and
  validates it with `internal/schema` (`santhosh-tekuri/jsonschema/v6`, pure Go,
  Apache-2.0; external `$ref`s refused). One repair prompt follows a failure; a second
  failure ends the turn as `protocol`. Usage counts both prompts. The final answer is
  the agent text after the last tool call. `linerpc.Peer.Barrier` makes the adapter wait
  until every update read before the response was routed, so no chunk is lost or
  credited to the next turn.
- **Challenge (self-review):** a new dependency for one runtime. It is small, has no
  transitive network or cgo use, and the engine can reuse it to double-check native
  schema output.
- **Consequences:** tests `TestStructuredOutput`, `TestBarrierKeepsEveryChunk`,
  `TestBarrierOrdersNotificationsBeforeResponse`.

## D-018 — Domain model in `internal/task`; store transitions are compare-and-set
*2026-11-25 · M3.3*

- **Context:** the engine (M3.5), recovery (M3.7) and the CLI all need the task, step,
  attempt, session and turn types and the §7.1 state machine; the store must enforce the
  write-ahead rule (§11) when the daemon, a CLI command (`cancel`, `retry`) and recovery
  race for the same row.
- **Options:** (a) state machine inside `internal/engine`, store writes whatever it is
  given; (b) a dependency-free domain package that the store and engine both import, and
  every store transition guarded by the caller's expected state.
- **Challenge (self-review):** (b) adds a package not in §16 and makes the store
  validate rules the engine already knows. But with (a) the store would import the
  engine (cycle) or accept any status string, and a lost update between `cancel` and the
  engine loop is exactly the stale-state bug class the prototype had. Validating twice
  is cheap; the CHECK constraints are a third line against manual edits.
- **Decision:** (b). `task` holds the types, IDs (10-char Crockford base32, 50 random
  bits) and state machines. Every store transition is `UPDATE … WHERE status = from`;
  zero rows yields `ErrNotFound` or a `*ConflictError` with the actual state. Steps,
  attempts and sessions only change while their parent is in the right state; partial
  unique indexes allow one running attempt per step and one turn in flight per session.
  Process start tokens are stored as decimal text (uint64 does not fit SQLite INTEGER).
- **Review (independent agent):** found that `cancel` did not stop later side effects
  and that a resumed session could not be recorded. Fixed: attempts, sessions and turns
  start only while the task is active (running/reviewing); leaving the active states
  ends running attempts, interrupts turns in flight and resets running steps in the same
  transaction; `done` needs every step verified. A session row is one conversation: a
  process replaced within it updates the row (`SetSessionProcess`), a resume after a
  daemon restart is a new row with `resumed_from`, and the native id is unique among
  live rows only.
- **Consequences:** §16 lists `internal/task`. Tests `TestTransitionTaskConcurrent`
  (16 writers, one winner), `TestCancelRacesEngine`, `TestResumeAfterRestart`,
  `TestDoneRequiresFinishedWork`, `TestNativeIDUnique`.

## D-021 — Permission matching: parsed commands, opaque runners, redirections as file access
*2026-11-26 · M4.1/M4.2*

- **Context:** §9.2 requires Claude Code's rule syntax with shell commands parsed into
  simple commands. Several cases are not covered by Claude Code's documentation.
- **Decision:**
  - Commands are split with `mvdan.cc/sh/v3` (Bash dialect) across lists, pipes,
    subshells, command and process substitution, compound commands and `sh -c` strings
    (nested up to 4 levels). Every simple command must be allowed; any deny denies.
  - A command is *dynamic* when a word expands at run time (`$X`, `$(…)`, globs, `~user`,
    `$'…'`), when it has variable assignments (`PATH=… x`), when it defines a function
    (which can shadow any name), or when it runs code it receives (`eval`, `source`,
    `xargs`, `sudo`, `exec`, `ssh`, `find -exec` …). Dynamic commands are never allowed
    by allow rules; prefix deny rules still match their static leading words.
  - Wrappers (`env`, `timeout`, `nice`, `nohup`, `time`, `command` …) are unwrapped, so
    `timeout 5 rm -rf /` meets `Bash(rm:*)`.
  - Redirection targets are file access: `>`/`>>` are checked as Write, `<` as Read,
    with the workspace and protected-directory invariants; `~/` resolves to home.
  - Edit rules cover Edit/Write/MultiEdit/NotebookEdit; Read rules cover Read/Grep/Glob;
    a pattern without a slash matches at any depth; a directory pattern covers its
    contents.
  - A settings file with any invalid rule or unknown key fails to load.
- **Challenge (self-review):** stricter than Claude Code in places (assignments,
  functions), so some harmless commands ask. Asking costs a click; a bypass costs the
  user's machine. A fuzz test found a parser-induced panic (`()0`), fixed and kept as a
  regression; the evaluator also recovers from panics as "ask".
- **Consequences:** `TestBypassCorpus` (23 bypass attempts), `FuzzSplit`, mode table.
