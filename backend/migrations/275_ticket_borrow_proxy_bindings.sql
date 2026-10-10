-- Borrow-only egress assignments. Normal account proxies remain independently managed.
ALTER TABLE ticket_borrow_selection ADD COLUMN use_proxy_pool BOOLEAN NOT NULL DEFAULT false;
CREATE TABLE ticket_borrow_proxy_bindings (
 account_id BIGINT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
 entry_id BIGINT NOT NULL UNIQUE REFERENCES proxy_pool_entries(id),
 device_id TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
