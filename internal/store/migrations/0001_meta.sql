-- Key/value facts about this state database (e.g. which OpenChef version created it).
-- Domain tables (tasks, steps, sessions, ...) arrive with the milestones that use them.
CREATE TABLE meta (
    key        TEXT NOT NULL PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;
