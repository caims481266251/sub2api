package service

import (
	"context"
	"errors"
	"maps"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
)

type borrowRouteWaitDisabledKey struct{}

// WithoutBorrowRouteWait still tries every currently available account/route.
// It prevents an upstream model rejection from waiting for future inventory.
func WithoutBorrowRouteWait(ctx context.Context) context.Context {
	return context.WithValue(ctx, borrowRouteWaitDisabledKey{}, true)
}

// Subscribe before selecting to avoid losing a publication between the check
// and the wait. One broadcast channel per service; no per-request goroutines.
func (s *TicketBorrowService) routePublished() <-chan struct{} {
	s.routeNotifyMu.Lock()
	defer s.routeNotifyMu.Unlock()
	if s.routeNotify == nil {
		s.routeNotify = make(chan struct{})
	}
	return s.routeNotify
}
func (s *TicketBorrowService) notifyRoutePublished() {
	s.routeNotifyMu.Lock()
	defer s.routeNotifyMu.Unlock()
	if s.routeNotify != nil {
		close(s.routeNotify)
	}
	s.routeNotify = make(chan struct{})
}

// Only the handler's proven initial rejection may register an account for a
// later admission retry. Ordinary alternatives still get the first selection;
// these temporary exclusions can be lifted only while no transport has begun.
func (r *BorrowBusinessRecovery) RememberAdmissionRetry(id int64, err error) bool {
	if r == nil || !errors.Is(err, ErrBorrowNoEligibleRoute) {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.err != nil || r.transports != 0 || r.sends != 0 || r.committed || r.writePossible {
		return false
	}
	if r.routeWaitDeadline.IsZero() {
		r.routeWaitDeadline = time.Now().Add(r.timeout)
	}
	if !time.Now().Before(r.routeWaitDeadline) {
		return false
	}
	if r.admissionRetries == nil {
		r.admissionRetries = make(map[int64]struct{})
	}
	r.admissionRetries[id] = struct{}{}
	return true
}

func (r *BorrowBusinessRecovery) HasAdmissionRetry(id int64) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.admissionRetries[id]
	return ok
}

func (r *BorrowBusinessRecovery) admissionRetryExclusions(excluded map[int64]struct{}) (map[int64]struct{}, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.err != nil || r.transports != 0 || r.sends != 0 || r.committed || r.writePossible {
		return nil, false
	}
	var retry map[int64]struct{}
	for id := range r.admissionRetries {
		if _, found := excluded[id]; found {
			if retry == nil {
				retry = maps.Clone(excluded)
			}
			delete(retry, id)
		}
	}
	return retry, retry != nil
}

// Use the existing recovery timeout for one bounded admission wait shared
// across reselections. Client/recovery cancellation can end it earlier. This
// does not start or extend the separate upstream-failure/replay budget.
func (r *BorrowBusinessRecovery) waitForRoute(ctx context.Context, published <-chan struct{}) bool {
	if disabled, _ := ctx.Value(borrowRouteWaitDisabledKey{}).(bool); r == nil || disabled {
		return false
	}
	r.mu.Lock()
	if r.routeWaitDeadline.IsZero() {
		r.routeWaitDeadline = time.Now().Add(r.timeout)
	}
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(r.routeWaitDeadline) {
		r.routeWaitDeadline = deadline
	}
	var clientDone <-chan struct{}
	clientCancelled := false
	if r.client != nil {
		clientDone = r.client.Done()
		clientCancelled = r.client.Err() != nil
		if deadline, ok := r.client.Deadline(); ok && deadline.Before(r.routeWaitDeadline) {
			r.routeWaitDeadline = deadline
		}
	}
	remaining := time.Until(r.routeWaitDeadline)
	stopped := r.closed || r.err != nil
	r.mu.Unlock()
	if stopped || remaining <= 0 || ctx.Err() != nil || clientCancelled {
		return false
	}
	observe := requesttiming.Observe(ctx, "borrow_route_wait")
	defer observe()
	// Polling covers other application instances, while local publish wakes at once.
	delay := min(remaining, time.Second)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-clientDone:
		return false
	case <-ctx.Done():
		return false
	case <-published:
		return true
	case <-timer.C:
		return remaining > delay
	}
}
