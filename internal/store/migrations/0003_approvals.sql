-- The approval queue (docs/architecture.md §9.2, D-022): permission requests and agent
-- questions waiting for the user. Rows are kept after they are settled, as the audit
-- trail of what was allowed and who decided.

CREATE TABLE approvals (
    id         INTEGER PRIMARY KEY,
    task_id    TEXT NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    agent      TEXT NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN ('command', 'file_change', 'tool', 'mode_change', 'question')),
    status     TEXT NOT NULL CHECK (status IN ('pending', 'allowed', 'denied', 'answered', 'expired', 'withdrawn')),
    tool       TEXT,
    summary    TEXT NOT NULL,
    options    TEXT NOT NULL DEFAULT '[]', -- JSON array of strings
    request    TEXT,                       -- the normalized request as JSON, for audit
    answer     TEXT,
    decided_by TEXT CHECK (decided_by IN ('user', 'timeout', 'daemon')),
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    decided_at TEXT,
    CHECK ((status = 'pending') = (decided_by IS NULL)),
    CHECK ((status = 'pending') = (decided_at IS NULL))
) STRICT;

CREATE INDEX approvals_pending ON approvals (created_at) WHERE status = 'pending';
CREATE INDEX approvals_task ON approvals (task_id, id);
