-- Every pre-upgrade route must pass mint/continue before accepting new work.
-- Existing business leases remain untouched so sent requests can drain.
ALTER TABLE ticket_borrow_routes DROP CONSTRAINT ticket_borrow_routes_state_check;
ALTER TABLE ticket_borrow_routes ADD CONSTRAINT ticket_borrow_routes_state_check
 CHECK(state IN ('normal','pending_probe','cooling','expired','invalid'));
UPDATE ticket_borrow_routes SET state='pending_probe',generation=generation+1,
 dispatch_limit=1,health_token='',health_until=NULL,health_checked_at=NULL
 WHERE state='normal' AND revoked_at IS NULL AND inventory_eligible AND NOT validation_only
 AND LEAST(expires_at,admission_until)>clock_timestamp();
-- Fence any old ordinary health check, including cooling recovery.
UPDATE ticket_borrow_routes SET health_token='',health_until=NULL WHERE health_token<>'';
UPDATE ticket_borrow_route_health SET token='',lease_until=NULL;
ALTER TABLE ticket_borrow_settings ALTER COLUMN route_pool_size SET DEFAULT 128;
-- Move the previous default forward; preserve independently configured sizes.
UPDATE ticket_borrow_settings SET route_pool_size=128,revision=revision+1 WHERE route_pool_size=4;
