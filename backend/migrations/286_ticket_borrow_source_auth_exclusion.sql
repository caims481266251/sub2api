-- Remember the rejected credential without copying credentials or deleting
-- accounts/history. A new access token automatically retires this exclusion.
ALTER TABLE ticket_borrow_sources
 ADD COLUMN IF NOT EXISTS auth_rejected_token_hash TEXT NOT NULL DEFAULT '',
 ADD COLUMN IF NOT EXISTS auth_rejected_at TIMESTAMPTZ;
