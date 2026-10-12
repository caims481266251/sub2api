-- Candidate ownership precedes target selection. Capacity is reserved before
-- source HTTP and remains occupied through verification, across all instances.
CREATE TABLE ticket_borrow_candidate_pool_control (
 id INTEGER PRIMARY KEY CHECK (id=1),
 source_cursor BIGINT NOT NULL DEFAULT 0,
 consumer_cursor BIGINT NOT NULL DEFAULT 0
);
INSERT INTO ticket_borrow_candidate_pool_control(id) VALUES(1);

CREATE TABLE ticket_borrow_candidate_pool (
 id TEXT PRIMARY KEY,
 owner_token TEXT NOT NULL,
 settings_revision BIGINT NOT NULL,
 source_id BIGINT NOT NULL REFERENCES ticket_borrow_sources(id),
 state TEXT NOT NULL DEFAULT 'acquiring' CHECK(state IN ('acquiring','queued','claimed','finished')),
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 lease_until TIMESTAMPTZ NOT NULL,
 enqueued_at TIMESTAMPTZ,
 admission_until TIMESTAMPTZ,
 cookie_hash TEXT,
 secret TEXT NOT NULL DEFAULT '',
 details JSONB NOT NULL DEFAULT '{}',
 target_id BIGINT REFERENCES ticket_borrow_targets(account_id) ON DELETE SET NULL,
 target_revision BIGINT,
 identity_hash TEXT,
 claim_token TEXT,
 claimed_at TIMESTAMPTZ,
 finished_at TIMESTAMPTZ,
 reason TEXT NOT NULL DEFAULT '',
 CHECK (state <> 'queued' OR (admission_until IS NOT NULL AND enqueued_at IS NOT NULL AND cookie_hash IS NOT NULL)),
 CHECK (state <> 'claimed' OR (claim_token IS NOT NULL AND claimed_at IS NOT NULL))
);
CREATE UNIQUE INDEX ticket_borrow_candidate_pool_cookie ON ticket_borrow_candidate_pool(cookie_hash) WHERE cookie_hash IS NOT NULL;
CREATE INDEX ticket_borrow_candidate_pool_pending ON ticket_borrow_candidate_pool(state,admission_until DESC) WHERE state<>'finished';
CREATE INDEX ticket_borrow_candidate_pool_target ON ticket_borrow_candidate_pool(target_id,claim_token) WHERE state='claimed';
CREATE INDEX ticket_borrow_candidate_pool_source ON ticket_borrow_candidate_pool(source_id) WHERE state='acquiring';

CREATE TABLE ticket_borrow_candidate_pool_demand (
 target_id BIGINT PRIMARY KEY REFERENCES ticket_borrow_targets(account_id) ON DELETE CASCADE,
 batch_id TEXT NOT NULL,
 identity_hash TEXT NOT NULL,
 slots INTEGER NOT NULL CHECK (slots BETWEEN 1 AND 100),
 until_at TIMESTAMPTZ NOT NULL,
 requested_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 granted_at TIMESTAMPTZ
);

ALTER TABLE ticket_borrow_candidate_environments ALTER COLUMN target_id DROP NOT NULL;
ALTER TABLE ticket_borrow_candidate_environments ADD COLUMN pool_candidate_id TEXT REFERENCES ticket_borrow_candidate_pool(id);
ALTER TABLE ticket_borrow_source_intents ALTER COLUMN target_id DROP NOT NULL;
ALTER TABLE ticket_borrow_source_intents ADD COLUMN pool_candidate_id TEXT REFERENCES ticket_borrow_candidate_pool(id);
ALTER TABLE ticket_borrow_source_attempts ADD COLUMN pool_candidate_id TEXT REFERENCES ticket_borrow_candidate_pool(id);
CREATE INDEX ticket_borrow_pool_source_http ON ticket_borrow_source_attempts(pool_candidate_id,lease_until) WHERE completed_at IS NULL;
CREATE INDEX ticket_borrow_pool_target_http ON ticket_borrow_requests((details->>'candidate_id'),lease_until) WHERE finished_at IS NULL;

-- No running flag, account state, timeout, budget or existing route is changed.
