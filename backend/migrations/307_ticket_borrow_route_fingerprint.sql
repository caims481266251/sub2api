-- Upgrade retained candidate snapshots to the explicit H2 template family.
-- Device IDs and every non-ALPN fingerprint parameter remain unchanged.
UPDATE ticket_borrow_candidate_environments SET fingerprint=
 jsonb_set(jsonb_set(jsonb_set(fingerprint,'{generator}','"borrow-h2-v3"'),
 '{profile,Name}',to_jsonb(replace(fingerprint->'profile'->>'Name','borrow-h1-v2-','borrow-h2-v3-'))),
 '{profile,ALPNProtocols}','["h2"]')
 WHERE fingerprint->>'generator'='borrow-h1-v2';

-- Recheck retained routes in the new business environment. Historical quality
-- answers remain audit data; existing business leases are never removed.
UPDATE ticket_borrow_routes r SET
 state=CASE WHEN EXISTS(SELECT 1 FROM ticket_borrow_candidate_environments e WHERE e.id=r.candidate_id) THEN
   CASE WHEN r.state='cooling' THEN 'cooling' ELSE 'pending_quality' END
   ELSE 'invalid' END,
 generation=r.generation+1,quality_valid_until=NULL,
 dispatch_limit=1,health_token='',health_until=NULL,health_checked_at=NULL
 WHERE r.revoked_at IS NULL AND r.inventory_eligible AND NOT r.validation_only
 AND r.state IN ('normal','pending_quality','pending_probe','cooling');

UPDATE ticket_borrow_targets SET next_probe_at=clock_timestamp(),next_attempt_at=clock_timestamp(),refresh_generation=refresh_generation+1
 WHERE enabled AND EXISTS(SELECT 1 FROM ticket_borrow_routes r WHERE r.target_id=account_id AND r.state IN ('pending_quality','cooling') AND r.revoked_at IS NULL);
