package repository

import (
	"context"
	"database/sql"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (r *hostingRepository) Billing(ctx context.Context, userID, accountID int64, route string, page, size int) (service.HostingBilling, error) {
	result := service.HostingBilling{Page: page, PageSize: size, Items: []service.HostingBillingEntry{}}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return result, err
	}
	// Fallback bills identify the purchased group, not the platform's internal
	// account name (which may contain an upstream email or operations notes).
	defer tx.Rollback()
	// Every row is scoped to the authenticated owner, including fallback rows.
	where := ` WHERE l.user_id=$1 AND l.hosting_route IN ('hosted','fallback') AND ($2::bigint=0 OR (l.account_id=$2 AND l.hosting_route='hosted')) AND ($3::text='' OR l.hosting_route=$3)`
	err = tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(l.input_tokens::bigint+l.output_tokens+l.cache_creation_tokens+l.cache_read_tokens),0),COALESCE(sum(l.total_cost),0),COALESCE(sum(l.actual_cost),0),COALESCE(sum(l.actual_cost) FILTER(WHERE l.hosting_route='hosted'),0),COALESCE(sum(l.actual_cost) FILTER(WHERE l.hosting_route='fallback'),0) FROM usage_logs l`+where, userID, accountID, route).Scan(&result.Requests, &result.Tokens, &result.StandardCost, &result.ActualCost, &result.HostingCost, &result.FallbackCost)
	if err != nil {
		return result, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT l.id,l.created_at,COALESCE(a.name,''),COALESCE(k.name,''),l.model,l.hosting_route,COALESCE(g.name,''),COALESCE(l.hosting_fallback_reason,''),l.input_tokens::bigint+l.output_tokens+l.cache_creation_tokens+l.cache_read_tokens,l.total_cost,l.rate_multiplier,l.actual_cost,COALESCE(l.request_id,''),l.input_tokens,l.output_tokens,l.cache_read_tokens,l.cache_creation_tokens,l.input_cost,l.output_cost,l.cache_read_cost,l.cache_creation_cost FROM usage_logs l LEFT JOIN accounts a ON a.id=l.account_id AND a.owner_user_id=l.user_id AND l.hosting_route='hosted' LEFT JOIN api_keys k ON k.id=l.api_key_id LEFT JOIN groups g ON g.id=l.group_id`+where+` ORDER BY l.created_at DESC,l.id DESC LIMIT $4 OFFSET $5`, userID, accountID, route, size, (page-1)*size)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var entry service.HostingBillingEntry
		if err = rows.Scan(&entry.ID, &entry.CreatedAt, &entry.AccountName, &entry.KeyName, &entry.Model, &entry.Route, &entry.GroupName, &entry.Reason, &entry.Tokens, &entry.StandardCost, &entry.Rate, &entry.ActualCost, &entry.RequestID, &entry.InputTokens, &entry.OutputTokens, &entry.CacheReadTokens, &entry.CacheCreationTokens, &entry.InputCost, &entry.OutputCost, &entry.CacheReadCost, &entry.CacheCreationCost); err != nil {
			rows.Close()
			return result, err
		}
		result.Items = append(result.Items, entry)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	return result, tx.Commit()
}
