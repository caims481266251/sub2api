CREATE TABLE hosting_pools (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id),
    name VARCHAR(100) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (user_id, id)
);
CREATE UNIQUE INDEX hosting_pools_user_name ON hosting_pools(user_id, lower(name));
ALTER TABLE accounts ADD COLUMN hosting_pool_id BIGINT;
ALTER TABLE accounts ADD CONSTRAINT accounts_hosting_pool_owner FOREIGN KEY (owner_user_id, hosting_pool_id) REFERENCES hosting_pools(user_id, id);
ALTER TABLE accounts ADD CONSTRAINT accounts_hosting_pool_private CHECK (hosting_pool_id IS NULL OR owner_user_id IS NOT NULL);
CREATE INDEX accounts_hosting_pool ON accounts(owner_user_id, hosting_pool_id) WHERE deleted_at IS NULL;
ALTER TABLE api_keys ADD COLUMN hosting_pool_id BIGINT;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_hosting_pool_owner FOREIGN KEY (user_id, hosting_pool_id) REFERENCES hosting_pools(user_id, id);
ALTER TABLE api_keys ADD CONSTRAINT api_keys_hosting_pool_route CHECK (hosting_pool_id IS NULL OR route_mode = 'hosted');
CREATE INDEX api_keys_hosting_pool ON api_keys(user_id, hosting_pool_id) WHERE deleted_at IS NULL;
