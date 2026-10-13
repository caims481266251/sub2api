package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Ownership is renewable and independent of Cookie and HTTP lifetimes.
const BorrowTargetLeaseTTL = 30 * time.Second
const borrowTargetLeaseRenewInterval = BorrowTargetLeaseTTL / 3

type borrowTargetLeaseKey struct{}
type borrowTargetLeaseState struct {
	mu      sync.Mutex
	paused  bool
	changed chan struct{}
}

func (s *borrowTargetLeaseState) pause(paused bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.paused == paused {
		return
	}
	s.paused = paused
	if s.changed != nil {
		close(s.changed)
	}
	s.changed = make(chan struct{})
}

// Wait only before new work or publication. This does not wrap business bodies
// or cancel an already sent probe on a single transient renewal failure.
func waitBorrowTargetLease(ctx context.Context) error {
	s, _ := ctx.Value(borrowTargetLeaseKey{}).(*borrowTargetLeaseState)
	if s == nil {
		return ctx.Err()
	}
	for {
		s.mu.Lock()
		paused, changed := s.paused, s.changed
		s.mu.Unlock()
		if !paused {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

type borrowConfirmedLeaseRepository interface {
	RenewTargetLeaseUntil(context.Context, BorrowAcquisition) (time.Time, error)
}

func borrowLeaseDefinitivelyLost(err error) bool {
	return errors.Is(err, ErrBorrowUnavailable) || errors.Is(err, ErrBorrowConflict) || errors.Is(err, ErrBorrowPaused)
}

// Keep ownership through verification and draining. An unacknowledged renewal
// never extends the last confirmed deadline. Stop and join before FinishJob.
func (s *TicketBorrowService) keepTargetLease(ctx context.Context, a BorrowAcquisition, cancel context.CancelCauseFunc) (func(), error) {
	renew := func(parent context.Context, confirmed time.Time) (time.Time, error) {
		deadline := time.Now().Add(3 * time.Second)
		if !confirmed.IsZero() && confirmed.Before(deadline) {
			deadline = confirmed
		}
		check, stop := context.WithDeadline(parent, deadline)
		defer stop()
		if r, ok := s.repo.(borrowConfirmedLeaseRepository); ok {
			return r.RenewTargetLeaseUntil(check, a)
		}
		// In-memory repositories use the same fixed ownership duration.
		started := time.Now()
		err := s.repo.RenewTargetLease(check, a)
		return started.Add(BorrowTargetLeaseTTL), err
	}
	until, err := renew(ctx, time.Time{})
	if err != nil {
		return nil, err
	}
	keep, stop := context.WithCancel(context.WithoutCancel(ctx))
	state, _ := ctx.Value(borrowTargetLeaseKey{}).(*borrowTargetLeaseState)
	done := make(chan struct{})
	go func() {
		defer close(done)
		delay := borrowTargetLeaseRenewInterval
		for {
			remaining := time.Until(until)
			if remaining <= 0 {
				cancel(borrowCancelTargetLeaseLost)
				return
			}
			timer := time.NewTimer(min(delay, remaining))
			select {
			case <-keep.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			if !time.Now().Before(until) {
				cancel(borrowCancelTargetLeaseLost)
				return
			}
			next, err := renew(keep, until)
			if keep.Err() != nil {
				return
			}
			if err == nil {
				if !time.Now().Before(next) {
					cancel(borrowCancelTargetLeaseLost)
					return
				}
				until = next
				state.pause(false)
				delay = borrowTargetLeaseRenewInterval
				continue
			}
			state.pause(true)
			slog.Warn("borrow_target_lease_renewal_failed", "target_id", a.TargetID,
				"reason", borrowAcquisitionReason(err), "error_type", fmt.Sprintf("%T", err),
				"storage_error", borrowStorageErrorClass(err), "confirmed_until", until, "definitive", borrowLeaseDefinitivelyLost(err))
			if borrowLeaseDefinitivelyLost(err) || !time.Now().Before(until) {
				cancel(borrowCancelTargetLeaseLost)
				return
			}
			delay = time.Second
		}
	}()
	return func() { stop(); <-done }, nil
}
