-- Opt-in rollout. Retired inventory remains readable by bound conversations.
ALTER TABLE ticket_borrow_settings ADD COLUMN replenishment JSONB NOT NULL DEFAULT '{"enabled":false,"target_ids":[],"inventory":2,"interval_seconds":10,"hold_seconds":150,"shadow":true}';
ALTER TABLE ticket_borrow_routes ADD COLUMN inventory_eligible BOOLEAN NOT NULL DEFAULT true,
 ADD COLUMN validation_only BOOLEAN NOT NULL DEFAULT false;
CREATE TABLE ticket_borrow_replenishment_holds (
 account_id BIGINT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
 environment_version BIGINT NOT NULL,
 until_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE ticket_borrow_replenishment (
 target_id BIGINT PRIMARY KEY REFERENCES ticket_borrow_targets(account_id) ON DELETE CASCADE,
 target_revision BIGINT NOT NULL,
 identity_hash TEXT NOT NULL,
 source_id BIGINT NOT NULL REFERENCES ticket_borrow_sources(id) ON DELETE CASCADE,
 source_revision BIGINT NOT NULL,
 environment_version BIGINT NOT NULL,
 rotation_revision BIGINT NOT NULL,
 settings_revision BIGINT NOT NULL,
 selection_revision BIGINT NOT NULL,
 policy_revision BIGINT NOT NULL,
 source_identity TEXT NOT NULL,
 target_identity TEXT NOT NULL,
 target_config TEXT NOT NULL,
 started_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 until_at TIMESTAMPTZ NOT NULL,
 next_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 reason TEXT NOT NULL DEFAULT 'success',
 attempts BIGINT NOT NULL DEFAULT 0
);
CREATE VIEW ticket_borrow_active_replenishment AS
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
 AND (cfg.replenishment->>'enabled')::boolean AND cfg.replenishment->'target_ids' @> to_jsonb(w.target_id)
 AND t.enabled AND ta.deleted_at IS NULL AND ta.status='active' AND ta.schedulable
 AND (NOT ta.auto_pause_on_expired OR ta.expires_at IS NULL OR ta.expires_at>clock_timestamp())
 AND encode(sha256(convert_to(COALESCE(ta.credentials->>'chatgpt_account_id','')||':'||COALESCE(ta.credentials->>'chatgpt_user_id',''),'UTF8')),'hex')=w.target_identity
 AND md5(jsonb_build_array(ta.proxy_id,ta.extra->'enable_tls_fingerprint',ta.extra->'tls_fingerprint_profile_id')::text)=w.target_config
 AND COALESCE(t.upstream_wait_until<=clock_timestamp(),true)
 AND (a.id IN (SELECT jsonb_array_elements_text(sel.account_ids)::bigint) OR EXISTS(SELECT 1 FROM account_groups ag JOIN groups g ON g.id=ag.group_id AND g.deleted_at IS NULL WHERE ag.account_id=a.id AND ag.group_id IN (SELECT jsonb_array_elements_text(sel.group_ids)::bigint)))
 AND NOT EXISTS(SELECT 1 FROM ticket_borrow_targets st WHERE st.account_id=a.id AND st.enabled)
 AND src.enabled AND src.deleted_at IS NULL AND a.deleted_at IS NULL AND a.status='active' AND a.schedulable
 AND (NOT a.auto_pause_on_expired OR a.expires_at IS NULL OR a.expires_at>clock_timestamp())
 AND COALESCE(a.rate_limit_reset_at<=clock_timestamp(),true) AND COALESCE(a.overload_until<=clock_timestamp(),true) AND COALESCE(a.temp_unschedulable_until<=clock_timestamp(),true)
 AND encode(sha256(convert_to(COALESCE(a.credentials->>'chatgpt_account_id','')||':'||COALESCE(a.credentials->>'chatgpt_user_id',''),'UTF8')),'hex')=w.source_identity
 AND COALESCE(rot.revision,0)=w.rotation_revision AND rot.active_job IS NULL
 AND w.until_at>clock_timestamp();
