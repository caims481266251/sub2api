package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

const borrowSelectedAccount = `(NOT c.excluded_account_ids @> jsonb_build_array(a.id) AND (a.id IN (SELECT jsonb_array_elements_text(c.account_ids)::bigint)
 OR EXISTS(SELECT 1 FROM account_groups ag JOIN groups g ON g.id=ag.group_id AND g.deleted_at IS NULL
 WHERE ag.account_id=a.id AND ag.group_id IN (SELECT jsonb_array_elements_text(c.group_ids)::bigint))))`
const borrowNormalScope = `a.deleted_at IS NULL AND a.platform='openai' AND a.type IN ('oauth','setup-token') AND a.parent_account_id IS NULL AND ` + borrowSelectedAccount

func (r *ticketBorrowRepository) Selection(ctx context.Context) (service.BorrowSelection, error) {
	var s service.BorrowSelection
	var groups, accounts, models, excluded []byte
	err := r.db.QueryRowContext(ctx, `SELECT revision,group_ids,account_ids,default_model,rotate_identity,model_overrides,use_proxy_pool,excluded_account_ids FROM ticket_borrow_selection WHERE id=1`).Scan(&s.Revision, &groups, &accounts, &s.DefaultModel, &s.RotateIdentity, &models, &s.UseProxyPool, &excluded)
	if err != nil {
		return s, err
	}
	for _, v := range []struct {
		raw  []byte
		dest any
	}{{groups, &s.GroupIDs}, {accounts, &s.AccountIDs}, {models, &s.ModelOverrides}, {excluded, &s.ExcludedAccountIDs}} {
		if err = json.Unmarshal(v.raw, v.dest); err != nil {
			return s, err
		}
	}
	return s, nil
}
func (r *ticketBorrowRepository) SaveSelection(ctx context.Context, s service.BorrowSelection) error {
	s.UseProxyPool, s.RotateIdentity = true, false
	if s.GroupIDs == nil {
		s.GroupIDs = []int64{}
	}
	if s.AccountIDs == nil {
		s.AccountIDs = []int64{}
	}
	groups, _ := json.Marshal(s.GroupIDs)
	accounts, _ := json.Marshal(s.AccountIDs)
	models, _ := json.Marshal(s.ModelOverrides)
	var excluded any
	if s.ExcludedAccountIDs != nil {
		raw, _ := json.Marshal(s.ExcludedAccountIDs)
		excluded = string(raw)
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Use the same pool -> selection lock order as periodic reconciliation.
	if _, err = tx.ExecContext(ctx, `SELECT id FROM proxy_pool_state WHERE id=1 FOR UPDATE`); err != nil {
		return err
	}
	// References may remain selected while disabled; removed accounts/groups may be
	// retained in a saved selection so an unrelated edit does not erase intent.
	res, err := tx.ExecContext(ctx, `UPDATE ticket_borrow_selection SET group_ids=$1,
 account_ids=(SELECT COALESCE(jsonb_agg(value),'[]'::jsonb) FROM jsonb_array_elements($2::jsonb) WHERE NOT COALESCE($8::jsonb,excluded_account_ids) @> jsonb_build_array(value)),
 excluded_account_ids=COALESCE($8::jsonb,excluded_account_ids),default_model=$3,rotate_identity=$4,model_overrides=$5,use_proxy_pool=$7,revision=revision+1 WHERE id=1 AND revision=$6`, string(groups), string(accounts), s.DefaultModel, s.RotateIdentity, string(models), s.Revision, s.UseProxyPool, excluded)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return service.ErrBorrowConflict
	}
	if err = reconcileBorrowProxyBindings(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}
func (r *ticketBorrowRepository) SourceOptions(ctx context.Context) (service.BorrowSourceOptions, error) {
	out := service.BorrowSourceOptions{Accounts: []service.BorrowSourceOption{}, Groups: []service.BorrowGroupOption{}}
	rows, err := r.db.QueryContext(ctx, `SELECT a.id,a.name,a.status,a.schedulable,false,EXISTS(SELECT 1 FROM ticket_borrow_targets t WHERE t.account_id=a.id AND t.enabled),COALESCE((SELECT jsonb_agg(ag.group_id ORDER BY ag.group_id) FROM account_groups ag JOIN groups g ON g.id=ag.group_id AND g.deleted_at IS NULL WHERE ag.account_id=a.id),'[]') FROM accounts a WHERE a.deleted_at IS NULL AND a.platform='openai' AND a.type IN ('oauth','setup-token') AND a.parent_account_id IS NULL ORDER BY a.id`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var a service.BorrowSourceOption
		var groups []byte
		if err = rows.Scan(&a.ID, &a.Name, &a.Status, &a.Schedulable, &a.AuthExcluded, &a.IsTarget, &groups); err != nil {
			rows.Close()
			return out, err
		}
		if err = json.Unmarshal(groups, &a.GroupIDs); err != nil {
			rows.Close()
			return out, err
		}
		out.Accounts = append(out.Accounts, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	rows, err = r.db.QueryContext(ctx, `SELECT id,name FROM groups WHERE deleted_at IS NULL AND platform='openai' ORDER BY name,id`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var g service.BorrowGroupOption
		if err = rows.Scan(&g.ID, &g.Name); err != nil {
			rows.Close()
			return out, err
		}
		out.Groups = append(out.Groups, g)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	err = r.db.QueryRowContext(ctx, `SELECT count(*),count(*) FILTER(WHERE EXISTS(SELECT 1 FROM ticket_borrow_transfers t WHERE t.source_id=s.id AND t.returned_at IS NULL)) FROM ticket_borrow_sources s WHERE account_id IS NULL AND deleted_at IS NULL`).Scan(&out.LegacyCount, &out.RestorableCount)
	return out, err
}
func (r *ticketBorrowRepository) normalSources(ctx context.Context, sourceID int64) ([]service.BorrowSource, error) {
	if sourceID == 0 {
		_, err := r.db.ExecContext(ctx, `INSERT INTO ticket_borrow_sources(name,secret,subject_hash,subject_id,account_id)
 SELECT a.name,'','normal-account:'||a.id,'',a.id FROM accounts a CROSS JOIN ticket_borrow_selection c WHERE c.id=1 AND `+borrowNormalScope+`
 ON CONFLICT(account_id) WHERE account_id IS NOT NULL DO NOTHING`)
		if err != nil {
			return nil, err
		}
	}
	rows, err := r.db.QueryContext(ctx, `SELECT s.id,a.id,a.name,COALESCE(c.model_overrides->>a.id::text,c.default_model),s.revision,s.cursor,
 s.last_reason,s.next_attempt_at,a.id IN (SELECT jsonb_array_elements_text(c.account_ids)::bigint),
 COALESCE((SELECT jsonb_agg(ag.group_id ORDER BY ag.group_id) FROM account_groups ag JOIN groups g ON g.id=ag.group_id AND g.deleted_at IS NULL WHERE ag.account_id=a.id AND ag.group_id IN (SELECT jsonb_array_elements_text(c.group_ids)::bigint)),'[]'),
 CASE WHEN EXISTS(SELECT 1 FROM ticket_borrow_targets t WHERE t.account_id=a.id AND t.enabled) THEN 'source_is_target'
 WHEN a.status<>'active' OR NOT a.schedulable THEN 'source_account_unavailable'
 WHEN a.auto_pause_on_expired AND a.expires_at<=clock_timestamp() THEN 'source_account_expired'
 WHEN a.rate_limit_reset_at>clock_timestamp() THEN 'source_rate_limited'
 WHEN a.overload_until>clock_timestamp() OR a.temp_unschedulable_until>clock_timestamp() THEN 'source_account_cooling'
 WHEN NOT EXISTS(SELECT 1 FROM proxy_pool_entries WHERE enabled AND retired_at IS NULL) THEN 'source_proxy_pool_unavailable'
 ELSE '' END,a.rate_limit_reset_at,s.lease_until
 FROM ticket_borrow_sources s JOIN accounts a ON a.id=s.account_id CROSS JOIN ticket_borrow_selection c
 WHERE c.id=1 AND s.deleted_at IS NULL AND `+borrowNormalScope+` AND ($1=0 OR s.id=$1) ORDER BY s.id`, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []service.BorrowSource{}
	for rows.Next() {
		var source service.BorrowSource
		var groups []byte
		if err = rows.Scan(&source.ID, &source.AccountID, &source.Name, &source.Model, &source.Revision, &source.Cursor, &source.LastReason, &source.NextAttemptAt, &source.Explicit, &groups, &source.UnavailableReason, &source.RateLimitResetAt, &source.LeaseUntil); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(groups, &source.GroupIDs); err != nil {
			return nil, err
		}
		source.Enabled = source.UnavailableReason == ""
		source.UseProxyPool = true
		source.RotationUnavailableReason = "borrow_rotation_per_candidate"
		out = append(out, source)
	}
	return out, rows.Err()
}

// Explicit, all-or-nothing ownership return. Existing route IDs, events, groups,
// account IDs and source IDs are retained. No old refresh worker can write back.
func (r *ticketBorrowRepository) RestoreSources(ctx context.Context) ([]int64, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(269001)`); err != nil {
		return nil, err
	}
	var running, busy bool
	if err = tx.QueryRowContext(ctx, `SELECT running FROM ticket_borrow_settings WHERE id=1 FOR UPDATE`).Scan(&running); err != nil {
		return nil, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ticket_borrow_sources WHERE lease_until>NOW()) OR EXISTS(SELECT 1 FROM ticket_borrow_targets WHERE lease_until>NOW())`).Scan(&busy); err != nil {
		return nil, err
	}
	if running || busy {
		return nil, service.ErrBorrowConflict
	}
	var selectedRaw, overridesRaw []byte
	if err = tx.QueryRowContext(ctx, `SELECT account_ids,model_overrides FROM ticket_borrow_selection WHERE id=1 FOR UPDATE`).Scan(&selectedRaw, &overridesRaw); err != nil {
		return nil, err
	}
	var selected []int64
	var overrides map[int64]string
	if err = json.Unmarshal(selectedRaw, &selected); err != nil {
		return nil, err
	}
	if err = json.Unmarshal(overridesRaw, &overrides); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT s.id,t.account_id,s.secret,s.model,s.enabled FROM ticket_borrow_sources s JOIN ticket_borrow_transfers t ON t.source_id=s.id JOIN accounts a ON a.id=t.account_id WHERE t.returned_at IS NULL AND s.account_id IS NULL AND s.deleted_at IS NULL AND a.deleted_at IS NOT NULL AND a.credentials='{}'::jsonb ORDER BY s.id FOR UPDATE OF s,t,a`)
	if err != nil {
		return nil, err
	}
	type item struct {
		id, account   int64
		secret, model string
		enabled       bool
	}
	items := []item{}
	for rows.Next() {
		var v item
		if err = rows.Scan(&v.id, &v.account, &v.secret, &v.model, &v.enabled); err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	restored := []int64{}
	for _, v := range items {
		var secret service.BorrowSourceSecret
		if err = r.open(v.secret, &secret); err != nil {
			return nil, err
		}
		if len(secret.Proxies) > 1 {
			return nil, errors.New("borrow_restore_multiple_proxies")
		}
		var proxyID *int64
		if len(secret.Proxies) == 1 {
			u, e := url.Parse(secret.Proxies[0])
			if e != nil || u.Hostname() == "" {
				return nil, errors.New("borrow_proxy_invalid")
			}
			port, e := strconv.Atoi(u.Port())
			if e != nil {
				return nil, errors.New("borrow_proxy_invalid")
			}
			username, password := "", ""
			if u.User != nil {
				username = u.User.Username()
				password, _ = u.User.Password()
			}
			var id int64
			err = tx.QueryRowContext(ctx, `INSERT INTO proxies(name,protocol,host,port,username,password,status) VALUES($1,$2,$3,$4,$5,$6,'active') RETURNING id`, fmt.Sprintf("借票迁回 #%d", v.account), u.Scheme, u.Hostname(), port, username, password).Scan(&id)
			if err != nil {
				return nil, err
			}
			proxyID = &id
		}
		credentials, e := json.Marshal(secret.Credentials)
		if e != nil {
			return nil, e
		}
		if _, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_sources SET account_id=$2,secret='',enabled=true,revision=revision+1,updated_at=NOW() WHERE id=$1`, v.id, v.account); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_transfers SET returned_at=NOW() WHERE source_id=$1`, v.id); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE accounts SET deleted_at=NULL,credentials=$2::jsonb,proxy_id=COALESCE($3,proxy_id),schedulable=true,updated_at=NOW() WHERE id=$1`, v.account, string(credentials), proxyID); err != nil {
			return nil, err
		}
		if err = enqueueSchedulerOutbox(ctx, tx, service.SchedulerOutboxEventAccountChanged, &v.account, nil, nil); err != nil {
			return nil, err
		}
		found := false
		for _, id := range selected {
			if id == v.account {
				found = true
				break
			}
		}
		if !found && v.enabled {
			selected = append(selected, v.account)
		}
		overrides[v.account] = v.model
		restored = append(restored, v.account)
	}
	if len(restored) > 0 {
		selectedRaw, _ = json.Marshal(selected)
		overridesRaw, _ = json.Marshal(overrides)
		if _, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_selection SET account_ids=$1,model_overrides=$2,revision=revision+1 WHERE id=1`, string(selectedRaw), string(overridesRaw)); err != nil {
			return nil, err
		}
	}
	return restored, tx.Commit()
}
