-- Keep repeated source/target/environment incompatibilities from consuming the
-- whole proxy pool. The key is a one-way hash assembled by the service from
-- target identity, source, gateway, pool entry and source Cookie pair.
CREATE TABLE ticket_borrow_compatibility (
 target_id BIGINT NOT NULL REFERENCES ticket_borrow_targets(account_id) ON DELETE CASCADE,
 identity_hash TEXT NOT NULL,
 compatibility_hash TEXT NOT NULL,
 consecutive_failures INTEGER NOT NULL DEFAULT 0,
 last_reason TEXT NOT NULL DEFAULT '',
 next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 last_success_at TIMESTAMPTZ,
 last_failure_at TIMESTAMPTZ,
 updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(target_id, identity_hash, compatibility_hash)
);
CREATE INDEX ticket_borrow_compatibility_due
 ON ticket_borrow_compatibility(target_id, identity_hash, next_attempt_at);
