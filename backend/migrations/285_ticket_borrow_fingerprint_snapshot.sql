-- Immutable snapshots include planned rotations as well as active bindings.
-- Existing devices keep the legacy algorithm until their next normal rotation.
ALTER TABLE ticket_borrow_fingerprint_history
 ADD COLUMN IF NOT EXISTS snapshot JSONB,
 ADD COLUMN IF NOT EXISTS client_hello_hash TEXT;
CREATE INDEX IF NOT EXISTS ticket_borrow_fingerprint_client_hello
 ON ticket_borrow_fingerprint_history(client_hello_hash);
-- A lost/corrupt v2 snapshot must never silently fall back to the v1 algorithm.
ALTER TABLE ticket_borrow_proxy_bindings ADD COLUMN IF NOT EXISTS fingerprint_version INTEGER NOT NULL DEFAULT 1;
ALTER TABLE ticket_borrow_rotation_assignments ADD COLUMN IF NOT EXISTS fingerprint_version INTEGER NOT NULL DEFAULT 1;
