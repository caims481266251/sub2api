-- Diagnostic snapshots contain only allowlisted metadata and hashes. Keep all
-- existing rows and data; historical events simply have no candidate snapshot.
ALTER TABLE ticket_borrow_events
    ADD COLUMN IF NOT EXISTS details JSONB NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE ticket_borrow_requests
    ADD COLUMN IF NOT EXISTS details JSONB NOT NULL DEFAULT '{}'::jsonb;
