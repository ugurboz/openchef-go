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
