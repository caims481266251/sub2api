package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"
	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/ent/user"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type hostingRepository struct {
	client *ent.Client
	db     *sql.DB
	cache  service.SchedulerCache
}

func NewHostingRepository(client *ent.Client, db *sql.DB, cache service.SchedulerCache) service.HostingRepository {
	return &hostingRepository{client, db, cache}
}

func (r *hostingRepository) GetUserPolicy(ctx context.Context, id int64) (service.HostingUserPolicy, error) {
	var p service.HostingUserPolicy
	err := r.db.QueryRowContext(ctx, "SELECT seat_limit, rate_multiplier FROM hosting_user_settings WHERE user_id = $1", id).Scan(&p.SeatLimit, &p.RateMultiplier)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return p, err
}
func (r *hostingRepository) SetUserPolicy(ctx context.Context, id int64, p service.HostingUserPolicy) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var lockedID int64
	if err = tx.QueryRowContext(ctx, "SELECT id FROM users WHERE id=$1 AND deleted_at IS NULL FOR UPDATE", id).Scan(&lockedID); err != nil {
		return service.ErrUserNotFound
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO hosting_user_settings (user_id,seat_limit,rate_multiplier) VALUES ($1,$2,$3) ON CONFLICT (user_id) DO UPDATE SET seat_limit=EXCLUDED.seat_limit,rate_multiplier=EXCLUDED.rate_multiplier,updated_at=NOW()`, id, p.SeatLimit, p.RateMultiplier)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (r *hostingRepository) ListOwned(ctx context.Context, id int64) ([]service.Account, error) {
	rows, err := r.client.Account.Query().Where(account.OwnerUserIDEQ(id), account.DeletedAtIsNil(), account.ParentAccountIDIsNil()).Order(ent.Desc(account.FieldID)).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]service.Account, 0, len(rows))
	for _, row := range rows {
		out = append(out, *accountEntityToService(row))
	}
	return out, nil
}
func (r *hostingRepository) SaveOwned(ctx context.Context, userID int64, a *service.Account, replaceID int64, seats int, settings ...*service.HostingAccountSettings) (*service.Account, error) {
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.User.Query().Where(user.IDEQ(userID), user.DeletedAtIsNil()).ForUpdate().Only(ctx); err != nil {
		return nil, service.ErrUserNotFound
	}
	// Serialize imports of the same upstream identity across different users.
	if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", a.GetChatGPTAccountID()); err != nil {
		return nil, err
	}
	// Public records may independently use the same upstream account.
	all, err := tx.Account.Query().Where(account.OwnerUserIDNotNil(), account.PlatformEQ(service.PlatformOpenAI), account.TypeEQ(service.AccountTypeOAuth), account.DeletedAtIsNil(), func(q *entsql.Selector) {
		q.Where(sqljson.ValueEQ(account.FieldCredentials, a.GetChatGPTAccountID(), sqljson.Path("chatgpt_account_id")))
	}).All(ctx)
	if err != nil {
		return nil, err
	}
	for _, existing := range all {
		identity := accountEntityToService(existing)
		if identity.GetChatGPTAccountID() == a.GetChatGPTAccountID() && (identity.GetCredential("chatgpt_user_id") == a.GetCredential("chatgpt_user_id") || identity.GetCredential("chatgpt_user_id") == "" || a.GetCredential("chatgpt_user_id") == "") && existing.ID != replaceID {
			return nil, service.ErrHostingIdentityConflict
		}
	}
	if replaceID == 0 {
		var override *int
		rows, err := tx.QueryContext(ctx, "SELECT seat_limit FROM hosting_user_settings WHERE user_id=$1", userID)
		if err != nil {
			return nil, err
		}
		if rows.Next() {
			err = rows.Scan(&override)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return nil, err
		}
		if override != nil {
			seats = *override
		}
		count, err := tx.Account.Query().Where(account.OwnerUserIDEQ(userID), account.DeletedAtIsNil(), account.ParentAccountIDIsNil()).Count(ctx)
		if err != nil {
			return nil, err
		}
		if count >= seats {
			return nil, service.ErrHostingSeatsFull
		}
		if err = createAccountRecord(ctx, tx.Client(), a); err != nil {
			return nil, translatePersistenceError(err, nil, service.ErrHostingIdentityConflict)
		}
	} else {
		existing, err := tx.Account.Query().Where(account.IDEQ(replaceID), account.OwnerUserIDEQ(userID), account.DeletedAtIsNil()).ForUpdate().Only(ctx)
		if err != nil {
			return nil, service.ErrHostingAccountNotFound
		}
		if existing.HostingIdentity == nil || *existing.HostingIdentity != *a.HostingIdentity {
			return nil, service.ErrHostingIdentityConflict
		}
		credentials := make(map[string]any, len(existing.Credentials)+len(a.Credentials))
		for key, value := range existing.Credentials {
			credentials[key] = value
		}
		for key, value := range a.Credentials {
			credentials[key] = value
		}
		a.Credentials = credentials
		_, err = tx.Account.UpdateOne(existing).SetName(a.Name).SetCredentials(a.Credentials).SetStatus(service.StatusActive).SetSchedulable(true).ClearErrorMessage().ClearRateLimitResetAt().ClearRateLimitedAt().ClearOverloadUntil().ClearTempUnschedulableUntil().ClearTempUnschedulableReason().Save(ctx)
		if err != nil {
			return nil, err
		}
		a.ID = replaceID
	}
	if len(settings) > 0 && settings[0] != nil {
		existing, err := tx.Account.Get(ctx, a.ID)
		if err != nil {
			return nil, err
		}
		b := tx.Account.UpdateOne(existing).SetUpdatedAt(time.Now())
		applyHostingSettingsMutation(b, existing, *settings[0])
		if _, err := b.Save(ctx); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	if r.cache != nil {
		_ = r.cache.DeleteAccount(ctx, a.ID)
	}
	_ = enqueueSchedulerOutbox(ctx, r.db, service.SchedulerOutboxEventAccountChanged, &a.ID, nil, nil)
	row, err := r.client.Account.Get(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	return accountEntityToService(row), nil
}
func (r *hostingRepository) ChangeOwned(ctx context.Context, userID, id int64, action string) error {
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.User.Query().Where(user.IDEQ(userID), user.DeletedAtIsNil()).ForUpdate().Only(ctx); err != nil {
		return service.ErrUserNotFound
	}
	b := tx.Account.Update().Where(account.IDEQ(id), account.OwnerUserIDEQ(userID), account.DeletedAtIsNil()).SetUpdatedAt(time.Now())
	switch action {
	case "delete":
		b.SetDeletedAt(time.Now()).SetSchedulable(false).SetCredentials(map[string]any{}).SetExtra(map[string]any{})
	case "pause":
		b.SetStatus(service.StatusDisabled).SetSchedulable(false)
	case "resume":
		b.Where(account.StatusNEQ(service.StatusError)).SetStatus(service.StatusActive).SetSchedulable(true)
	case "schedule-enable":
		b.SetSchedulable(true)
	case "schedule-disable":
		b.SetSchedulable(false)
	default:
		return service.ErrHostingAccountNotFound
	}
	n, err := b.Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return service.ErrHostingAccountNotFound
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	_ = enqueueSchedulerOutbox(ctx, r.db, service.SchedulerOutboxEventAccountChanged, &id, nil, nil)
	if r.cache != nil {
		_ = r.cache.DeleteAccount(ctx, id)
	}
	return nil
}
func (r *hostingRepository) Summary(ctx context.Context, id int64) (service.HostingSummary, error) {
	var out service.HostingSummary
	err := r.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(total_cost) FILTER (WHERE hosting_route='hosted'),0), COALESCE(SUM(actual_cost) FILTER (WHERE hosting_route='hosted'),0), COALESCE(SUM(actual_cost) FILTER (WHERE hosting_route='fallback'),0) FROM usage_logs WHERE user_id=$1 AND hosting_route IS NOT NULL`, id).Scan(&out.StandardCost, &out.HostingCost, &out.FallbackCost)
	return out, err
}

func (r *hostingRepository) UpdateOwnedSettings(ctx context.Context, userID, id int64, input service.HostingAccountSettings) (*service.Account, error) {
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	existing, err := tx.Account.Query().Where(account.IDEQ(id), account.OwnerUserIDEQ(userID), account.DeletedAtIsNil(), account.ParentAccountIDIsNil()).ForUpdate().Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, service.ErrHostingAccountNotFound
		}
		return nil, err
	}
	b := tx.Account.UpdateOne(existing).SetUpdatedAt(time.Now())
	applyHostingSettingsMutation(b, existing, input)
	_, err = b.Save(ctx)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	_ = enqueueSchedulerOutbox(ctx, r.db, service.SchedulerOutboxEventAccountChanged, &id, nil, nil)
	if r.cache != nil {
		_ = r.cache.DeleteAccount(ctx, id)
	}
	// Read through the existing credential query hook after the encryption
	// mutation hook; Save returns the encrypted storage representation.
	row, err := r.client.Account.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return accountEntityToService(row), nil
}

func applyHostingSettingsMutation(b *ent.AccountUpdateOne, existing *ent.Account, input service.HostingAccountSettings) {
	if input.Status != nil && *input.Status != existing.Status {
		b.SetStatus(*input.Status)
		if *input.Status == service.StatusActive {
			b.ClearErrorMessage()
		}
	}
	if input.HostingPoolID != nil {
		if *input.HostingPoolID == 0 {
			b.ClearHostingPoolID()
		} else {
			b.SetHostingPoolID(*input.HostingPoolID)
		}
	}
	if input.Name != nil {
		b.SetName(*input.Name)
	}
	if input.Concurrency != nil {
		b.SetConcurrency(*input.Concurrency)
	}
	if input.Priority != nil {
		b.SetPriority(*input.Priority)
	}
	if input.Notes != nil {
		b.SetNotes(*input.Notes)
	}
	if input.LoadFactor != nil {
		if *input.LoadFactor == 0 {
			b.ClearLoadFactor()
		} else {
			b.SetLoadFactor(*input.LoadFactor)
		}
	}
	if input.ExpiresAt != nil {
		if *input.ExpiresAt == 0 {
			b.ClearExpiresAt()
		} else {
			b.SetExpiresAt(time.Unix(*input.ExpiresAt, 0))
		}
	}
	if input.AutoPauseOnExpired != nil {
		b.SetAutoPauseOnExpired(*input.AutoPauseOnExpired)
	}
	// Patch the locked row instead of round-tripping a client snapshot. This
	// preserves refreshed tokens and server-owned quota/recovery state.
	if len(input.Credentials) > 0 {
		b.SetCredentials(mergeHostingSettings(existing.Credentials, input.Credentials))
	}
	if len(input.Extra) > 0 {
		b.SetExtra(service.PrepareHostedAccountSettingsExtra(accountEntityToService(existing), mergeHostingSettings(existing.Extra, input.Extra)))
	}
}

func mergeHostingSettings(current, patch map[string]any) map[string]any {
	merged := make(map[string]any, len(current)+len(patch))
	for key, value := range current {
		merged[key] = value
	}
	for key, value := range patch {
		if value == nil {
			delete(merged, key)
		} else {
			merged[key] = value
		}
	}
	return merged
}
