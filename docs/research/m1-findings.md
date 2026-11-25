# M1 Spike Findings

Answers to the questions in `docs/architecture.md` §17, verified on 2026-11-25 against
`claude` 2.1.325, `codex-cli` 0.172.0 and `agy` 1.5.16 on macOS (Apple Silicon), using the
cheapest models (`haiku` at `--effort low`, `gpt-6-luna` at effort `low`,
`gemini-3.8-flash`). Spike programs live in `spikes/` (separate Go module); scrubbed
recordings are in `testdata/<runtime>/` and are the fixtures for adapter replay tests.

| § | Question | Status |
|---|---|---|
| 17.1 | Codex app-server | **Answered — adopted** |
| 17.2 | Stdin EOF behaviour | **Answered** (macOS) |
| 17.3 | Claude permission modes | **Answered — design changed** |
| 17.4 | Windows | Deferred (no logged-in Windows machine available) |
| 17.5 | Structured plan output | **Answered** |

---

## 1. Codex `app-server` (§17.1) — adopted as the Codex transport

Transport: newline-delimited JSON-RPC over stdio. Handshake `initialize`
(`clientInfo` required) → `initialized` notification. Recording: `testdata/codex/`.

| Check | Result |
|---|---|
| `thread/start` with `cwd`, `model`, `approvalPolicy: "untrusted"`, `sandbox: "workspace-write"` | works |
| `turn/start` accepts `effort` and `outputSchema` per turn | works |
| Command approval: server request `item/commandExecution/requestApproval` (`command`, `cwd`, `commandActions`, `reason`) | answered with `{"decision": "accept"}` → command ran |
| Same with `{"decision": "decline"}` | command did not run; the turn continued and completed |
| `turn/interrupt` mid-turn | `turn/completed` with `status: "interrupted"` in ~1 s |
| `kill -9` the app-server, new process, `thread/resume {threadId}` | context recovered (code word recalled) |
| `account/rateLimits/read` | first-party windows: `primary`/`secondary` with `usedPercent`, `windowDurationMins`, `resetsAt` (epoch s), plus `planType` |
| `account/rateLimits/updated` notifications | 9 during one short session → usable as L1 |
| Turn errors | typed: `turn.error.codexErrorInfo` ∈ `usageLimitExceeded`, `rateLimitExceeded`, `unauthorized`, `contextWindowExceeded`, `serverOverloaded`, … — no text matching needed |
| `item/tool/requestUserInput` | **not available** in the default collaboration mode (the model answers "I can't use that tool in Default mode") |
| OpenChef MCP server passed per thread via `thread/start.config.mcp_servers` | works: Codex sends `mcpServer/elicitation/request` (`_meta.codex_approval_kind: "mcp_tool_call"`) before the call; answering `{"action": "accept"}` runs the tool and the agent used the answer |

Consequences:

- Codex uses `app-server`; the `exec` fallback is kept only for diagnostics.
- Questions to the user go through OpenChef's own `ask_user` MCP tool, exactly as for
  Claude. The approval layer auto-accepts the elicitation for OpenChef's own tool.
- Quota exhaustion is detected from `codexErrorInfo`, not from message text. The shape is
  known from the schema; the live value has not been observed yet (requires hitting a
  real limit).
- The Codex account on the test machine is on the **free** plan (`planType: "free"`,
  one 30-day window); Plus/Pro accounts expose a second window.

## 2. Stdin EOF (§17.2)

| Runtime | Idle | Mid-turn |
|---|---|---|
| `codex app-server` | exits in ~7–21 ms | exits in ~25 ms (turn abandoned) |
| `agy acp` | exits in ~18–32 ms | exits in ~26–49 ms (turn abandoned) |
| `claude -p` (stream-json) | exits in ~0.9 s | **finishes the current turn**, emits `result`, then exits (8.8 s in the test) |

Consequence for §5.1 (macOS, no parent-death signal): an orphaned agent cannot outlive
the daemon indefinitely. Codex and agy die immediately; Claude completes at most the turn
in flight and exits. The startup sweep (`procs.KillIfAlive`) remains as the second line
of defence.

## 3. Claude permission modes (§17.3) — design changed

Every run gave the same three-part task: a shell command, a file write with the edit
tool, and a shell command that the host should deny. Recording: `testdata/claude/`.

| Mode | Reached the host | Executed | Denied command |
|---|---|---|---|
| `manual` | Bash, Write, Bash (all) | as decided | blocked by host |
| `acceptEdits` | none | all | **ran** |
| `plan` | only `ExitPlanMode` | nothing | — |
| `auto` | none | all | **ran** |
| `dontAsk` | none | nothing | — |
| `bypassPermissions` | none | all | **ran** |

1. **Host path:** `--permission-prompt-tool stdio` plus an `initialize` control request
   (the Agent SDK control protocol) delivers `control_request` / `can_use_tool` messages
   on stdout with `tool_name`, `input`, `description`, `blocked_path` and
   `permission_suggestions`; answering with a `control_response`
   (`behavior: allow|deny`) is enforced. No separate MCP process is needed for approvals.
   `--permission-prompts host` alone (with or without `initialize`) delivered **no**
   requests and refused everything. The MCP `--permission-prompt-tool mcp__…` path still
   works (Phase 0) and remains the fallback.
2. **Deny rules must be native.** In `acceptEdits`, `auto` and `bypassPermissions` the
   host is never consulted, so a host-side evaluator cannot enforce deny rules. Passing
   them in Claude's own syntax via `--settings '{"permissions":{"deny":[…]}}'` blocked the
   denied command in all three modes, including `bypassPermissions`. Because OpenChef
   uses Claude Code's rule syntax (§9.1), deny rules can be passed through unchanged.
3. **Planner sessions:** in `plan` mode the only request is `ExitPlanMode`; OpenChef must
   deny it for planner and reviewer sessions so they stay read-only.
4. **`dontAsk`** refuses anything that would prompt without consulting the host —
   identical to OpenChef's definition.

## 4. Structured plan output (§17.5)

| Runtime | Mechanism | Result |
|---|---|---|
| Claude | `--json-schema '<schema>'` | `result.structured_output` holds schema-valid JSON; `result.result` holds the same as text |
| Codex | `turn/start.outputSchema` | final `agentMessage` text is schema-valid JSON |
| agy | none observed | prompt + strict validation + one repair turn (as planned) |

## 5. Effort and modes per runtime (§7.4)

| Runtime | Effort | Modes |
|---|---|---|
| Claude | `--effort low\|medium\|high\|xhigh\|max` | `manual`, `acceptEdits`, `plan`, `auto`, `dontAsk`, `bypassPermissions` |
| Codex | `turn/start.effort`; values advertised per model by `model/list` (e.g. `gpt-6-luna`: low…max) | approval policy `untrusted`/`on-request`/`never` + sandbox `read-only`/`workspace-write`/`danger-full-access` |
| agy | config option `thought_level` (`low`…`xhigh`) via `session/set_config_option`; model is a separate config option (`gemini-4-neon`, …) | `default`, `accept-edits`, `auto`, `plan` |

## 6. Usage accounting

- Claude: summing `assistant` message `usage` gives **exactly twice** `result.usage`.
  Only `result.usage` may be used (confirms the prototype's double-counting bug).
- Claude `result` also carries `total_cost_usd`, `modelUsage` and `permission_denials`
  (useful for the report).
- agy: even a one-word reply costs ~20 k input tokens (tool-heavy system prompt).

## 7. Agents report success they did not achieve

In several runs the agent answered `DONE` after its command had been **refused**
(Codex and Claude). This is direct evidence for principle 1 (§2): success is decided by
the kernel's verification command, never by the agent's reply.

## 8. Still open

- Windows behaviour of all three CLIs (§17.4).
- Live quota-exhaustion values: Codex `usageLimitExceeded`, Claude `rate_limit_event`
  rejection, agy error shape.

---

## 9. Follow-up spike M1.6 — Claude hardening (after design review D-006)

Run on 2026-11-25 with `claude` 2.1.325 (`spikes/claude-cli/m16.go`, recordings
`testdata/claude/m16-*.jsonl`). Triggered by an independent design review of the
Claude adapter.

| Check | Result |
|---|---|
| `--restricted` + Write tool to `../outside.txt` | blocked by Claude **without** asking the host |
| `--restricted` + Write `.claude/settings.local.json` | reached `can_use_tool` even in `acceptEdits` |
| `--restricted` + Bash `echo b > ../outside2.txt` | reached `can_use_tool` even in `acceptEdits` |
| `--restricted` + `--permission-mode bypassPermissions` | refused at start: `Error: bypassPermissions not supported in restricted mode` (exit 1) |
| `--tools "Bash,Read,Edit,Write,Glob,Grep"` | limits the built-in tool set |
| Interrupt (`control_request` `{"subtype":"interrupt"}`) | `control_response` success `{"still_queued":[]}`, then `result` `subtype: error_during_execution`, `terminal_reason: aborted_streaming` |
| Next turn after interrupt | normal `result` with `result_index: 1` |
| One `result` per user message | yes; `result_index` increments per turn |
| `--max-budget-usd` exceeded | `result` `subtype: error_max_budget_usd`, `is_error: true`, `terminal_reason: budget_exhausted` |
| `--permission-mode plan` with `--model haiku` | ran on `claude-sonnet-5-5` (again); ~25× the cost of the same prompt in other modes |

Without `--restricted`, the review also found (from the M1 recordings): the session
loads the repository's `.claude/settings.local.json` (hooks, allow rules, env), the
init message reports `other_hook_ask_outranks_host: true`, and the default tool set
includes tools that act outside the turn (`CronCreate`, `RemoteTrigger`,
`ScheduleWakeup`, `SendToProject`, `PushNotification`, `Monitor`, `Task`).

## 10. Follow-up spike M1.7 — `--restricted` in combination (review round 2)

Recordings `testdata/claude/m17-*.jsonl`.

| Check | Result |
|---|---|
| Planner shape: `--restricted --permission-mode dontAsk --tools Read,Glob,Grep --json-schema … --mcp-config …` with `permissions.allow: ["mcp__openchef__ask_user"]` | works: `StructuredOutput` stays available (schema-valid `structured_output`), `ask_user` was called and its answer used, model stayed `claude-haiku-5-5` (no plan-mode model switch) |
| `--restricted --permission-mode auto` | accepted; two turns completed |
| Deny rule `Write(**/.claude/**)` against `sub/.claude/settings.json` | denied (`permission_denials: [Write]`), the sibling write succeeded |
| `--resume` in a new process | context kept; `result_index` **restarts at 0** in the new process |
| Tools exposed by an MCP server | every tool of the server is offered to the model (`mcp__openchef__approve` appeared next to `ask_user`), so OpenChef's server must expose only `ask_user` |
