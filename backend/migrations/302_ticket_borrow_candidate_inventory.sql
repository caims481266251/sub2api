-- Candidate inventory follows the observed Cookie deadline. Execution leases
-- still fence workers; neither target ownership nor route admission is a TTL
-- for queued inventory. Do not revive candidates completed by older versions.
ALTER TABLE ticket_borrow_candidate_environments ADD COLUMN cookie_hash TEXT;
ALTER TABLE ticket_borrow_candidate_pool
 ADD COLUMN source_credential_hash TEXT NOT NULL DEFAULT '',
 ADD COLUMN source_subject_hash TEXT NOT NULL DEFAULT '';

-- Preserve the exact admission cutoff of existing routes. New publications
-- explicitly use their first publication instant as this immutable anchor.
ALTER TABLE ticket_borrow_routes ADD COLUMN admission_started_at TIMESTAMPTZ;
UPDATE ticket_borrow_routes SET admission_started_at=acquisition_started_at;

-- Old queued payloads do not prove that their environment/deadline survived
-- independently of the former 15-second queue lease. Refill normally instead
-- of extending them during migration. Claimed/sent work keeps its lease.
WITH finished AS (
 UPDATE ticket_borrow_candidate_pool SET state='finished',finished_at=clock_timestamp(),secret='',
 reason='candidate_pool_lifecycle_upgraded' WHERE state='queued' RETURNING id,source_id,details
), released AS (
 UPDATE ticket_borrow_candidate_environments e SET released_at=COALESCE(released_at,clock_timestamp())
 FROM finished f WHERE e.id=f.id
)
INSERT INTO ticket_borrow_events(source_id,stage,reason,details)
SELECT source_id,'candidate','candidate_pool_lifecycle_upgraded',details||jsonb_build_object('candidate_id',id) FROM finished;
