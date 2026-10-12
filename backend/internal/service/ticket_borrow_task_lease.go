package service

import (
	"context"
	"log/slog"
	"time"
)

// Task ownership is renewable, independent of candidate validity and HTTP
// deadlines. These are internal crash-recovery bounds, not a batch time limit.
const BorrowTargetLeaseTTL = 30 * time.Second

const borrowTargetLeaseRenewInterval = BorrowTargetLeaseTTL / 3

// Keep ownership until the caller has drained its work. Stopping a task's
// context must not release its lease while admitted verifications clean up.
// The returned function cancels and joins renewal before FinishJob releases it.
func (s *TicketBorrowService) keepTargetLease(ctx context.Context, a BorrowAcquisition, cancel context.CancelCauseFunc) (func(), error) {
	renew := func(ctx context.Context) error {
		check, stop := context.WithTimeout(ctx, 3*time.Second)
		defer stop()
		return s.repo.RenewTargetLease(check, a)
	}
	if err := renew(ctx); err != nil {
		return nil, err
	}
	keep, stop := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(borrowTargetLeaseRenewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-keep.Done():
				return
			case <-ticker.C:
				if err := renew(keep); err != nil {
					if keep.Err() == nil {
						cancel(borrowCancelTargetLeaseLost)
						slog.Warn("borrow_target_lease_renewal_failed", "target_id", a.TargetID, "reason", borrowAcquisitionReason(err))
					}
					return
				}
			}
		}
	}()
	return func() { stop(); <-done }, nil
}
