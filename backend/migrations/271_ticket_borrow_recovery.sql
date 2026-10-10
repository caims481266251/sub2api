-- Recovery controls acquisition only; route expiry/admission remain independent.
ALTER TABLE ticket_borrow_settings ADD COLUMN recovery JSONB NOT NULL DEFAULT '{"enabled":true,"failure_threshold":3,"cooldown_seconds":[60,120,180],"probe_requests_per_minute":12}'::jsonb;
ALTER TABLE ticket_borrow_targets
 ADD COLUMN cooldown_until TIMESTAMPTZ,
 ADD COLUMN cooldown_seconds INTEGER NOT NULL DEFAULT 0,
 ADD COLUMN recovery_level INTEGER NOT NULL DEFAULT 0,
 ADD COLUMN consecutive_failures INTEGER NOT NULL DEFAULT 0,
 ADD COLUMN failures_since_cooldown INTEGER NOT NULL DEFAULT 0,
 ADD COLUMN recovery_epoch BIGINT NOT NULL DEFAULT 0,
 ADD COLUMN last_request_started_at TIMESTAMPTZ,
 ADD COLUMN last_request_finished_at TIMESTAMPTZ;
-- One entry per dispatched target HTTP attempt. No prompt, credentials or cookies.
CREATE TABLE ticket_borrow_requests (
 id TEXT PRIMARY KEY, target_id BIGINT NOT NULL REFERENCES ticket_borrow_targets(account_id) ON DELETE CASCADE,
 kind TEXT NOT NULL CHECK(kind IN ('probe','business')), batch_id TEXT NOT NULL DEFAULT '',
 source_id BIGINT NOT NULL DEFAULT 0, phase TEXT NOT NULL DEFAULT '',
 reserved_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(), started_at TIMESTAMPTZ,
 finished_at TIMESTAMPTZ, lease_until TIMESTAMPTZ NOT NULL,
 outcome TEXT NOT NULL DEFAULT '', status INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX ticket_borrow_requests_recent ON ticket_borrow_requests(target_id,reserved_at DESC);
CREATE INDEX ticket_borrow_requests_inflight ON ticket_borrow_requests(target_id,lease_until) WHERE finished_at IS NULL;
