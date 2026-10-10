package repository

import (
	"context"
	"database/sql"
)

// Reconcile only the borrow-specific inventory. Serialize with pool mutations
// and selection saves; neither account proxies nor published routes are edited.
func (r *ticketBorrowRepository) reconcileProxyBindings(ctx context.Context) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = reconcileBorrowProxyBindings(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func reconcileBorrowProxyBindings(ctx context.Context, tx *sql.Tx) error {
	var err error
	if _, err = tx.ExecContext(ctx, `SELECT id FROM proxy_pool_state WHERE id=1 FOR UPDATE`); err != nil {
		return err
	}
	var enabled bool
	if err = tx.QueryRowContext(ctx, `SELECT use_proxy_pool FROM ticket_borrow_selection WHERE id=1 FOR SHARE`).Scan(&enabled); err != nil {
		return err
	}
	if !enabled {
		return nil
	}
	// A legacy target environment can become a source binding once that account
	// is no longer an enabled target. The proxy and device themselves stay fixed.
	if _, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_proxy_bindings b SET role='source' WHERE b.role='target'
 AND NOT EXISTS(SELECT 1 FROM ticket_borrow_targets t WHERE t.account_id=b.account_id AND t.enabled)
 AND EXISTS(SELECT 1 FROM accounts a CROSS JOIN ticket_borrow_selection c WHERE c.id=1 AND a.id=b.account_id AND `+borrowNormalScope+`)`); err != nil {
		return err
	}

	if _, err = tx.ExecContext(ctx, `DELETE FROM ticket_borrow_proxy_bindings b WHERE b.role='source' AND
 NOT EXISTS(SELECT 1 FROM ticket_borrow_rotation_accounts x WHERE x.account_id=b.account_id AND x.active_job IS NOT NULL) AND
 NOT EXISTS(SELECT 1 FROM ticket_borrow_sources s WHERE s.account_id=b.account_id AND s.lease_until>clock_timestamp()) AND (
 NOT EXISTS(SELECT 1 FROM proxy_pool_entries p WHERE p.id=b.entry_id AND p.enabled AND p.retired_at IS NULL)
 OR NOT EXISTS(SELECT 1 FROM accounts a CROSS JOIN ticket_borrow_selection c WHERE c.id=1 AND a.id=b.account_id AND `+borrowNormalScope+`
 AND NOT EXISTS(SELECT 1 FROM ticket_borrow_targets t WHERE t.account_id=a.id AND t.enabled)))`); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT a.id FROM accounts a CROSS JOIN ticket_borrow_selection c WHERE c.id=1 AND `+borrowNormalScope+`
 AND a.status='active' AND a.schedulable
 AND NOT EXISTS(SELECT 1 FROM ticket_borrow_targets t WHERE t.account_id=a.id AND t.enabled)
 AND NOT EXISTS(SELECT 1 FROM ticket_borrow_proxy_bindings b WHERE b.account_id=a.id) ORDER BY a.id`)
	if err != nil {
		return err
	}
	var accounts []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		accounts = append(accounts, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(accounts) == 0 {
		return nil
	}
	rows, err = tx.QueryContext(ctx, `SELECT p.id FROM proxy_pool_entries p WHERE p.enabled AND p.retired_at IS NULL
 AND NOT EXISTS(SELECT 1 FROM ticket_borrow_proxy_bindings b WHERE b.entry_id=p.id)
 AND NOT EXISTS(SELECT 1 FROM ticket_borrow_rotation_assignments x WHERE x.entry_id=p.id) ORDER BY p.id`)
	if err != nil {
		return err
	}
	entries := []int64{}
	for rows.Next() {
		var entry int64
		if err = rows.Scan(&entry); err != nil {
			rows.Close()
			return err
		}
		entries = append(entries, entry)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	accounts = accounts[:min(len(accounts), len(entries))]
	if len(accounts) == 0 {
		return nil
	}
	assignments, err := rotationMatching(ctx, accounts, entries, nil)
	if err != nil {
		return err
	}
	if err = seedBorrowFingerprintHistory(ctx, tx); err != nil {
		return err
	}
	for _, account := range accounts {
		entry := assignments[account]
		device, e := newBorrowFingerprint(ctx, tx, account, entry)
		if e != nil {
			return e
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO ticket_borrow_proxy_bindings(account_id,entry_id,device_id) VALUES($1,$2,$3)`, account, entry, device); err != nil {
			return err
		}
		if err = recordBorrowProxyUse(ctx, tx, account, entry); err != nil {
			return err
		}
	}
	return nil
}
