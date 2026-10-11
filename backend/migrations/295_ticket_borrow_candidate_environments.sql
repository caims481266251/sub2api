-- Separate acquisition policy from business identity: changing acquisition
-- credentials must fence workers without revoking already admitted routes.
ALTER TABLE ticket_borrow_targets
 ADD COLUMN acquisition_identity TEXT NOT NULL DEFAULT 'self' CHECK (acquisition_identity IN ('self','source')),
 ADD COLUMN acquisition_revision BIGINT NOT NULL DEFAULT 1,
 ADD COLUMN lease_acquisition_revision BIGINT,
 ADD COLUMN acquisition_duration_ms BIGINT NOT NULL DEFAULT 0;
UPDATE ticket_borrow_targets SET acquisition_identity='source' WHERE enabled;

ALTER TABLE ticket_borrow_settings ADD COLUMN admission_window_seconds INTEGER NOT NULL DEFAULT 80
 CHECK (admission_window_seconds BETWEEN 1 AND 240);
-- An untouched installation has revision 3 after the earlier default migrations.
-- Preserve configured/existing deployments; initialize an unused installation at 80s.
UPDATE ticket_borrow_settings SET admission_window_seconds=CASE
 WHEN revision=3 AND ttl_seconds=230 AND min_route_remaining_seconds=120
 AND NOT EXISTS(SELECT 1 FROM ticket_borrow_targets) AND NOT EXISTS(SELECT 1 FROM ticket_borrow_sources)
 THEN 80 ELSE GREATEST(1,LEAST(240,ttl_seconds-min_route_remaining_seconds)) END,
 replenishment=jsonb_build_object('enabled',COALESCE((replenishment->>'enabled')::boolean,false),'inventory',2,'interval_seconds',1,'hold_seconds',0),
 revision=revision+1;
ALTER TABLE ticket_borrow_settings ALTER COLUMN replenishment SET DEFAULT '{"enabled":false,"inventory":2,"interval_seconds":1,"hold_seconds":0}'::jsonb;

ALTER TABLE ticket_borrow_routes ALTER COLUMN source_id DROP NOT NULL;
ALTER TABLE ticket_borrow_routes
 ADD COLUMN acquisition_identity TEXT NOT NULL DEFAULT 'source',
 ADD COLUMN acquisition_account_id BIGINT,
 ADD COLUMN acquisition_started_at TIMESTAMPTZ,
 ADD COLUMN admission_until TIMESTAMPTZ,
 ADD COLUMN candidate_id TEXT;
-- Legacy rows have no trustworthy acquisition start. Freeze their old cutoff.
UPDATE ticket_borrow_routes r SET admission_until=LEAST(r.expires_at,COALESCE(d.expires_at,r.expires_at))-cfg.min_route_remaining_seconds*INTERVAL '1 second'
 FROM ticket_borrow_settings cfg LEFT JOIN ticket_borrow_cookie_deadlines d ON true
 WHERE cfg.id=1 AND d.cookie_hash=r.cookie_hash;
UPDATE ticket_borrow_routes r SET admission_until=r.expires_at-cfg.min_route_remaining_seconds*INTERVAL '1 second'
 FROM ticket_borrow_settings cfg WHERE cfg.id=1 AND r.admission_until IS NULL;
ALTER TABLE ticket_borrow_routes ALTER COLUMN admission_until SET NOT NULL;
UPDATE ticket_borrow_routes r SET acquisition_account_id=s.account_id FROM ticket_borrow_sources s WHERE s.id=r.source_id;

-- Self acquisition has no source record. Its real account is always recorded.
ALTER TABLE ticket_borrow_source_intents ALTER COLUMN source_id DROP NOT NULL;
ALTER TABLE ticket_borrow_source_attempts ALTER COLUMN source_id DROP NOT NULL;
ALTER TABLE ticket_borrow_source_attempts ADD COLUMN acquisition_account_id BIGINT;

CREATE TABLE ticket_borrow_candidate_environments (
 id TEXT PRIMARY KEY,
 target_id BIGINT NOT NULL REFERENCES ticket_borrow_targets(account_id) ON DELETE CASCADE,
 batch_id TEXT NOT NULL,
 identity_key TEXT NOT NULL,
 entry_id BIGINT NOT NULL REFERENCES proxy_pool_entries(id),
 entry_identity TEXT NOT NULL,
 proxy_ciphertext TEXT NOT NULL,
 session_randomized BOOLEAN NOT NULL DEFAULT false,
 allocation_round BIGINT NOT NULL,
 fingerprint JSONB NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 lease_until TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()+INTERVAL '180 seconds',
 started_at TIMESTAMPTZ,
 sent_at TIMESTAMPTZ,
 admission_until TIMESTAMPTZ,
 released_at TIMESTAMPTZ
);
CREATE INDEX ticket_borrow_candidate_environments_active ON ticket_borrow_candidate_environments(identity_key,entry_id,lease_until) WHERE released_at IS NULL;
CREATE TABLE ticket_borrow_candidate_proxy_usage (
 identity_key TEXT NOT NULL,
 entry_id BIGINT NOT NULL REFERENCES proxy_pool_entries(id),
 uses BIGINT NOT NULL DEFAULT 0,
 round_offset BIGINT NOT NULL DEFAULT 0,
 available BOOLEAN NOT NULL DEFAULT true,
 last_used_at TIMESTAMPTZ,
 PRIMARY KEY(identity_key,entry_id)
);

-- Successful environments and account-bound rotation are historical only.
UPDATE ticket_borrow_replenishment SET until_at=LEAST(until_at,clock_timestamp()),reason='per_candidate_environment' WHERE reason='success';
DELETE FROM ticket_borrow_replenishment_holds;
UPDATE ticket_borrow_rotation_jobs SET phase='cancelled',reason='per_candidate_environment',finished_at=clock_timestamp() WHERE finished_at IS NULL;
UPDATE ticket_borrow_rotation_accounts SET automatic=false,active_job=NULL,phase='idle',reason='per_candidate_environment';
