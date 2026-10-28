-- Only new Docker accounts receive a system profile. Existing rows retain NULL.
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS docker_system_profile VARCHAR(64);
ALTER TABLE accounts ADD CONSTRAINT accounts_docker_system_profile_check CHECK (
    docker_system_profile IS NULL OR (
        execution_mode IN ('docker_kernel', 'vm_kernel') AND
        docker_system_profile IN ('centos-stream-9', 'ubuntu-24.04', 'debian-12', 'archlinux', 'fedora-44')
    )
);
CREATE INDEX IF NOT EXISTS idx_accounts_docker_system_bindings
    ON accounts (docker_system_profile)
    WHERE deleted_at IS NULL AND execution_mode IN ('docker_kernel', 'vm_kernel');

-- Guard raw SQL and imports as well as the immutable Ent field.
CREATE OR REPLACE FUNCTION preserve_account_docker_system_profile() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.docker_system_profile IS DISTINCT FROM OLD.docker_system_profile THEN
        RAISE EXCEPTION 'docker_system_profile is immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER accounts_docker_system_profile_immutable
    BEFORE UPDATE OF docker_system_profile ON accounts
    FOR EACH ROW EXECUTE FUNCTION preserve_account_docker_system_profile();
