-- Preserve legacy rollout settings for audit; continuous maintenance requires an explicit opt-in.
CREATE TABLE ticket_borrow_inventory_upgrade AS SELECT clock_timestamp() AS saved_at,revision,replenishment FROM ticket_borrow_settings;
UPDATE ticket_borrow_settings SET replenishment=(replenishment-'shadow'-'target_ids') ||
 jsonb_build_object('enabled',false,'maintenance_interval_seconds',GREATEST(30,(replenishment->>'interval_seconds')::int)),revision=revision+1;
ALTER TABLE ticket_borrow_settings ALTER COLUMN replenishment SET DEFAULT '{"enabled":false,"inventory":2,"interval_seconds":10,"maintenance_interval_seconds":30,"hold_seconds":150}'::jsonb;
ALTER TABLE ticket_borrow_targets
 ADD COLUMN refresh_generation BIGINT NOT NULL DEFAULT 0,
 ADD COLUMN processed_generation BIGINT NOT NULL DEFAULT 0,
 ADD COLUMN lease_generation BIGINT NOT NULL DEFAULT 0,
 ADD COLUMN last_batch_started_at TIMESTAMPTZ,
 ADD COLUMN next_probe_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 ADD COLUMN last_inventory_state TEXT NOT NULL DEFAULT '',
 ADD COLUMN lease_settings_revision BIGINT,
 ADD COLUMN lease_policy_revision BIGINT,
 ADD COLUMN lease_identity TEXT NOT NULL DEFAULT '',
 ADD COLUMN lease_account_signature TEXT NOT NULL DEFAULT '';
CREATE INDEX ticket_borrow_targets_due ON ticket_borrow_targets(next_probe_at) WHERE enabled;
CREATE TABLE ticket_borrow_candidate_results (
 target_id BIGINT NOT NULL REFERENCES ticket_borrow_targets(account_id) ON DELETE CASCADE,
 target_revision BIGINT NOT NULL,identity_hash TEXT NOT NULL,cookie_hash TEXT NOT NULL,
 candidate_id TEXT NOT NULL,batch_id TEXT NOT NULL,verdict TEXT NOT NULL,publication TEXT NOT NULL,
 expires_at TIMESTAMPTZ NOT NULL,created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(target_id,target_revision,identity_hash,cookie_hash)
);
CREATE TABLE ticket_borrow_source_intents (
 id TEXT PRIMARY KEY,target_id BIGINT NOT NULL REFERENCES ticket_borrow_targets(account_id) ON DELETE CASCADE,
 batch_id TEXT NOT NULL,source_id BIGINT NOT NULL REFERENCES ticket_borrow_sources(id) ON DELETE CASCADE,
 category TEXT NOT NULL,queued_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),expires_at TIMESTAMPTZ NOT NULL,
 UNIQUE(target_id,batch_id,source_id)
);
CREATE TABLE ticket_borrow_source_arbiter (
 id INT PRIMARY KEY CHECK(id=1),cursor INT NOT NULL DEFAULT 0
);
INSERT INTO ticket_borrow_source_arbiter(id) VALUES(1);
CREATE TABLE ticket_borrow_source_grants (
 target_id BIGINT PRIMARY KEY REFERENCES ticket_borrow_targets(account_id) ON DELETE CASCADE,
 granted_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
ALTER TABLE ticket_borrow_source_attempts ADD COLUMN intent_id TEXT, ADD COLUMN target_id BIGINT,
 ADD COLUMN sent_at TIMESTAMPTZ, ADD COLUMN completed_at TIMESTAMPTZ;
CREATE UNIQUE INDEX ticket_borrow_source_attempt_intent ON ticket_borrow_source_attempts(intent_id) WHERE intent_id IS NOT NULL;
CREATE OR REPLACE VIEW ticket_borrow_active_replenishment AS
 SELECT w.*,src.account_id AS source_account_id,cfg.replenishment
 FROM ticket_borrow_replenishment w
 JOIN ticket_borrow_targets t ON t.account_id=w.target_id AND t.revision=w.target_revision AND t.probe_policy_revision=w.policy_revision
 JOIN accounts ta ON ta.id=t.account_id
 JOIN ticket_borrow_sources src ON src.id=w.source_id AND src.revision=w.source_revision
 JOIN accounts a ON a.id=src.account_id
 JOIN ticket_borrow_proxy_bindings b ON b.account_id=src.account_id AND b.role='source' AND b.environment_version=w.environment_version
 JOIN proxy_pool_entries p ON p.id=b.entry_id AND p.enabled AND p.retired_at IS NULL
 LEFT JOIN ticket_borrow_rotation_accounts rot ON rot.account_id=src.account_id
 CROSS JOIN ticket_borrow_settings cfg CROSS JOIN ticket_borrow_selection sel
 WHERE cfg.id=1 AND sel.id=1 AND cfg.running AND cfg.revision=w.settings_revision AND sel.revision=w.selection_revision AND sel.use_proxy_pool
 AND (cfg.replenishment->>'enabled')::boolean
 AND t.enabled AND ta.deleted_at IS NULL AND ta.status='active' AND ta.schedulable
 AND (NOT ta.auto_pause_on_expired OR ta.expires_at IS NULL OR ta.expires_at>clock_timestamp())
 AND encode(sha256(convert_to(COALESCE(ta.credentials->>'chatgpt_account_id','')||':'||COALESCE(ta.credentials->>'chatgpt_user_id',''),'UTF8')),'hex')=w.target_identity
 AND md5(jsonb_build_array(ta.proxy_id,ta.extra->'enable_tls_fingerprint',ta.extra->'tls_fingerprint_profile_id')::text)=w.target_config
 AND COALESCE(t.upstream_wait_until<=clock_timestamp(),true)
 AND (a.id IN (SELECT jsonb_array_elements_text(sel.account_ids)::bigint) OR EXISTS(SELECT 1 FROM account_groups ag JOIN groups g ON g.id=ag.group_id AND g.deleted_at IS NULL WHERE ag.account_id=a.id AND ag.group_id IN (SELECT jsonb_array_elements_text(sel.group_ids)::bigint)))
 AND NOT sel.excluded_account_ids @> to_jsonb(a.id)
 AND NOT EXISTS(SELECT 1 FROM ticket_borrow_targets st WHERE st.account_id=a.id AND st.enabled)
 AND src.enabled AND src.deleted_at IS NULL AND a.deleted_at IS NULL AND a.status='active' AND a.schedulable
 AND (NOT a.auto_pause_on_expired OR a.expires_at IS NULL OR a.expires_at>clock_timestamp())
 AND COALESCE(a.rate_limit_reset_at<=clock_timestamp(),true) AND COALESCE(a.overload_until<=clock_timestamp(),true) AND COALESCE(a.temp_unschedulable_until<=clock_timestamp(),true)
 AND encode(sha256(convert_to(COALESCE(a.credentials->>'chatgpt_account_id','')||':'||COALESCE(a.credentials->>'chatgpt_user_id',''),'UTF8')),'hex')=w.source_identity
 AND COALESCE(rot.revision,0)=w.rotation_revision AND rot.active_job IS NULL
 AND w.until_at>clock_timestamp();
