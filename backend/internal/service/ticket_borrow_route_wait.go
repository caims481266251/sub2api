package service

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"time"
)

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

// A logical request receives at most five seconds of route waiting, shared
// across reselections. Recovery's outer deadline can end it earlier. Selection
// is rerun normally after wakeup; notifications never grant a route or a slot.
func (r *BorrowBusinessRecovery) waitForRoute(ctx context.Context, published <-chan struct{}) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	if r.routeWaitDeadline.IsZero() {
		r.routeWaitDeadline = time.Now().Add(5 * time.Second)
	}
	remaining := time.Until(r.routeWaitDeadline)
	stopped := r.closed || r.err != nil
	r.mu.Unlock()
	if stopped || remaining <= 0 || ctx.Err() != nil {
		return false
	}
	observe := requesttiming.Observe(ctx, "borrow_route_wait")
	defer observe()
	// Polling covers other application instances, while local publish wakes at once.
	delay := min(remaining, time.Second)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-published:
		return true
	case <-timer.C:
		return remaining > delay
	}
}
