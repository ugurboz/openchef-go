-- Domain tables for the engine (docs/architecture.md §4, §7, §11).
-- Times are RFC 3339 UTC text with nanoseconds; empty strings never stand in for NULL.
-- State values are validated by package task before they are written; the CHECK
-- constraints are a second line of defence against a bad migration or manual edit.

CREATE TABLE tasks (
    id          TEXT NOT NULL PRIMARY KEY,
    kind        TEXT NOT NULL CHECK (kind IN ('run', 'pipeline')),
    status      TEXT NOT NULL CHECK (status IN (
                    'planning', 'awaiting_approval', 'running', 'reviewing', 'waiting_quota',
                    'needs_attention', 'done', 'failed', 'cancelled')),
    prompt      TEXT NOT NULL,
    repo_root   TEXT NOT NULL,
    base_commit TEXT,
    mode        TEXT NOT NULL CHECK (mode IN ('isolated', 'team')),
    backend     TEXT NOT NULL CHECK (backend IN ('local', 'sandbox')),
    importance  TEXT NOT NULL CHECK (importance IN ('low', 'normal', 'high', 'critical')),
    agent_pin   TEXT,
    branch      TEXT,
    worktree    TEXT,
    resume_at   TEXT,
    reason      TEXT,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
) STRICT;

-- Recovery and `openchef status` scan unfinished tasks. The terminal list must match
-- task.Status.Terminal (TestTerminalListMatchesDomain).
CREATE INDEX tasks_active ON tasks (status) WHERE status NOT IN ('done', 'failed', 'cancelled');
CREATE INDEX tasks_created ON tasks (created_at);

CREATE TABLE steps (
    task_id         TEXT NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    idx             INTEGER NOT NULL CHECK (idx BETWEEN 1 AND 7),
    title           TEXT NOT NULL,
    prompt          TEXT NOT NULL,
    files           TEXT NOT NULL DEFAULT '[]', -- JSON array of strings
    verify          TEXT,
    importance      TEXT NOT NULL CHECK (importance IN ('low', 'normal', 'high', 'critical')),
    status          TEXT NOT NULL CHECK (status IN ('pending', 'running', 'verified', 'failed')),
    retries         INTEGER NOT NULL DEFAULT 0 CHECK (retries >= 0),
    verified_commit TEXT,
    updated_at      TEXT NOT NULL,
    PRIMARY KEY (task_id, idx)
) STRICT;

CREATE TABLE attempts (
    id         INTEGER PRIMARY KEY,
    task_id    TEXT NOT NULL,
    step_idx   INTEGER NOT NULL,
    agent      TEXT NOT NULL,
    runtime    TEXT NOT NULL,
    model      TEXT,
    pool       TEXT NOT NULL,
    status     TEXT NOT NULL CHECK (status IN (
                   'running', 'succeeded', 'failed_over', 'failed', 'interrupted', 'cancelled')),
    outcome    TEXT,
    started_at TEXT NOT NULL,
    ended_at   TEXT,
    FOREIGN KEY (task_id, step_idx) REFERENCES steps (task_id, idx) ON DELETE CASCADE
) STRICT;

CREATE INDEX attempts_step ON attempts (task_id, step_idx);
-- At most one live attempt per step.
CREATE UNIQUE INDEX attempts_one_running ON attempts (task_id, step_idx) WHERE status = 'running';

CREATE TABLE sessions (
    id          INTEGER PRIMARY KEY,
    attempt_id  INTEGER NOT NULL REFERENCES attempts (id) ON DELETE CASCADE,
    -- A session resumed after a daemon restart continues this closed session's
    -- conversation (same native id, new process).
    resumed_from INTEGER REFERENCES sessions (id),
    runtime     TEXT NOT NULL,
    native_id   TEXT,
    pid         INTEGER NOT NULL CHECK (pid > 0),
    start_token TEXT NOT NULL, -- decimal uint64 (procs.Identity.StartToken)
    state       TEXT NOT NULL CHECK (state IN ('starting', 'open', 'closed')),
    log_dir     TEXT,
    opened_at   TEXT NOT NULL,
    closed_at   TEXT,
    CHECK (state <> 'open' OR native_id IS NOT NULL)
) STRICT;

CREATE INDEX sessions_attempt ON sessions (attempt_id);
CREATE INDEX sessions_live ON sessions (state) WHERE state <> 'closed';
-- One live process per CLI conversation: two would interleave turns. Closed rows keep
-- their native id so a resumed session can continue the conversation.
CREATE UNIQUE INDEX sessions_native ON sessions (runtime, native_id)
    WHERE native_id IS NOT NULL AND state <> 'closed';

CREATE TABLE turns (
    id         INTEGER PRIMARY KEY,
    session_id INTEGER NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    seq        INTEGER NOT NULL CHECK (seq >= 1),
    kind       TEXT NOT NULL CHECK (kind IN ('prompt', 'retry', 'handoff', 'recovery', 'nudge')),
    message    TEXT NOT NULL,
    status     TEXT NOT NULL CHECK (status IN ('sent', 'completed', 'failed', 'interrupted')),
    outcome    TEXT,
    started_at TEXT NOT NULL,
    ended_at   TEXT,
    UNIQUE (session_id, seq)
) STRICT;

-- At most one turn in flight per session (runtime.Session is not safe for concurrent turns).
CREATE UNIQUE INDEX turns_one_sent ON turns (session_id) WHERE status = 'sent';
