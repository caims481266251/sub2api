-- Lightweight account monitoring is independent from user quality rules.
CREATE TABLE account_quality_monitors (
    account_id BIGINT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    identity_scope TEXT NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT false,
    model VARCHAR(160) NOT NULL,
    interval_minutes INTEGER NOT NULL DEFAULT 5 CHECK (interval_minutes BETWEEN 1 AND 1440),
    result_version BIGINT NOT NULL DEFAULT 1,
    next_due_at TIMESTAMPTZ,
    run_id TEXT,
    lease_until TIMESTAMPTZ,
    latest_attempt JSONB,
    last_valid JSONB
);
CREATE INDEX account_quality_monitors_due ON account_quality_monitors(next_due_at) WHERE enabled;
