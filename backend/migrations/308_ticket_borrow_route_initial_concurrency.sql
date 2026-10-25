-- Start new and recovered routes with three business slots. Account capacity,
-- quality validity and cooldown remain independent admission requirements.
ALTER TABLE ticket_borrow_routes ALTER COLUMN dispatch_limit SET DEFAULT 3;

-- Preserve grown windows, route generations and all in-flight leases.
UPDATE ticket_borrow_routes SET dispatch_limit=3
 WHERE dispatch_limit<3 AND revoked_at IS NULL AND inventory_eligible
 AND NOT validation_only AND state IN ('normal','pending_quality','pending_probe','cooling');
