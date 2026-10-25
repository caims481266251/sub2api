-- Historical pool attribution survives pool/key edits and deletion.
-- 0 identifies the default pool; NULL means no historical attribution.
ALTER TABLE usage_logs ADD COLUMN hosting_pool_id BIGINT;
ALTER TABLE usage_logs ADD COLUMN hosting_pool_name VARCHAR(100);

-- Only backfill when the current key and pool predate the usage record
-- and neither has been edited since. Never infer from a moved account.
UPDATE usage_logs u
SET hosting_pool_id = COALESCE(k.hosting_pool_id, 0),
    hosting_pool_name = COALESCE(p.name, '')
FROM api_keys k LEFT JOIN hosting_pools p ON p.id = k.hosting_pool_id AND p.user_id = k.user_id
WHERE u.api_key_id = k.id AND u.user_id = k.user_id
  AND u.hosting_route IN ('hosted', 'fallback') AND k.route_mode = 'hosted'
  AND k.updated_at <= u.created_at
  AND (k.hosting_pool_id IS NULL OR p.updated_at <= u.created_at);
