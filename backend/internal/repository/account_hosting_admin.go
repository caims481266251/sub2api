package repository

import (
	"context"

	"github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (r *hostingRepository) ListAllHosted(ctx context.Context, search string, page, pageSize int) ([]service.HostingAdminAccount, int, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	const scope = ` FROM accounts a JOIN users u ON u.id=a.owner_user_id LEFT JOIN hosting_pools p ON p.id=a.hosting_pool_id AND p.user_id=a.owner_user_id
 WHERE a.deleted_at IS NULL AND u.deleted_at IS NULL AND a.parent_account_id IS NULL AND a.platform='openai' AND a.type='oauth'
 AND ($1='' OR ($1 ~ '^[0-9]+$' AND u.id::text=$1) OR ($1 !~ '^[0-9]+$' AND (a.name ILIKE '%'||$1||'%' OR u.email ILIKE '%'||$1||'%' OR u.username ILIKE '%'||$1||'%' OR p.name ILIKE '%'||$1||'%')))`
	var total int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*)`+scope, search).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT a.id,u.id,u.email,u.username,COALESCE(p.name,'')`+scope+` ORDER BY a.id DESC LIMIT $2 OFFSET $3`, search, pageSize, (page-1)*pageSize)
	if err != nil {
		return nil, 0, err
	}
	items := make([]service.HostingAdminAccount, 0, pageSize)
	ids := make([]int64, 0, pageSize)
	for rows.Next() {
		var item service.HostingAdminAccount
		var id int64
		if err := rows.Scan(&id, &item.UserID, &item.UserEmail, &item.Username, &item.PoolName); err != nil {
			rows.Close()
			return nil, 0, err
		}
		item.Account.ID = id
		ids = append(ids, id)
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, 0, err
	}
	if len(ids) == 0 {
		return items, total, nil
	}
	entities, err := r.client.Account.Query().Where(account.IDIn(ids...), account.OwnerUserIDNotNil(), account.DeletedAtIsNil()).All(ctx)
	if err != nil {
		return nil, 0, err
	}
	byID := make(map[int64]service.Account, len(entities))
	for _, entity := range entities {
		byID[entity.ID] = *accountEntityToService(entity)
	}
	visible := make([]service.HostingAdminAccount, 0, len(items))
	for _, item := range items {
		if a, ok := byID[item.Account.ID]; ok && a.OwnerUserID != nil && *a.OwnerUserID == item.UserID {
			item.Account = a
			visible = append(visible, item)
		}
	}
	return visible, total, nil
}
