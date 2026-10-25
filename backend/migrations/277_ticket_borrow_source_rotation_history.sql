-- Rotation belongs to sources. Retire previously configured target jobs without
-- changing their existing environments, requests or published routes.
UPDATE ticket_borrow_rotation_accounts SET automatic=false,next_at=NULL,active_job=NULL,
 phase='cancelled',reason='rotation_source_only',revision=revision+1 WHERE role='target';
DELETE FROM ticket_borrow_rotation_assignments WHERE job_id IN (SELECT id FROM ticket_borrow_rotation_jobs WHERE role='target' AND finished_at IS NULL);
UPDATE ticket_borrow_rotation_jobs SET phase='cancelled',reason='rotation_source_only',finished_at=clock_timestamp() WHERE role='target' AND finished_at IS NULL;
DELETE FROM ticket_borrow_rotation_accounts WHERE role='target';
-- Pending source plans were selected without usage history; release those
-- reservations so the next request is planned under the new priorities.
UPDATE ticket_borrow_rotation_accounts SET active_job=NULL,phase='cancelled',reason='rotation_configuration_changed' WHERE role='source' AND active_job IS NOT NULL;
DELETE FROM ticket_borrow_rotation_assignments WHERE job_id IN (SELECT id FROM ticket_borrow_rotation_jobs WHERE role='source' AND finished_at IS NULL);
UPDATE ticket_borrow_rotation_jobs SET phase='cancelled',reason='rotation_configuration_changed',finished_at=clock_timestamp() WHERE role='source' AND finished_at IS NULL;
ALTER TABLE ticket_borrow_rotation_accounts DROP CONSTRAINT ticket_borrow_rotation_accounts_check;
ALTER TABLE ticket_borrow_rotation_accounts ADD CONSTRAINT ticket_borrow_rotation_source_automatic CHECK(role='source' OR NOT automatic);
ALTER TABLE ticket_borrow_rotation_accounts ADD COLUMN selection_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE ticket_borrow_rotation_assignments ADD COLUMN selection_kind TEXT NOT NULL DEFAULT '';

-- History is not removed with an account or seven-day event cleanup. Retired
-- pool entries retain their IDs when imported again, preserving global use.
CREATE TABLE ticket_borrow_proxy_usage (
 account_id BIGINT NOT NULL,
 entry_id BIGINT NOT NULL REFERENCES proxy_pool_entries(id),
 first_used_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 last_used_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 use_count BIGINT NOT NULL DEFAULT 1,
 PRIMARY KEY(account_id,entry_id)
);
CREATE INDEX ticket_borrow_proxy_usage_entry ON ticket_borrow_proxy_usage(entry_id);
CREATE TABLE ticket_borrow_fingerprint_history (
 fingerprint_hash TEXT PRIMARY KEY,
 tls_profile_hash TEXT,
 account_id BIGINT NOT NULL,
 entry_id BIGINT NOT NULL REFERENCES proxy_pool_entries(id),
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX ticket_borrow_fingerprint_tls ON ticket_borrow_fingerprint_history(tls_profile_hash);
INSERT INTO ticket_borrow_proxy_usage(account_id,entry_id,first_used_at,last_used_at)
 SELECT account_id,entry_id,created_at,created_at FROM ticket_borrow_proxy_bindings;
INSERT INTO ticket_borrow_fingerprint_history(fingerprint_hash,account_id,entry_id,created_at)
 SELECT encode(sha256(convert_to(device_id,'UTF8')),'hex'),account_id,entry_id,created_at FROM ticket_borrow_proxy_bindings ON CONFLICT DO NOTHING;
