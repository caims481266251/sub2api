ALTER TABLE ticket_borrow_proxy_bindings
 ADD COLUMN role TEXT NOT NULL DEFAULT 'source' CHECK(role IN ('source','target')),
 ADD COLUMN environment_version BIGINT NOT NULL DEFAULT 1;

CREATE TABLE ticket_borrow_rotation_jobs (
 id TEXT PRIMARY KEY,
 role TEXT NOT NULL CHECK(role IN ('source','target')),
 trigger_kind TEXT NOT NULL CHECK(trigger_kind IN ('manual','automatic')),
 account_ids JSONB NOT NULL,
 phase TEXT NOT NULL DEFAULT 'draining',
 reason TEXT NOT NULL DEFAULT 'rotation_draining',
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 deadline TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()+INTERVAL '180 seconds',
 finished_at TIMESTAMPTZ
);
CREATE TABLE ticket_borrow_rotation_accounts (
 account_id BIGINT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
 role TEXT NOT NULL CHECK(role IN ('source','target')),
 revision BIGINT NOT NULL DEFAULT 1,
 automatic BOOLEAN NOT NULL DEFAULT false,
 interval_seconds INTEGER NOT NULL DEFAULT 60 CHECK(interval_seconds BETWEEN 1 AND 86400),
 next_at TIMESTAMPTZ,
 last_at TIMESTAMPTZ,
 phase TEXT NOT NULL DEFAULT 'idle',
 reason TEXT NOT NULL DEFAULT '',
 active_job TEXT REFERENCES ticket_borrow_rotation_jobs(id),
 CHECK(role='target' OR NOT automatic)
);
-- Reserve future assignments while old requests drain. This also supports
-- atomic permutations when every pool entry is already assigned.
CREATE TABLE ticket_borrow_rotation_assignments (
 job_id TEXT NOT NULL REFERENCES ticket_borrow_rotation_jobs(id) ON DELETE CASCADE,
 account_id BIGINT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
 entry_id BIGINT NOT NULL UNIQUE REFERENCES proxy_pool_entries(id),
 previous_entry_id BIGINT NOT NULL,
 device_id TEXT NOT NULL,
 environment_version BIGINT NOT NULL
);
CREATE INDEX ticket_borrow_rotation_due ON ticket_borrow_rotation_accounts(next_at) WHERE automatic AND active_job IS NULL;
CREATE INDEX ticket_borrow_rotation_active_jobs ON ticket_borrow_rotation_jobs(created_at) WHERE finished_at IS NULL;
CREATE INDEX ticket_borrow_rotation_finished_jobs ON ticket_borrow_rotation_jobs(finished_at) WHERE finished_at IS NOT NULL;
