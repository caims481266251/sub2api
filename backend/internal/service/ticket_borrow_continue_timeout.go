package service

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const borrowContinueTimeoutReason = "target_continue_timeout"

// A policy deadline is terminal for this candidate, unlike a transient network error.
type borrowContinueTimeoutError struct{ Seconds int }

func (e *borrowContinueTimeoutError) Error() string {
	return fmt.Sprintf("target continuation exceeded %d seconds", e.Seconds)
}

func borrowContinueDeadline(ctx context.Context, seconds int) (context.Context, context.CancelFunc) {
	return context.WithTimeoutCause(ctx, time.Duration(seconds)*time.Second, &borrowContinueTimeoutError{Seconds: seconds})
}

func borrowContinueTimedOut(err error) bool {
	var timeout *borrowContinueTimeoutError
	return errors.As(err, &timeout)
}
