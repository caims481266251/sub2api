package service

import (
	"context"
	"errors"
	"net"
	"time"
)

// Cancellation must not erase a completed HTTP outcome. Retries are bounded
// and idempotent; the repository's periodic lease reaper handles a longer outage.
func borrowCleanup(parent context.Context, finish func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
	defer cancel()
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		call, stop := context.WithTimeout(ctx, 3*time.Second)
		err = finish(call)
		stop()
		if err == nil || ctx.Err() != nil || borrowLeaseDefinitivelyLost(err) {
			return err
		}
		if attempt < 2 {
			timer := time.NewTimer(200 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return err
			case <-timer.C:
			}
		}
	}
	return err
}

// Log only structured classes, never raw SQL or connection strings.
func borrowStorageErrorClass(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	var sqlState interface{ SQLState() string }
	if errors.As(err, &sqlState) {
		return "sqlstate_" + sqlState.SQLState()
	}
	var network net.Error
	if errors.As(err, &network) {
		if network.Timeout() {
			return "network_timeout"
		}
		return "network_error"
	}
	return "storage_error"
}
