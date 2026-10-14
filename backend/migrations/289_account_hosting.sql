ALTER TABLE accounts ADD COLUMN IF NOT EXISTS owner_user_id BIGINT REFERENCES users(id);
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS hosting_identity VARCHAR(64);
CREATE INDEX IF NOT EXISTS idx_accounts_hosting_owner ON accounts(owner_user_id) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_accounts_hosting_identity ON accounts(hosting_identity) WHERE deleted_at IS NULL AND hosting_identity IS NOT NULL;
ALTER TABLE accounts ADD CONSTRAINT accounts_hosting_platform CHECK (owner_user_id IS NULL OR (platform = 'openai' AND type = 'oauth'));
ALTER TABLE accounts ADD CONSTRAINT accounts_hosting_no_shadow CHECK (owner_user_id IS NULL OR parent_account_id IS NULL);
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS route_mode VARCHAR(20) NOT NULL DEFAULT 'group';
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS hosting_fallback_group_id BIGINT REFERENCES groups(id);
ALTER TABLE api_keys ADD CONSTRAINT api_keys_route_mode_check CHECK (route_mode IN ('group', 'hosted'));
ALTER TABLE api_keys ADD CONSTRAINT api_keys_hosted_group_check CHECK (route_mode <> 'hosted' OR group_id IS NULL);
CREATE TABLE IF NOT EXISTS hosting_user_settings (
    user_id BIGINT PRIMARY KEY REFERENCES users(id),
    seat_limit INTEGER CHECK (seat_limit >= 0),
    rate_multiplier NUMERIC(10,4) CHECK (rate_multiplier >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS hosting_route VARCHAR(20);
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS hosting_fallback_reason TEXT;
INSERT INTO settings (key, value) VALUES ('account_hosting', '{"enabled":false,"default_seats":1,"rate_multiplier":0.03,"fallback_group_id":null}') ON CONFLICT (key) DO NOTHING;

-- Guard every membership write, including bulk admin and direct repository paths.
CREATE FUNCTION guard_hosted_account_membership() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM 1 FROM accounts WHERE id=NEW.account_id AND owner_user_id IS NULL FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'hosted accounts cannot join public groups' USING ERRCODE='23514'; END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER hosted_account_membership_guard BEFORE INSERT OR UPDATE ON account_groups
FOR EACH ROW EXECUTE FUNCTION guard_hosted_account_membership();

CREATE FUNCTION guard_hosted_account_identity() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE upstream_id TEXT; upstream_user TEXT;
BEGIN
    IF TG_OP='UPDATE' AND OLD.owner_user_id IS NOT NULL AND
        (NEW.owner_user_id IS DISTINCT FROM OLD.owner_user_id OR NEW.hosting_identity IS DISTINCT FROM OLD.hosting_identity) THEN
        RAISE EXCEPTION 'hosted ownership is immutable' USING ERRCODE='23514';
    END IF;
    IF TG_OP='UPDATE' AND OLD.owner_user_id IS NOT NULL AND NEW.deleted_at IS NULL AND
        (NEW.credentials->>'chatgpt_account_id' IS DISTINCT FROM OLD.credentials->>'chatgpt_account_id'
         OR NEW.credentials->>'chatgpt_user_id' IS DISTINCT FROM OLD.credentials->>'chatgpt_user_id') THEN
        RAISE EXCEPTION 'hosted upstream identity is immutable' USING ERRCODE='23514';
    END IF;
    IF NEW.owner_user_id IS NOT NULL AND EXISTS(SELECT 1 FROM account_groups WHERE account_id=NEW.id) THEN
        RAISE EXCEPTION 'hosted accounts cannot join public groups' USING ERRCODE='23514';
    END IF;
    IF NEW.parent_account_id IS NOT NULL AND EXISTS(SELECT 1 FROM accounts WHERE id=NEW.parent_account_id AND owner_user_id IS NOT NULL) THEN
        RAISE EXCEPTION 'hosted accounts cannot create credential shadows' USING ERRCODE='23514';
    END IF;
    upstream_id:=NEW.credentials->>'chatgpt_account_id'; upstream_user:=NEW.credentials->>'chatgpt_user_id';
    IF NEW.owner_user_id IS NOT NULL AND EXISTS(SELECT 1 FROM jsonb_each_text(NEW.credentials) kv
        WHERE kv.key IN ('access_token','refresh_token','id_token') AND kv.value<>'' AND kv.value NOT LIKE 'hosted-token:v1:%') THEN
        RAISE EXCEPTION 'hosted tokens must be encrypted' USING ERRCODE='23514';
    END IF;
    IF NEW.deleted_at IS NULL AND NEW.platform='openai' AND NEW.type='oauth' AND COALESCE(upstream_id,'')<>'' THEN
        PERFORM pg_advisory_xact_lock(hashtextextended(upstream_id,0));
        IF EXISTS(SELECT 1 FROM accounts a WHERE a.id<>NEW.id AND a.deleted_at IS NULL
            AND a.platform='openai' AND a.type='oauth' AND (a.owner_user_id IS NOT NULL OR NEW.owner_user_id IS NOT NULL)
            AND a.credentials->>'chatgpt_account_id'=upstream_id
            AND (COALESCE(a.credentials->>'chatgpt_user_id','')=COALESCE(upstream_user,'')
                OR COALESCE(a.credentials->>'chatgpt_user_id','')='' OR COALESCE(upstream_user,'')='')) THEN
            RAISE EXCEPTION 'account identity already hosted or managed' USING ERRCODE='23505';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER hosted_account_identity_guard BEFORE INSERT OR UPDATE OF owner_user_id, hosting_identity, credentials, parent_account_id ON accounts
FOR EACH ROW EXECUTE FUNCTION guard_hosted_account_identity();
