ALTER TABLE ticket_borrow_settings ADD COLUMN route_quality jsonb NOT NULL DEFAULT
 '{"model":"gpt-6-astra","reasoning_effort":"medium","interval_seconds":30}';
ALTER TABLE ticket_borrow_routes ADD COLUMN quality_result jsonb NOT NULL DEFAULT '{}';
ALTER TABLE ticket_borrow_routes ADD COLUMN quality_valid_until timestamptz;
ALTER TABLE ticket_borrow_routes DROP CONSTRAINT ticket_borrow_routes_state_check;
ALTER TABLE ticket_borrow_routes ADD CONSTRAINT ticket_borrow_routes_state_check
 CHECK(state IN ('normal','pending_quality','pending_probe','cooling','expired','invalid'));
ALTER TABLE ticket_borrow_routes ALTER COLUMN state SET DEFAULT 'pending_quality';
-- Prior mint/continue results never imply answer quality. Sent leases survive.
UPDATE ticket_borrow_routes SET state='pending_probe',generation=generation+1,
 dispatch_limit=1,health_token='',health_until=NULL,health_checked_at=NULL
 WHERE state='normal' AND revoked_at IS NULL AND inventory_eligible AND NOT validation_only;
UPDATE ticket_borrow_routes SET health_token='',health_until=NULL WHERE health_token<>'';
UPDATE ticket_borrow_route_health SET token='',lease_until=NULL;

UPDATE ticket_borrow_settings SET recovery=jsonb_set(recovery,'{probe_requests_per_minute}','3'::jsonb),revision=revision+1 WHERE (recovery->>'probe_requests_per_minute')::int<3;
