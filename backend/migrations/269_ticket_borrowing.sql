-- Dedicated donor credentials never participate in normal account scheduling.
CREATE TABLE ticket_borrow_sources (
 id BIGSERIAL PRIMARY KEY, name VARCHAR(100) NOT NULL,
 subject_hash VARCHAR(64) NOT NULL, subject_id TEXT NOT NULL, user_id TEXT NOT NULL DEFAULT '',
 secret TEXT NOT NULL, model VARCHAR(256) NOT NULL DEFAULT 'gpt-6-luna',
 enabled BOOLEAN NOT NULL DEFAULT true, revision BIGINT NOT NULL DEFAULT 1,
 rotate_identity BOOLEAN NOT NULL DEFAULT false,
 cursor BIGINT NOT NULL DEFAULT 0,
 lease_token TEXT NOT NULL DEFAULT '', lease_until TIMESTAMPTZ,
 next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 last_reason TEXT NOT NULL DEFAULT '', updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 deleted_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX ticket_borrow_sources_subject ON ticket_borrow_sources(subject_hash) WHERE deleted_at IS NULL;
CREATE TABLE ticket_borrow_targets (
 account_id BIGINT PRIMARY KEY REFERENCES accounts(id),
 enabled BOOLEAN NOT NULL DEFAULT false, revision BIGINT NOT NULL DEFAULT 1,
 cursor BIGINT NOT NULL DEFAULT 0,
 lease_token TEXT NOT NULL DEFAULT '', lease_until TIMESTAMPTZ,
 next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 refresh_requested BOOLEAN NOT NULL DEFAULT false,
 scan_sources JSONB NOT NULL DEFAULT '[]'::jsonb, scan_offset INTEGER NOT NULL DEFAULT 0,
 phase TEXT NOT NULL DEFAULT 'waiting', last_reason TEXT NOT NULL DEFAULT '',
 updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE ticket_borrow_routes (
 id TEXT PRIMARY KEY, target_id BIGINT NOT NULL REFERENCES ticket_borrow_targets(account_id),
 target_revision BIGINT NOT NULL, source_id BIGINT NOT NULL,
 identity_hash VARCHAR(64) NOT NULL, cookie_hash VARCHAR(64) NOT NULL,
 gateway TEXT NOT NULL, secret TEXT NOT NULL,
 checked_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), expires_at TIMESTAMPTZ NOT NULL,
 revoked_at TIMESTAMPTZ, reason TEXT NOT NULL DEFAULT '',
 UNIQUE(target_id, target_revision, identity_hash, cookie_hash)
);
CREATE INDEX ticket_borrow_routes_target ON ticket_borrow_routes(target_id, expires_at DESC) WHERE revoked_at IS NULL;
CREATE TABLE ticket_borrow_sessions (
 target_id BIGINT NOT NULL REFERENCES ticket_borrow_targets(account_id),
 session_hash VARCHAR(64) NOT NULL, route_id TEXT NOT NULL REFERENCES ticket_borrow_routes(id) ON DELETE CASCADE,
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY(target_id,session_hash)
);
CREATE TABLE ticket_borrow_rejections (
 target_id BIGINT NOT NULL, identity_hash VARCHAR(64) NOT NULL, cookie_hash VARCHAR(64) NOT NULL,
 expires_at TIMESTAMPTZ NOT NULL, PRIMARY KEY(target_id,identity_hash,cookie_hash)
);
-- Retain first observation independently of candidate eviction or source deletion.
CREATE TABLE ticket_borrow_cookie_deadlines (
 cookie_hash VARCHAR(64) PRIMARY KEY, expires_at TIMESTAMPTZ NOT NULL,
 observed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE ticket_borrow_events (
 id BIGSERIAL PRIMARY KEY, target_id BIGINT, source_id BIGINT,
 route_id TEXT, stage TEXT NOT NULL, reason TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE ticket_borrow_settings (
 id INTEGER PRIMARY KEY CHECK (id=1), revision BIGINT NOT NULL DEFAULT 1, running BOOLEAN NOT NULL DEFAULT true,
 ttl_seconds INTEGER NOT NULL DEFAULT 230 CHECK(ttl_seconds BETWEEN 30 AND 240),
 prewarm_seconds INTEGER NOT NULL DEFAULT 60 CHECK(prewarm_seconds BETWEEN 0 AND ttl_seconds)
);
INSERT INTO ticket_borrow_settings(id) VALUES(1);

CREATE TABLE ticket_borrow_transfers (
 account_id BIGINT PRIMARY KEY REFERENCES accounts(id),
 source_id BIGINT NOT NULL REFERENCES ticket_borrow_sources(id),
 transferred_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- An archived former donor must not be revived by a stale refresh/login worker.
CREATE FUNCTION guard_ticket_borrow_transfer() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF EXISTS(SELECT 1 FROM ticket_borrow_transfers WHERE account_id=OLD.id)
    AND (NEW.credentials IS DISTINCT FROM OLD.credentials OR NEW.deleted_at IS NULL) THEN
   RAISE EXCEPTION 'credential ownership transferred to borrowing';
 END IF;
 RETURN NEW;
END;
$$;
CREATE TRIGGER guard_ticket_borrow_transfer BEFORE UPDATE ON accounts
 FOR EACH ROW EXECUTE FUNCTION guard_ticket_borrow_transfer();

-- Serialize both directions of identity ownership changes, including imports
-- through the normal account APIs. Empty user IDs match conservatively.
CREATE FUNCTION guard_ticket_borrow_source_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 PERFORM pg_advisory_xact_lock(269001);
 IF NEW.deleted_at IS NULL AND EXISTS (
   SELECT 1 FROM accounts WHERE deleted_at IS NULL AND platform='openai'
    AND credentials->>'chatgpt_account_id'=NEW.subject_id
    AND (NEW.user_id='' OR COALESCE(credentials->>'chatgpt_user_id','')='' OR credentials->>'chatgpt_user_id'=NEW.user_id)
 ) THEN RAISE EXCEPTION 'borrow_source_is_normal_account'; END IF;
 IF NEW.deleted_at IS NULL AND EXISTS (
   SELECT 1 FROM ticket_borrow_sources WHERE id<>NEW.id AND deleted_at IS NULL AND subject_id=NEW.subject_id
    AND (user_id='' OR NEW.user_id='' OR user_id=NEW.user_id)
 ) THEN RAISE EXCEPTION 'borrow_duplicate_source'; END IF;
 RETURN NEW;
END;
$$;
CREATE TRIGGER guard_ticket_borrow_source_identity BEFORE INSERT OR UPDATE OF subject_id,user_id,deleted_at ON ticket_borrow_sources
 FOR EACH ROW EXECUTE FUNCTION guard_ticket_borrow_source_identity();
CREATE FUNCTION guard_normal_account_borrow_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.deleted_at IS NOT NULL OR NEW.platform<>'openai' OR COALESCE(NEW.credentials->>'chatgpt_account_id','')='' THEN RETURN NEW; END IF;
 PERFORM pg_advisory_xact_lock(269001);
 IF EXISTS(SELECT 1 FROM ticket_borrow_sources WHERE deleted_at IS NULL AND subject_id=NEW.credentials->>'chatgpt_account_id'
   AND (user_id='' OR COALESCE(NEW.credentials->>'chatgpt_user_id','')='' OR user_id=NEW.credentials->>'chatgpt_user_id')) THEN
   RAISE EXCEPTION 'credential ownership belongs to borrowing';
 END IF;
 RETURN NEW;
END;
$$;
CREATE TRIGGER guard_normal_account_borrow_identity BEFORE INSERT OR UPDATE OF credentials,deleted_at ON accounts
 FOR EACH ROW EXECUTE FUNCTION guard_normal_account_borrow_identity();
-- A login task queued from an old account snapshot must not acquire credentials
-- after their exclusive ownership moved to the source store.
CREATE FUNCTION guard_ticket_borrow_reauth_task() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 PERFORM 1 FROM accounts WHERE id=NEW.account_id FOR UPDATE;
 IF EXISTS(SELECT 1 FROM ticket_borrow_transfers WHERE account_id=NEW.account_id) THEN
  RAISE EXCEPTION 'credential ownership transferred to borrowing';
 END IF;
 RETURN NEW;
END;
$$;
CREATE TRIGGER guard_ticket_borrow_reauth_task BEFORE INSERT OR UPDATE OF status ON openai_oauth_reauth_tasks
 FOR EACH ROW EXECUTE FUNCTION guard_ticket_borrow_reauth_task();
