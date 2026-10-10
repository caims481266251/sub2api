# Database Migrations

## Overview

This directory contains SQL migration files for database schema changes. The migration system uses SHA256 checksums to ensure migration immutability and consistency across environments.

All SQL files are embedded in the application binary. On a fresh installation,
startup applies the full sequence to the empty database before serving requests,
including required initial rows. Operators do not import SQL or run a separate
migration command. Later starts verify checksums and apply only new scripts.

The proxy-pool and borrowing migration dependencies are also named explicitly in
`migrations.go`, so omitting one from a build context is a compilation error.
`TestProxyPoolFirstInstall` verifies the complete embedded sequence against an
empty database, then exercises the proxy-pool HTTP handlers through initial
loading, encryption setup, import, and a simulated restart.

## Migration File Naming

Format: `NNN_description.sql`
- `NNN`: Sequential number (e.g., 001, 002, 003)
- `description`: Brief description in snake_case

Example: `017_add_gemini_tier_id.sql`

### `_notx.sql` 命名与执行语义（并发索引专用）

当迁移包含 `CREATE INDEX CONCURRENTLY` 或 `DROP INDEX CONCURRENTLY` 时，必须使用 `_notx.sql` 后缀，例如：

- `062_add_accounts_priority_indexes_notx.sql`
- `063_drop_legacy_indexes_notx.sql`

运行规则：

1. `*.sql`（不带 `_notx`）按事务执行。
2. `*_notx.sql` 按非事务执行，不会包裹在 `BEGIN/COMMIT` 中。
3. `*_notx.sql` 仅允许并发索引语句，不允许混入事务控制语句或其他 DDL/DML。

幂等要求（必须）：

- 创建索引：`CREATE INDEX CONCURRENTLY IF NOT EXISTS ...`
- 删除索引：`DROP INDEX CONCURRENTLY IF EXISTS ...`

这样可以保证灾备重放、重复执行时不会因对象已存在/不存在而失败。

## Migration File Structure

This project uses a custom migration runner (`internal/repository/migrations_runner.go`) that executes the full SQL file content as-is.

- Regular migrations (`*.sql`): executed in a transaction.
- Non-transactional migrations (`*_notx.sql`): split by statement and executed without transaction (for `CONCURRENTLY`).

```sql
-- Forward-only migration (recommended)
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS example_column VARCHAR(100);
```

> ⚠️ Do **not** place executable "Down" SQL in the same file. The runner does not parse goose Up/Down sections and will execute all SQL statements in the file.

## Important Rules

### ⚠️ Immutability Principle

**Once a migration is applied to ANY environment (dev, staging, production), it MUST NOT be modified.**

Why?
- Each migration has a SHA256 checksum stored in the `schema_migrations` table
- Modifying an applied migration causes checksum mismatch errors
- Different environments would have inconsistent database states
- Breaks audit trail and reproducibility

### ✅ Correct Workflow

1. **Create new migration**
   ```bash
   # Create new file with next sequential number
   touch migrations/018_your_change.sql
   ```

2. **Write forward-only migration SQL**
   - Put only the intended schema change in the file
   - If rollback is needed, create a new migration file to revert

3. **Test locally**
   ```bash
   # From backend/, with Docker available. This creates a disposable database.
   go test -tags=integration ./internal/handler/admin -run '^TestProxyPoolFirstInstall$' -count=1
   ```
   For a disposable local PostgreSQL server, set
   `SUB2API_FRESH_INSTALL_TEST_POSTGRES_URL` to its PostgreSQL URL. The test creates
   and removes its own database. Never point this test at a production server.

4. **Commit and deploy**
   ```bash
   git add migrations/018_your_change.sql
   git commit -m "feat(db): add your change"
   ```

### ❌ What NOT to Do

- ❌ Modify an already-applied migration file
- ❌ Delete migration files
- ❌ Change migration file names
- ❌ Reorder migration numbers

### 🔧 If You Accidentally Modified an Applied Migration

**Error message:**
```
migration 017_add_gemini_tier_id.sql checksum mismatch (db=abc123... file=def456...)
```

**Solution:**
```bash
# 1. Find the original version
git log --oneline -- migrations/017_add_gemini_tier_id.sql

# 2. Revert to the commit when it was first applied
git checkout <commit-hash> -- migrations/017_add_gemini_tier_id.sql

# 3. Create a NEW migration for your changes
touch migrations/018_your_new_change.sql
```

## Migration System Details

- **Checksum Algorithm**: SHA256 of trimmed file content
- **Tracking Table**: `schema_migrations` (filename, checksum, applied_at)
- **Runner**: `internal/repository/migrations_runner.go`
- **Auto-run**: Migrations run automatically on service startup

## Best Practices

1. **Keep migrations small and focused**
   - One logical change per migration
   - Easier to review and rollback

2. **Write reversible migrations**
   - Always provide a working Down migration
   - Test rollback before committing

3. **Use transactions**
   - Wrap DDL statements in transactions when possible
   - Ensures atomicity

4. **Add comments**
   - Explain WHY the change is needed
   - Document any special considerations

5. **Test in development first**
   - Apply migration locally
   - Verify data integrity
   - Test rollback

## Example Migration

```sql
-- Add tier_id field to Gemini OAuth accounts for quota tracking
UPDATE accounts
SET credentials = jsonb_set(
    credentials,
    '{tier_id}',
    '"LEGACY"',
    true
)
WHERE platform = 'gemini'
  AND type = 'oauth'
  AND credentials->>'tier_id' IS NULL;
```

## Troubleshooting

### Checksum Mismatch
See "If You Accidentally Modified an Applied Migration" above.

### Migration Failed
```bash
# Check migration status
psql -d sub2api -c "SELECT * FROM schema_migrations ORDER BY applied_at DESC;"

# Manually rollback if needed (use with caution)
# Better to fix the migration and create a new one
```

### Need to Skip a Migration (Emergency Only)
```sql
-- DANGEROUS: Only use in development or with extreme caution
INSERT INTO schema_migrations (filename, checksum, applied_at)
VALUES ('NNN_migration.sql', 'calculated_checksum', NOW());
```

## References

- Migration runner: `internal/repository/migrations_runner.go`
- PostgreSQL docs: https://www.postgresql.org/docs/

### 281–282：路由业务准入与全局探测继承

281 新增全局最低剩余有效期，默认 120 秒。282 新增全局探测并发与提前量，以及目标账号覆盖标记；已有明确保存或非默认的策略保留为账号覆盖，新账号默认继承。两项迁移不修改历史路由、会话、请求或计费记录。

### Borrow probe renewal default (283)

Migration 283 sets the renewal default to 180 seconds for new global and target rows. It updates the untouched initial settings row (revision 1, old default 15) and preserves saved settings and existing account overrides. The service and UI fallback use the same default.

### Shared borrowing proxy assignments (284)

Migration 284 removes only entry-level uniqueness from source bindings and pending rotation assignments, retaining account primary keys and foreign keys. Non-unique entry indexes support allocation counts. The pool lock serializes planning, and each account keeps its own device identity. Existing bindings and histories remain intact.

### Borrowing fingerprint snapshots (285)

Migration 285 adds immutable identity/TLS snapshots and an indexed ClientHello structural comparison key to the fingerprint history. Active bindings and pending assignments carry a generator version; existing rows remain version 1 until normal rotation. Missing version 2 snapshots fail closed. The migration does not rotate sources, alter target identities, or remove history.

- `286_ticket_borrow_source_auth_exclusion.sql`: 保存来源 401 拒绝凭据的 SHA-256 标识；按同一凭据剔除借票来源，保留账号、路由和日志，新凭据可恢复。

### Normal account borrowing flow (287)

Migration 287 requires borrowing source proxy/fingerprint bindings and retires only old source auth/quota task delays. Migration 286's rejection columns remain audit metadata; normal account eligibility and recovery now control source admission. Account cooldowns, proxy settings, task budgets, routes and historical logs are preserved. A legacy binding without a complete fingerprint fails closed until normal source rotation creates a snapshot.
