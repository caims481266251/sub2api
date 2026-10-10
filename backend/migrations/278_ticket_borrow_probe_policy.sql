-- Probe policy has its own revision: changing acquisition scheduling must not
-- invalidate routes or sessions that are still serving business traffic.
ALTER TABLE ticket_borrow_targets
 ADD COLUMN probe_policy_revision BIGINT NOT NULL DEFAULT 1,
 ADD COLUMN probe_concurrency INTEGER NOT NULL DEFAULT 3 CHECK(probe_concurrency BETWEEN 1 AND 32),
 ADD COLUMN renew_before_seconds INTEGER NOT NULL DEFAULT 15 CHECK(renew_before_seconds BETWEEN 0 AND 240);

CREATE INDEX ticket_borrow_requests_active_probes ON ticket_borrow_requests(target_id,lease_until)
 WHERE kind='probe' AND finished_at IS NULL;

-- Retain historical business rows, but probe status must describe probes only.
UPDATE ticket_borrow_targets t SET
 last_request_started_at=(SELECT max(started_at) FROM ticket_borrow_requests x WHERE x.target_id=t.account_id AND x.kind='probe'),
 last_request_finished_at=(SELECT max(finished_at) FROM ticket_borrow_requests x WHERE x.target_id=t.account_id AND x.kind='probe');
