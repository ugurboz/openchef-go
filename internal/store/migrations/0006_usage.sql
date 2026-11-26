-- Per-step cost history (L3, docs/architecture.md §10.1, §10.3): the share of its pool's
-- tightest window one attempt used, the input to admission estimates.
CREATE TABLE usage (
    id         INTEGER PRIMARY KEY,
    pool       TEXT NOT NULL,
    attempt_id INTEGER, -- for audit only: costs outlive the attempts they came from
    share      REAL NOT NULL CHECK (share >= 0 AND share <= 1),
    at         TEXT NOT NULL
) STRICT;

CREATE INDEX usage_pool ON usage (pool, id);

-- What API pools spent (§10.3: daily and monthly USD budgets).
CREATE TABLE spend (
    id   INTEGER PRIMARY KEY,
    pool TEXT NOT NULL,
    usd  REAL NOT NULL CHECK (usd >= 0),
    at   TEXT NOT NULL
) STRICT;

CREATE INDEX spend_pool_at ON spend (pool, at);
