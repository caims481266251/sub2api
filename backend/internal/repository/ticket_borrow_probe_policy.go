package repository

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (r *ticketBorrowRepository) SaveProbePolicy(ctx context.Context, id int64, p service.BorrowProbePolicy) error {
	if p.Revision < 1 || p.Concurrency < 1 || p.Concurrency > 32 || p.RenewBeforeSeconds < 0 || p.RenewBeforeSeconds > 240 {
		return service.ErrBorrowProbePolicyInvalid
	}
	res, err := r.db.ExecContext(ctx, `UPDATE ticket_borrow_targets SET
 probe_concurrency=$2,renew_before_seconds=$3,probe_policy_revision=probe_policy_revision+1,
 next_attempt_at=clock_timestamp(),updated_at=clock_timestamp()
 WHERE account_id=$1 AND probe_policy_revision=$4
 AND EXISTS(SELECT 1 FROM accounts a WHERE a.id=$1 AND a.deleted_at IS NULL)`, id, p.Concurrency, p.RenewBeforeSeconds, p.Revision)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return service.ErrBorrowConflict
	}
	return nil
}
