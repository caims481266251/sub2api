-- Ordinary outbound-proxy inventory. Credentials are encrypted and are not
-- copied into account configuration by pool import or replacement.
CREATE TABLE proxy_pool_state (
 id SMALLINT PRIMARY KEY CHECK (id=1),
 revision BIGINT NOT NULL DEFAULT 1,
 updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
INSERT INTO proxy_pool_state(id) VALUES(1);
CREATE TABLE proxy_pool_entries (
 id BIGSERIAL PRIMARY KEY,
 identity_hash TEXT NOT NULL UNIQUE,
 protocol TEXT NOT NULL CHECK (protocol IN ('http','https','socks5h')),
 host TEXT NOT NULL,
 port INTEGER NOT NULL CHECK (port BETWEEN 1 AND 65535),
 authenticated BOOLEAN NOT NULL,
 credential_ciphertext TEXT NOT NULL,
 enabled BOOLEAN NOT NULL DEFAULT true,
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 retired_at TIMESTAMPTZ
);
CREATE INDEX proxy_pool_entries_active ON proxy_pool_entries(id) WHERE retired_at IS NULL;
