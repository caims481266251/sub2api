-- Proxy entries can serve multiple source accounts when the pool is smaller
-- than the source set. Account/device bindings and pending jobs stay distinct.
ALTER TABLE ticket_borrow_proxy_bindings
 DROP CONSTRAINT IF EXISTS ticket_borrow_proxy_bindings_entry_id_key;
ALTER TABLE ticket_borrow_rotation_assignments
 DROP CONSTRAINT IF EXISTS ticket_borrow_rotation_assignments_entry_id_key;
CREATE INDEX IF NOT EXISTS ticket_borrow_proxy_bindings_entry
 ON ticket_borrow_proxy_bindings(entry_id);
CREATE INDEX IF NOT EXISTS ticket_borrow_rotation_assignments_entry
 ON ticket_borrow_rotation_assignments(entry_id);
