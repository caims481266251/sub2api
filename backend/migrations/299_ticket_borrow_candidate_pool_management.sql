-- Capacity has its own revision: resizing the pool must not invalidate source
-- tasks, candidate claims, target leases or published business routes.
ALTER TABLE ticket_borrow_candidate_pool_control
 ADD COLUMN capacity INTEGER NOT NULL DEFAULT 64 CHECK (capacity BETWEEN 1 AND 1024),
 ADD COLUMN revision BIGINT NOT NULL DEFAULT 1;

CREATE INDEX ticket_borrow_candidate_pool_history ON ticket_borrow_candidate_pool(created_at DESC,id DESC);
CREATE INDEX ticket_borrow_candidate_pool_state_history ON ticket_borrow_candidate_pool(state,created_at DESC,id DESC);
CREATE INDEX ticket_borrow_pool_candidate_events ON ticket_borrow_events((details->>'candidate_id'),id DESC);
CREATE INDEX ticket_borrow_pool_candidate_routes ON ticket_borrow_routes(candidate_id);
