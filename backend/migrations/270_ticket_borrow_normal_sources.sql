-- Account credentials have one owner: the normal accounts table. References
-- keep their source IDs so published routes and historical events survive.
ALTER TABLE ticket_borrow_sources ADD COLUMN account_id BIGINT REFERENCES accounts(id);
CREATE UNIQUE INDEX ticket_borrow_sources_account ON ticket_borrow_sources(account_id) WHERE account_id IS NOT NULL;
ALTER TABLE ticket_borrow_transfers ADD COLUMN returned_at TIMESTAMPTZ;
CREATE TABLE ticket_borrow_selection (
 id INTEGER PRIMARY KEY CHECK(id=1), revision BIGINT NOT NULL DEFAULT 1,
 group_ids JSONB NOT NULL DEFAULT '[]', account_ids JSONB NOT NULL DEFAULT '[]',
 default_model VARCHAR(256) NOT NULL DEFAULT 'gpt-6-luna',
 rotate_identity BOOLEAN NOT NULL DEFAULT true,
 model_overrides JSONB NOT NULL DEFAULT '{}'
);
INSERT INTO ticket_borrow_selection(id) VALUES(1);

CREATE OR REPLACE FUNCTION guard_ticket_borrow_transfer() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF EXISTS(SELECT 1 FROM ticket_borrow_transfers WHERE account_id=OLD.id AND returned_at IS NULL)
    AND (NEW.credentials IS DISTINCT FROM OLD.credentials OR NEW.deleted_at IS NULL) THEN
   RAISE EXCEPTION 'credential ownership transferred to borrowing';
 END IF;
 RETURN NEW;
END;
$$;
CREATE OR REPLACE FUNCTION guard_ticket_borrow_source_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 PERFORM pg_advisory_xact_lock(269001);
 IF NEW.account_id IS NOT NULL THEN RETURN NEW; END IF;
 IF NEW.deleted_at IS NULL AND EXISTS (
   SELECT 1 FROM accounts WHERE deleted_at IS NULL AND platform='openai'
    AND credentials->>'chatgpt_account_id'=NEW.subject_id
    AND (NEW.user_id='' OR COALESCE(credentials->>'chatgpt_user_id','')='' OR credentials->>'chatgpt_user_id'=NEW.user_id)
 ) THEN RAISE EXCEPTION 'borrow_source_is_normal_account'; END IF;
 IF NEW.deleted_at IS NULL AND EXISTS (
   SELECT 1 FROM ticket_borrow_sources WHERE id<>NEW.id AND deleted_at IS NULL AND account_id IS NULL AND subject_id=NEW.subject_id
    AND (user_id='' OR NEW.user_id='' OR user_id=NEW.user_id)
 ) THEN RAISE EXCEPTION 'borrow_duplicate_source'; END IF;
 RETURN NEW;
END;
$$;
CREATE OR REPLACE FUNCTION guard_normal_account_borrow_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.deleted_at IS NOT NULL OR NEW.platform<>'openai' OR COALESCE(NEW.credentials->>'chatgpt_account_id','')='' THEN RETURN NEW; END IF;
 PERFORM pg_advisory_xact_lock(269001);
 IF EXISTS(SELECT 1 FROM ticket_borrow_sources WHERE deleted_at IS NULL AND account_id IS NULL AND subject_id=NEW.credentials->>'chatgpt_account_id'
   AND (user_id='' OR COALESCE(NEW.credentials->>'chatgpt_user_id','')='' OR user_id=NEW.credentials->>'chatgpt_user_id')) THEN
   RAISE EXCEPTION 'credential ownership belongs to borrowing';
 END IF;
 RETURN NEW;
END;
$$;

ALTER TABLE ticket_borrow_sources ADD CONSTRAINT ticket_borrow_reference_has_no_credentials
 CHECK(account_id IS NULL OR secret='');
