-- Verified inventory is a refill target, never an eviction limit. Historical
-- admission_until values are immutable conservative deadlines; do not revive
-- routes already expired/retired by an older release.
ALTER TABLE ticket_borrow_settings
 ADD COLUMN route_pool_size INTEGER NOT NULL DEFAULT 4 CHECK(route_pool_size BETWEEN 1 AND 256),
 ADD COLUMN route_cooldown_seconds INTEGER NOT NULL DEFAULT 120 CHECK(route_cooldown_seconds BETWEEN 1 AND 86400);
ALTER TABLE ticket_borrow_routes
 ADD COLUMN state TEXT NOT NULL DEFAULT 'normal' CHECK(state IN ('normal','cooling','expired','invalid')),
 ADD COLUMN generation BIGINT NOT NULL DEFAULT 1,
 ADD COLUMN dispatch_limit INTEGER NOT NULL DEFAULT 1 CHECK(dispatch_limit>0),
 ADD COLUMN cooldown_until TIMESTAMPTZ,
 ADD COLUMN last_success_at TIMESTAMPTZ,
 ADD COLUMN last_business_success_at TIMESTAMPTZ,
 ADD COLUMN last_error_at TIMESTAMPTZ,
 ADD COLUMN last_error TEXT NOT NULL DEFAULT '',
 ADD COLUMN health_token TEXT NOT NULL DEFAULT '',
 ADD COLUMN health_until TIMESTAMPTZ,
 ADD COLUMN health_checked_at TIMESTAMPTZ,
 ADD COLUMN verification_model TEXT NOT NULL DEFAULT '',
 ADD COLUMN deadline_source TEXT NOT NULL DEFAULT 'legacy';
UPDATE ticket_borrow_routes SET state=CASE
 WHEN revoked_at IS NOT NULL OR NOT inventory_eligible OR validation_only THEN 'invalid'
 WHEN LEAST(expires_at,admission_until)<=clock_timestamp() THEN 'expired' ELSE 'normal' END,
 last_success_at=checked_at;
ALTER TABLE ticket_borrow_route_leases
 ADD COLUMN generation BIGINT NOT NULL DEFAULT 1,
 ADD COLUMN outcome_recorded BOOLEAN NOT NULL DEFAULT false;
CREATE INDEX ticket_borrow_route_pool_status ON ticket_borrow_routes(target_id,state,cooldown_until);
-- Single global check owner across application instances. A renewable lease
-- and a persisted start interval survive restarts without a burst of checks.
CREATE TABLE ticket_borrow_route_health (
 id INTEGER PRIMARY KEY CHECK(id=1),
 token TEXT NOT NULL DEFAULT '',
 lease_until TIMESTAMPTZ,
 last_started_at TIMESTAMPTZ,
 last_target_id BIGINT NOT NULL DEFAULT 0
);
INSERT INTO ticket_borrow_route_health(id) VALUES(1);
-- Remove the obsolete writable settings, including their compatibility aliases.
-- Keep historical route cutoffs and historical replenishment records unchanged.
DROP VIEW ticket_borrow_active_replenishment;
ALTER TABLE ticket_borrow_settings DROP COLUMN ttl_seconds, DROP COLUMN prewarm_seconds,
 DROP COLUMN min_route_remaining_seconds, DROP COLUMN admission_window_seconds, DROP COLUMN replenishment, DROP COLUMN renew_before_seconds;
ALTER TABLE ticket_borrow_targets DROP COLUMN renew_before_seconds;
