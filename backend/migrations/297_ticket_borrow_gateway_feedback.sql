-- Acquisition feedback is separate from business route eligibility.
CREATE TABLE ticket_borrow_gateway_feedback (
 target_id BIGINT NOT NULL REFERENCES ticket_borrow_targets(account_id) ON DELETE CASCADE,
 scope TEXT NOT NULL,
 gateway TEXT NOT NULL,
 reset_after TIMESTAMPTZ NOT NULL DEFAULT '-infinity',
 last_success_started_at TIMESTAMPTZ NOT NULL DEFAULT '-infinity',
 deprioritized_until TIMESTAMPTZ NOT NULL DEFAULT '-infinity',
 updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(target_id,scope,gateway)
);
CREATE TABLE ticket_borrow_gateway_outcomes (
 target_id BIGINT NOT NULL REFERENCES ticket_borrow_targets(account_id) ON DELETE CASCADE,
 scope TEXT NOT NULL,
 gateway TEXT NOT NULL,
 candidate_id TEXT NOT NULL,
 cookie_hash TEXT NOT NULL,
 started_at TIMESTAMPTZ NOT NULL,
 reason TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(target_id,scope,candidate_id),
 UNIQUE(target_id,scope,cookie_hash)
);
CREATE INDEX ticket_borrow_gateway_outcomes_recent ON ticket_borrow_gateway_outcomes(target_id,scope,gateway,created_at);

-- A self-acquisition reservation occupies a target background slot until the
-- real response completes. Crashed workers retain only a bounded reservation.
ALTER TABLE ticket_borrow_source_attempts ADD COLUMN lease_until TIMESTAMPTZ;
UPDATE ticket_borrow_source_attempts SET lease_until=reserved_at+INTERVAL '180 seconds' WHERE completed_at IS NULL;
CREATE INDEX ticket_borrow_source_attempts_inflight ON ticket_borrow_source_attempts(target_id,lease_until)
 WHERE source_id IS NULL AND completed_at IS NULL;
