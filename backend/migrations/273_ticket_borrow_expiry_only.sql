-- Historical recovery settings are retained for audit, but no longer govern
-- acquisition. Upstream deadlines and request budgets are independent.
ALTER TABLE ticket_borrow_settings ADD COLUMN legacy_acquisition_settings JSONB;
UPDATE ticket_borrow_settings SET legacy_acquisition_settings=jsonb_build_object('prewarm_seconds',prewarm_seconds,'recovery',recovery),
 prewarm_seconds=0,recovery=(recovery || '{"enabled":false,"failure_threshold":0,"cooldown_seconds":[],"source_requests_per_minute":12}'::jsonb);
ALTER TABLE ticket_borrow_settings ALTER COLUMN prewarm_seconds SET DEFAULT 0;
ALTER TABLE ticket_borrow_settings ALTER COLUMN recovery SET DEFAULT '{"enabled":false,"failure_threshold":0,"cooldown_seconds":[],"probe_requests_per_minute":12,"source_requests_per_minute":12}'::jsonb;

ALTER TABLE ticket_borrow_targets
 ADD COLUMN upstream_wait_until TIMESTAMPTZ,
 ADD COLUMN upstream_wait_reason TEXT NOT NULL DEFAULT '',
 ADD COLUMN budget_wait_until TIMESTAMPTZ,
 ADD COLUMN legacy_recovery_state JSONB;
UPDATE ticket_borrow_targets SET legacy_recovery_state=jsonb_build_object('cooldown_until',cooldown_until,'cooldown_seconds',cooldown_seconds,'last_reason',last_reason,'recovery_level',recovery_level),
 -- The legacy reason is shared and may have been overwritten after a rate
 -- limit. Preserve every outstanding deadline once, without extending it.
 upstream_wait_until=CASE WHEN cooldown_until>clock_timestamp() THEN cooldown_until END,
 upstream_wait_reason=CASE WHEN cooldown_until>clock_timestamp() THEN CASE WHEN last_reason IN ('target_rate_limited','target_credentials_unavailable') THEN last_reason ELSE 'legacy_wait_unknown' END ELSE '' END,
 cooldown_until=NULL,cooldown_seconds=0,recovery_level=0,failures_since_cooldown=0,next_attempt_at=clock_timestamp();
-- Source next_attempt_at also mixed ordinary and upstream waits. Retain the
-- original deadline once; a shared last_reason cannot reliably distinguish them.

CREATE TABLE ticket_borrow_source_attempts (
 id BIGSERIAL PRIMARY KEY,source_id BIGINT NOT NULL,
 reserved_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX ticket_borrow_source_attempts_recent ON ticket_borrow_source_attempts(reserved_at);
