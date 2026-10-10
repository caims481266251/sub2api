package service

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
)

func IsBorrowRouteUnavailable(err error) bool {
	var admission *OpenAITurnAdmissionError
	return errors.Is(err, ErrBorrowUnavailable) ||
		(errors.As(err, &admission) && admission.Reason == "borrow_route_unavailable")
}

type borrowSelectionFailureKey struct{}

// Preserve a borrow veto when legacy or advanced selection reduces candidates
// to ErrNoAvailableAccounts. This only annotates a failed selection; account
// ordering, successful selections and the existing retry policy are unchanged.
func withBorrowSelectionFailure(ctx context.Context) (context.Context, func(error) error) {
	unavailable := &atomic.Bool{}
	return context.WithValue(ctx, borrowSelectionFailureKey{}, unavailable), func(err error) error {
		if unavailable.Load() && errors.Is(err, ErrNoAvailableAccounts) {
			return fmt.Errorf("%w: %w", err, ErrBorrowUnavailable)
		}
		return err
	}
}

func observeBorrowSelectionFailure(ctx context.Context, err error) {
	if unavailable, ok := ctx.Value(borrowSelectionFailureKey{}).(*atomic.Bool); ok && IsBorrowRouteUnavailable(err) {
		unavailable.Store(true)
	}
}
