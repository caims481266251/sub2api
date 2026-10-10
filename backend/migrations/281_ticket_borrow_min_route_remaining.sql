-- Apply the business admission floor to existing and new installations.
-- Zero preserves expiry-only admission for an explicit rollback.
ALTER TABLE ticket_borrow_settings ADD COLUMN IF NOT EXISTS min_route_remaining_seconds
    INTEGER NOT NULL DEFAULT 120 CHECK (min_route_remaining_seconds BETWEEN 0 AND 240);
