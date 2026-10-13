-- Renewal estimates look up the last publication within the current identity.
-- Keep this lookup bounded as route history grows; no settings or data reset.
CREATE INDEX ticket_borrow_routes_search_anchor
 ON ticket_borrow_routes(target_id,target_revision,identity_hash,checked_at DESC)
 WHERE NOT validation_only;
