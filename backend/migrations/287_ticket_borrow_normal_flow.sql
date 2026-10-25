-- All new borrowing model requests require a persisted proxy/fingerprint.
-- Preserve account health, old auth rejection evidence, routes and request logs.
UPDATE ticket_borrow_selection
SET use_proxy_pool=true, rotate_identity=false, revision=revision+1
WHERE NOT use_proxy_pool OR rotate_identity;

-- Retire only old source auth/quota lease delays. Ordinary account cooldowns
-- remain authoritative; request-budget and rotation waits are untouched.
UPDATE ticket_borrow_sources
SET next_attempt_at=LEAST(next_attempt_at,clock_timestamp())
WHERE account_id IS NOT NULL AND deleted_at IS NULL
  AND last_reason IN ('source_auth_excluded','source_auth_response_stale',
      'source_auth_exclusion_failed','source_http_401','source_http_403',
      'source_http_429','source_rate_limited','source_credentials_unavailable');
