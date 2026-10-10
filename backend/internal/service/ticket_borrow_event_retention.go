package service

import (
	"context"
	"log/slog"
	"time"
)

// BorrowEventRetentionRepository separates frequent event expiry from the
// hourly cleanup of routes, rotation jobs and request diagnostics.
type BorrowEventRetentionRepository interface {
	CleanupEvents(context.Context) error
}

func (s *TicketBorrowService) cleanupEvents(ctx context.Context) {
	r, ok := s.repo.(BorrowEventRetentionRepository)
	if !ok || time.Now().Before(s.nextEventCleanup) {
		return
	}
	cleanup, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := r.CleanupEvents(cleanup); err != nil {
		s.nextEventCleanup = time.Now().Add(5 * time.Second)
		if ctx.Err() == nil {
			slog.Warn("ticket_borrow_event_cleanup_failed")
		}
		return
	}
	s.nextEventCleanup = time.Now().Add(time.Minute)
}
