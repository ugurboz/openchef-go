-- Per-task permission mode (§9.1, M4.4). Tasks created before this column ran under
-- the M3 policy, which used acceptEdits (D-020).
ALTER TABLE tasks ADD COLUMN permission_mode TEXT NOT NULL DEFAULT 'acceptEdits'
    CHECK (permission_mode IN ('manual', 'acceptEdits', 'plan', 'auto', 'dontAsk', 'bypassPermissions'));
