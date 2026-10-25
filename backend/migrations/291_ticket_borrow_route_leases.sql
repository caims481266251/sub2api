-- Business occupancy is shared across instances; account concurrency remains
-- owned by the existing account scheduler. Probes never reserve these leases.
CREATE TABLE ticket_borrow_route_leases (
 id TEXT PRIMARY KEY,
 target_id BIGINT NOT NULL REFERENCES ticket_borrow_targets(account_id) ON DELETE CASCADE,
 route_id TEXT NOT NULL REFERENCES ticket_borrow_routes(id) ON DELETE CASCADE,
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 lease_until TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()+INTERVAL '60 seconds'
);
CREATE INDEX ticket_borrow_route_leases_load ON ticket_borrow_route_leases(route_id,lease_until);
CREATE INDEX ticket_borrow_route_leases_expiry ON ticket_borrow_route_leases(lease_until);
