-- Business 429s retire only the affected route. Existing leases keep draining.
ALTER TABLE ticket_borrow_settings
 ADD COLUMN route_429_threshold INTEGER NOT NULL DEFAULT 2 CHECK (route_429_threshold > 0);

ALTER TABLE ticket_borrow_routes
 ADD COLUMN rate_limit_count BIGINT NOT NULL DEFAULT 0 CHECK (rate_limit_count >= 0);
