-- Schema v2 had no explicit untrack command, so every inactive row was
-- inferred from a live filesystem deletion. Restore those registrations once;
-- schema v3 inactive rows are created only by explicit user action.
UPDATE watch_tracking SET active = 1 WHERE active = 0;
