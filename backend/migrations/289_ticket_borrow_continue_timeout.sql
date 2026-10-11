-- Bound only borrowed target continuation requests; preserve other probe timeouts.
ALTER TABLE ticket_borrow_settings ADD COLUMN IF NOT EXISTS continue_timeout_seconds
    INTEGER NOT NULL DEFAULT 7 CHECK (continue_timeout_seconds BETWEEN 1 AND 45);
