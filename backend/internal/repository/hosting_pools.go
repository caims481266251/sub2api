package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (r *hostingRepository) ListPools(ctx context.Context, userID int64) ([]service.HostingPool, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT p.id, p.name,
 (SELECT COUNT(*) FROM accounts a WHERE a.owner_user_id=$1 AND a.deleted_at IS NULL AND COALESCE(a.hosting_pool_id,0)=p.id),
 (SELECT COUNT(*) FROM api_keys k WHERE k.user_id=$1 AND k.route_mode='hosted' AND k.deleted_at IS NULL AND COALESCE(k.hosting_pool_id,0)=p.id)
 FROM (SELECT 0::bigint AS id, ''::varchar AS name UNION ALL SELECT id,name FROM hosting_pools WHERE user_id=$1) p ORDER BY p.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	pools := make([]service.HostingPool, 0)
	for rows.Next() {
		var pool service.HostingPool
		if err := rows.Scan(&pool.ID, &pool.Name, &pool.AccountCount, &pool.KeyCount); err != nil {
			return nil, err
		}
		pools = append(pools, pool)
	}
	return pools, rows.Err()
}

func (r *hostingRepository) GetOwnedPool(ctx context.Context, userID, id int64) (*service.HostingPool, error) {
	var pool service.HostingPool
	err := r.db.QueryRowContext(ctx, "SELECT id,name FROM hosting_pools WHERE user_id=$1 AND id=$2", userID, id).Scan(&pool.ID, &pool.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrHostingPoolNotFound
	}
	return &pool, err
}

func (r *hostingRepository) SavePool(ctx context.Context, userID, id int64, name string) (*service.HostingPool, error) {
	pool := &service.HostingPool{}
	var err error
	if id == 0 {
		err = r.db.QueryRowContext(ctx, "INSERT INTO hosting_pools (user_id,name) VALUES ($1,$2) RETURNING id,name", userID, name).Scan(&pool.ID, &pool.Name)
	} else {
		err = r.db.QueryRowContext(ctx, "UPDATE hosting_pools SET name=$3, updated_at=NOW() WHERE user_id=$1 AND id=$2 RETURNING id,name", userID, id, name).Scan(&pool.ID, &pool.Name)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrHostingPoolNotFound
	}
	if err != nil {
		return nil, translatePersistenceError(err, nil, service.ErrHostingPoolNameExists)
	}
	return pool, nil
}

func (r *hostingRepository) DeletePool(ctx context.Context, userID, id int64) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var lockedID int64
	err = tx.QueryRowContext(ctx, "SELECT id FROM hosting_pools WHERE user_id=$1 AND id=$2 FOR UPDATE", userID, id).Scan(&lockedID)
	if errors.Is(err, sql.ErrNoRows) {
		return service.ErrHostingPoolNotFound
	}
	if err != nil {
		return err
	}
	var inUse bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE owner_user_id=$1 AND hosting_pool_id=$2 AND deleted_at IS NULL)
 OR EXISTS(SELECT 1 FROM api_keys WHERE user_id=$1 AND hosting_pool_id=$2 AND deleted_at IS NULL)`, userID, id).Scan(&inUse)
	if err != nil {
		return err
	}
	if inUse {
		return service.ErrHostingPoolInUse
	}
	if _, err = tx.ExecContext(ctx, "UPDATE accounts SET hosting_pool_id=NULL WHERE owner_user_id=$1 AND hosting_pool_id=$2 AND deleted_at IS NOT NULL", userID, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE api_keys SET hosting_pool_id=NULL WHERE user_id=$1 AND hosting_pool_id=$2 AND deleted_at IS NOT NULL", userID, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM hosting_pools WHERE user_id=$1 AND id=$2", userID, id); err != nil {
		return err
	}
	return tx.Commit()
}
