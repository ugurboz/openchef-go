# Phase 0 Research Findings

Empirical findings about the agent CLIs OpenChef drives. Every claim below was verified by
a throwaway spike against a real CLI and a real subscription in November 2026. The spike
scripts themselves were written in Python and live in the archived prototype repository
(`ugurboz/Orchestrator`, tag `python-legacy`, directory `spikes/phase0/`).

Only **type-shape fixtures** are kept here (`fixtures/*.shape.json`). They contain field
names and value types, never real values, tokens or account data.

Tested versions: `claude` 2.1.311–2.1.325, `codex-cli` 0.172.0, `opencode` 2.3.2,
`agy` 1.5.13–1.5.15, macOS (Apple Silicon).

> These findings describe third-party behaviour at a point in time. Each adapter must
> re-verify its contract at startup (version check) and fail loudly when it changes.

---

## 1. Claude Code (`claude -p`)

### Streaming I/O
- `claude -p --input-format stream-json --output-format stream-json --verbose` gives a
  multi-turn session over stdin/stdout. Each user turn is one JSON line:
  `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"..."}]}}`.
- A turn ends with a `{"type":"result", ...}` event. The `result` field repeats the final
  assistant text, so consumers must **not** concatenate assistant text and `result.result`.
- `result.usage` is the cumulative usage of the turn. Per-message `assistant.message.usage`
  values must not be summed together with it.
- With an anonymous pipe, closing stdin makes `claude -p` exit (~0.8 s after EOF, rc=0).
  The process stays alive as long as stdin stays open.

### Session identity and recovery
- `--session-id <uuid>` lets the caller choose the session id up front (no need to parse
  the `system/init` event).
- `--resume <session_id>` in a new process restores full conversation context after the
  original process is killed. Verified with a code-word recall test.

### Permissions and questions
- `--permission-prompt-tool mcp__<server>__<tool>` routes every permission decision to an
  MCP tool the orchestrator provides.
  - Incoming arguments: `{"tool_name": "Bash", "input": {...}, "tool_use_id": "toolu_..."}`.
  - Expected text result: `{"behavior":"allow","updatedInput":<input>}` or
    `{"behavior":"deny","message":"..."}`.
  - Delayed answers (5 s allow, 1 s deny) work; the agent blocks until answered.
- `--disallowedTools AskUserQuestion` plus an orchestrator-provided `ask_user` MCP tool
  lets the agent ask the human a question and block for the answer.
- Claude also calls the permission tool **before** calling the `ask_user` tool. The
  approval layer must auto-allow its own `ask_user` tool to avoid double prompts.
- `--strict-mcp-config --setting-sources local` with a clean `cwd` keeps the user's global
  hooks, plugins and MCP servers out of the headless session.

### Quota signals (L1, in-stream)
- `rate_limit_event` events carry utilization as a **fraction** (`0.54`) and `resetsAt` as
  **epoch seconds**, per window (`five_hour`, `seven_day`).
- `overageStatus: "rejected"` (e.g. `overageDisabledReason: org_level_disabled`) means a
  hit limit cannot be extended: treat as hard quota exhaustion.

### Quota probe (L2, out-of-band)
- `GET https://api.anthropic.com/api/oauth/usage` with header
  `anthropic-beta: oauth-2025-04-20` and the Claude Code OAuth token returns HTTP 200.
- Units differ from the stream: utilization is a **percentage** (`55.0`), resets are
  **ISO-8601** strings. A normalization layer is mandatory.
- `limits[]` holds `kind: "session"` (= five hour) and `kind: "weekly_all"` (= seven day).
- `seven_day_breakdown.rows[]` is a per-**surface** breakdown (`claude_code`, `chat`,
  `cowork`, `other`), not per model. Pro accounts have no separate per-model weekly bucket.
- The response contains many opaque, frequently changing fields (feature flags). Parse
  only the fields used, tolerate unknown ones, and fail closed if the used ones change.
- Shape: [`fixtures/claude_usage.shape.json`](fixtures/claude_usage.shape.json).
- This endpoint is undocumented. See the architecture document for the risk policy.

## 2. Codex CLI (`codex exec`)

- `codex exec --json --sandbox workspace-write --cd <dir> -` reads the prompt from stdin
  and emits JSONL: `thread.started` (`thread_id`), `turn.started`, `item.completed`
  (`item.type = agent_message`, `item.text`), `turn.completed` (`usage`).
- One process per turn. `codex exec resume --json <thread_id> -` continues the same
  thread in a new process with full context (verified: turn 1 5.6 s, turn 2 6.3 s, recall
  succeeded).
- Shape: [`fixtures/codex_exec.events.shape.json`](fixtures/codex_exec.events.shape.json).

### ChatGPT quota probe (L2)
- `GET https://chatgpt.com/backend-api/wham/usage` with `Authorization: Bearer <token>`
  and `ChatGPT-Account-Id` returns `plan_type` and
  `rate_limit.{allowed, limit_reached, primary_window, secondary_window}`; each window has
  `used_percent`, `limit_window_seconds`, `reset_after_seconds`, `reset_at` (epoch).
- Credentials are read from `~/.codex/auth.json` (`tokens.access_token`,
  `tokens.account_id`).
- On a free plan `secondary_window` is `null`; Plus exposes both windows.
- Shape: [`fixtures/chatgpt_usage.shape.json`](fixtures/chatgpt_usage.shape.json).
- Undocumented endpoint, same risk policy as the Claude probe.

## 3. Antigravity CLI (`agy`)

### Headless `-p` mode is unsuitable for supervised work
- Default `permission_mode: proceed-in-sandbox`: commands inside the sandbox run
  **without asking**, and `permissions.ask` rules are ignored.
- Writes outside the sandbox are blocked, yet the run still reports `status: SUCCESS` and
  exit code 0. Failure is only visible in tool output and response text.
- Conclusion: never trust `agy` exit status; success is decided by an
  orchestrator-executed verification command.

### ACP mode (`agy acp`, Agent Client Protocol over stdio JSON-RPC)
- Flow: `initialize` → `session/new` → `session/prompt`.
- Live approvals: when the agent wants to escape the sandbox (`BypassSandbox: true`) the
  client receives `session/request_permission` with options `allow_once` / `reject_once`.
  `allow` ran the command; `reject` failed the tool call and no file was created.
- Session modes: `default`, `accept-edits`, `auto`, `plan`.
- `session/set_model` switches models directly (e.g. `gemini-3.8-flash-low`).
- `session/new` lists the available models (27 on Google AI Pro), including
  `claude-opus-5-5-low|medium|high`, i.e. Opus capacity separate from a Claude
  subscription.
- **Recovery:** after `SIGKILL` of the `agy acp` process, a fresh process plus
  `session/load` (`loadSession` capability) restored context (code-word recall in 3.7 s).
- Isolation flags: `--cli_dir_name <name>` and `--include-default-customizations=false`
  keep the user's own agy configuration out of orchestrated sessions.
- Cost: even a trivial turn costs ~18–19k input tokens (large tool system prompt).
- Quota: no quota fields in the stream. ACP `usage_update {used, size}` is context-window
  usage, not subscription quota. Quota for agy is estimated (L3) only.
- Pitfall: relative paths in agy deny rules (e.g. `.env`) break sandbox setup with
  `sandbox configuration error: ... non-absolute file path`.

## 4. OpenCode (`opencode serve`)

- Auth: `OPENCODE_SERVER_PASSWORD` + HTTP Basic (`opencode:<pw>`). `/api/*` and
  `/openapi.json` return 401 without it (web UI shell routes do not).
- The OpenAPI spec lives at `/openapi.json`. Adapters should verify the contract on start.
- `experimental/session/{id}/log` is unusable (only emits `log.synced`).
- `/api/event` (SSE) carries a gapless per-session `durable: {aggregateID, seq, version}`
  but has **no replay** (`Last-Event-ID` is ignored).
- Recovery: detect a `seq` gap, then reconcile via REST (`/api/session/{id}`, `/message`,
  `/permission`, `/api/session/active`). Verified: a 10-event gap was fully recovered.
  The event stream is for live display; REST is the source of truth.
- Permissions: with `permission: {"edit":"ask","write":"ask","bash":{"*":"ask"}}`,
  pending requests appear at `GET /api/session/{id}/permission` (`action`, `resources`,
  and for edits `metadata.files[].patch` with the full diff). Reply with
  `POST /api/session/{id}/permission/{pid}/reply {"decision":"once"|"always"|"reject","message"?}`
  (204). `reject` physically prevents the write.
- Questions: built-in `GET /api/session/{id}/form` and
  `POST /api/session/{id}/form/{fid}/reply {"answer":{...}}` (204).

## 5. Process supervision

- Children started in a new session/process group survive a `SIGKILL` of the parent, and
  a restarted supervisor can re-adopt them by validating PID + process create time.
- This prototype proved detach-and-adopt is possible, but it requires FIFO tricks and
  byte-offset log tailing, and it caused process leaks in practice. Since all three
  primary runtimes support native resume (`--resume`, `exec resume`, `session/load`),
  OpenChef recovers by **resuming sessions** instead of adopting orphaned processes.
  See the architecture document.

## 6. Open questions (not yet observed)

- The exact error envelope when a Codex/ChatGPT Plus limit is hit.
- The exact error envelope when an `agy` quota limit is hit.
- Full model IDs exposed to Codex on a Plus plan.
