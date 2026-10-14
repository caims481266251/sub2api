-- Business routes use the target account's existing shared concurrency budget.
-- Per-route leases remain for load balancing, accounting and natural drain.
-- Preserve route state, generations, quality results and all active leases.
ALTER TABLE ticket_borrow_routes DROP COLUMN IF EXISTS dispatch_limit;
