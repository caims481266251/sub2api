-- Retain the original request for idempotency even when participants are skipped.
ALTER TABLE ticket_borrow_rotation_jobs
 ADD COLUMN requested_account_ids JSONB,
 ADD COLUMN skipped JSONB NOT NULL DEFAULT '[]'::jsonb;
UPDATE ticket_borrow_rotation_jobs SET requested_account_ids=account_ids;
ALTER TABLE ticket_borrow_rotation_jobs ALTER COLUMN requested_account_ids SET NOT NULL;
