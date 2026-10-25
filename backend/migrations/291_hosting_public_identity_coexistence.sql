-- Public and hosted records may independently reference the same upstream identity.
-- Keep identity uniqueness and all ownership/credential guards for hosted records.
CREATE OR REPLACE FUNCTION guard_hosted_account_identity() RETURNS trigger LANGUAGE plpgsql AS $$
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
    IF NEW.owner_user_id IS NOT NULL AND NEW.deleted_at IS NULL AND NEW.platform='openai' AND NEW.type='oauth' AND COALESCE(upstream_id,'')<>'' THEN
        PERFORM pg_advisory_xact_lock(hashtextextended(upstream_id,0));
        IF EXISTS(SELECT 1 FROM accounts a WHERE a.id<>NEW.id AND a.deleted_at IS NULL
            AND a.platform='openai' AND a.type='oauth' AND a.owner_user_id IS NOT NULL
            AND a.credentials->>'chatgpt_account_id'=upstream_id
            AND (COALESCE(a.credentials->>'chatgpt_user_id','')=COALESCE(upstream_user,'')
                OR COALESCE(a.credentials->>'chatgpt_user_id','')='' OR COALESCE(upstream_user,'')='')) THEN
            RAISE EXCEPTION 'account identity already hosted' USING ERRCODE='23505';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
