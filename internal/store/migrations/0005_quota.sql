-- Quota observations (docs/architecture.md §10.1, §11): what each source said about a
-- pool, kept so a restarted daemon routes by what it knew, and for `openchef quota`.
CREATE TABLE quota_observations (
    id        INTEGER PRIMARY KEY,
    pool      TEXT NOT NULL,
    source    TEXT NOT NULL CHECK (source IN ('L1', 'L2', 'L3')),
    at        TEXT NOT NULL,
    exhausted INTEGER NOT NULL DEFAULT 0 CHECK (exhausted IN (0, 1)),
    windows   TEXT NOT NULL DEFAULT '[]' -- JSON array of quota.Window
) STRICT;

CREATE INDEX quota_observations_at ON quota_observations (at);

-- Switches the user flips from the CLI (`openchef quota probes off`), so they outlive
-- the daemon without OpenChef rewriting the user's config.toml.
CREATE TABLE flags (
    name       TEXT NOT NULL PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;
