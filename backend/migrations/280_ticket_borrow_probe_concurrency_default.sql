-- Only new targets receive the new default. Preserve saved per-target policies.
ALTER TABLE ticket_borrow_targets ALTER COLUMN probe_concurrency SET DEFAULT 2;
