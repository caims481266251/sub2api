package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

type accountQualityMonitorRepository struct{ db *sql.DB }

func NewAccountQualityMonitorRepository(db *sql.DB) service.AccountQualityMonitorRepository {
	return &accountQualityMonitorRepository{db: db}
}

const qualityMonitorColumns = `account_id,identity_scope,enabled,model,interval_minutes,result_version,next_due_at,run_id,lease_until,latest_attempt,last_valid`
const qualityMonitorSummaryColumns = `account_id,identity_scope,enabled,model,interval_minutes,result_version,next_due_at,run_id,lease_until,latest_attempt-'text',last_valid-'text'`

func scanQualityMonitor(row scannable) (*service.AccountQualityMonitorRecord, error) {
	r := &service.AccountQualityMonitorRecord{}
	var next, lease sql.NullTime
	var run sql.NullString
	var latest, valid []byte
	if err := row.Scan(&r.AccountID, &r.Scope, &r.Enabled, &r.Model, &r.IntervalMinutes, &r.ResultVersion, &next, &run, &lease, &latest, &valid); err != nil {
		return nil, err
	}
	if next.Valid {
		r.NextDueAt = &next.Time
	}
	if lease.Valid {
		r.LeaseUntil = &lease.Time
	}
	r.RunID = run.String
	if len(latest) > 0 {
		if err := json.Unmarshal(latest, &r.LatestAttempt); err != nil {
			return nil, err
		}
	}
	if len(valid) > 0 {
		if err := json.Unmarshal(valid, &r.LastValid); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (r *accountQualityMonitorRepository) Get(ctx context.Context, id int64, details bool) (*service.AccountQualityMonitorRecord, error) {
	columns := qualityMonitorSummaryColumns
	if details {
		columns = qualityMonitorColumns
	}
	record, err := scanQualityMonitor(r.db.QueryRowContext(ctx, `SELECT `+columns+` FROM account_quality_monitors WHERE account_id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return record, err
}

func (r *accountQualityMonitorRepository) Batch(ctx context.Context, ids []int64) (map[int64]*service.AccountQualityMonitorRecord, error) {
	result := make(map[int64]*service.AccountQualityMonitorRecord, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	rows, err := r.db.QueryContext(ctx, `SELECT `+qualityMonitorSummaryColumns+` FROM account_quality_monitors WHERE account_id=ANY($1)`, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		record, err := scanQualityMonitor(rows)
		if err != nil {
			return nil, err
		}
		result[record.AccountID] = record
	}
	return result, rows.Err()
}

func (r *accountQualityMonitorRepository) Save(ctx context.Context, id int64, config service.AccountQualityMonitorConfig, now time.Time) (*service.AccountQualityMonitorRecord, error) {
	return r.save(ctx, id, config, now, false)
}

// Ensure initializes a new identity without overwriting a configuration that
// another request has already saved for that same identity.
func (r *accountQualityMonitorRepository) Ensure(ctx context.Context, id int64, config service.AccountQualityMonitorConfig, now time.Time) (*service.AccountQualityMonitorRecord, error) {
	return r.save(ctx, id, config, now, true)
}

func (r *accountQualityMonitorRepository) save(ctx context.Context, id int64, config service.AccountQualityMonitorConfig, now time.Time, onlyNewIdentity bool) (*service.AccountQualityMonitorRecord, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	account, scope, err := lockQualityMonitorScope(ctx, tx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrAccountNotFound
	}
	if err != nil {
		return nil, err
	}
	if scope != config.Scope {
		return nil, service.ErrAccountQualityScopeChanged
	}
	if !service.IsAccountQualityTextModel(account, config.Model) {
		return nil, service.ErrAccountQualityTextModel
	}
	condition := ""
	if onlyNewIdentity {
		condition = " WHERE q.identity_scope<>EXCLUDED.identity_scope"
	}
	record, err := scanQualityMonitor(tx.QueryRowContext(ctx, `INSERT INTO account_quality_monitors AS q
 (account_id,identity_scope,enabled,model,interval_minutes,next_due_at)
 VALUES($1,$2,$3,$4,$5,CASE WHEN $3 THEN $6::timestamptz ELSE NULL END)
 ON CONFLICT(account_id) DO UPDATE SET
 identity_scope=EXCLUDED.identity_scope,enabled=EXCLUDED.enabled,model=EXCLUDED.model,interval_minutes=EXCLUDED.interval_minutes,
 result_version=q.result_version+CASE WHEN q.identity_scope<>EXCLUDED.identity_scope OR q.model<>EXCLUDED.model THEN 1 ELSE 0 END,
 latest_attempt=CASE WHEN q.identity_scope<>EXCLUDED.identity_scope OR q.model<>EXCLUDED.model THEN NULL ELSE q.latest_attempt END,
 last_valid=CASE WHEN q.identity_scope<>EXCLUDED.identity_scope OR q.model<>EXCLUDED.model THEN NULL ELSE q.last_valid END,
 next_due_at=CASE WHEN NOT EXCLUDED.enabled THEN NULL
 WHEN NOT q.enabled OR q.identity_scope<>EXCLUDED.identity_scope OR q.model<>EXCLUDED.model THEN $6::timestamptz
 ELSE COALESCE((q.latest_attempt->>'checked_at')::timestamptz+make_interval(mins=>EXCLUDED.interval_minutes),$6::timestamptz) END
 `+condition+` RETURNING `+qualityMonitorColumns, id, config.Scope, config.Enabled, config.Model, config.IntervalMinutes, now))
	if errors.Is(err, sql.ErrNoRows) && onlyNewIdentity {
		record, err = scanQualityMonitor(tx.QueryRowContext(ctx, `SELECT `+qualityMonitorColumns+` FROM account_quality_monitors WHERE account_id=$1`, id))
	}
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return record, nil
}

func (r *accountQualityMonitorRepository) ListDue(ctx context.Context, now time.Time, limit int) ([]*service.AccountQualityMonitorRecord, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+qualityMonitorSummaryColumns+` FROM account_quality_monitors
 WHERE enabled AND next_due_at<=$1 AND (lease_until IS NULL OR lease_until<=$1)
 AND EXISTS(SELECT 1 FROM accounts WHERE id=account_id AND deleted_at IS NULL)
 ORDER BY next_due_at,account_id LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []*service.AccountQualityMonitorRecord
	for rows.Next() {
		record, err := scanQualityMonitor(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (r *accountQualityMonitorRepository) Claim(ctx context.Context, record *service.AccountQualityMonitorRecord, runID string, now, until time.Time, automatic bool) (bool, error) {
	result, err := r.db.ExecContext(ctx, `UPDATE account_quality_monitors SET run_id=$4,lease_until=$6
 WHERE account_id=$1 AND identity_scope=$2 AND result_version=$3
 AND (lease_until IS NULL OR lease_until<=$5)
 AND (NOT $7 OR (enabled AND next_due_at<=$5))
 AND EXISTS(SELECT 1 FROM accounts WHERE id=account_id AND deleted_at IS NULL)`, record.AccountID, record.Scope, record.ResultVersion, runID, now, until, automatic)
	return qualityMonitorUpdated(result, err)
}

func (r *accountQualityMonitorRepository) Renew(ctx context.Context, id int64, runID string, until time.Time) (bool, error) {
	result, err := r.db.ExecContext(ctx, `UPDATE account_quality_monitors SET lease_until=$3
 WHERE account_id=$1 AND run_id=$2 AND lease_until>NOW()`, id, runID, until)
	return qualityMonitorUpdated(result, err)
}

// Lock only while comparing the actual credential identity and saving the two
// snapshots. An account replacement cannot race this commit.
func (r *accountQualityMonitorRepository) Finish(ctx context.Context, record *service.AccountQualityMonitorRecord, runID string, attempt *service.AccountQualityMonitorResult, now time.Time) (bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	_, scope, err := lockQualityMonitorScope(ctx, tx, record.AccountID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if scope != record.Scope {
		return false, nil
	}
	latest, err := json.Marshal(attempt)
	if err != nil {
		return false, err
	}
	valid := *attempt
	valid.Text = ""
	summary, err := json.Marshal(valid)
	if err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE account_quality_monitors SET latest_attempt=$5::jsonb,
 last_valid=CASE WHEN $5::jsonb->>'state' IN ('passed','failed') THEN $6::jsonb ELSE last_valid END,
 next_due_at=CASE WHEN enabled THEN $7::timestamptz+make_interval(mins=>interval_minutes) ELSE NULL END
 WHERE account_id=$1 AND identity_scope=$2 AND result_version=$3 AND run_id=$4 AND lease_until>NOW()`,
		record.AccountID, record.Scope, record.ResultVersion, runID, string(latest), string(summary), now)
	updated, err := qualityMonitorUpdated(result, err)
	if err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return updated, nil
}

func lockQualityMonitorScope(ctx context.Context, tx *sql.Tx, id int64) (*service.Account, string, error) {
	account, err := lockQualityMonitorAccount(ctx, tx, id)
	if err != nil {
		return nil, "", err
	}
	credentialAccount := account
	if account.IsCredentialShadow() {
		credentialAccount, err = lockQualityMonitorAccount(ctx, tx, *account.ParentAccountID)
		if err != nil {
			return nil, "", err
		}
	}
	scope := service.AccountActivityScopeWithCredentialAccount(account, credentialAccount)
	if scope == "" {
		return nil, "", service.ErrAccountQualityUnavailable
	}
	return account, scope, nil
}

func lockQualityMonitorAccount(ctx context.Context, tx *sql.Tx, id int64) (*service.Account, error) {
	account := &service.Account{ID: id}
	var credentials, extra []byte
	err := tx.QueryRowContext(ctx, `SELECT platform,type,execution_mode,parent_account_id,credentials,extra FROM accounts WHERE id=$1 AND deleted_at IS NULL FOR SHARE`, id).
		Scan(&account.Platform, &account.Type, &account.ExecutionMode, &account.ParentAccountID, &credentials, &extra)
	if err != nil {
		return nil, err
	}
	if len(credentials) > 0 {
		if err = json.Unmarshal(credentials, &account.Credentials); err != nil {
			return nil, err
		}
	}
	if len(extra) > 0 {
		if err = json.Unmarshal(extra, &account.Extra); err != nil {
			return nil, err
		}
	}
	return account, nil
}

func (r *accountQualityMonitorRepository) Release(ctx context.Context, id int64, runID string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE account_quality_monitors SET run_id=NULL,lease_until=NULL WHERE account_id=$1 AND run_id=$2`, id, runID)
	return err
}

func (r *accountQualityMonitorRepository) Postpone(ctx context.Context, record *service.AccountQualityMonitorRecord, next time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE account_quality_monitors SET next_due_at=$4
 WHERE account_id=$1 AND identity_scope=$2 AND result_version=$3 AND enabled AND (lease_until IS NULL OR lease_until<=NOW())`, record.AccountID, record.Scope, record.ResultVersion, next)
	return err
}

func qualityMonitorUpdated(result sql.Result, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}
