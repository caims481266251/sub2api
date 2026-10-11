package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
)

// ErrBorrowResponseRejected marks a non-error response whose borrowed routing
// identity changed. It is terminal: the business request may already have executed.
var ErrBorrowResponseRejected = errors.New("borrow_response_rejected")

type borrowPreSendError struct{ error }

func (e *borrowPreSendError) Unwrap() error { return e.error }

// Preserve storage/control-plane failures instead of labeling them as a known
// missing route. Only the latter can be cured by local account reselection.
func borrowAdmissionError(err error) error {
	if err == nil || IsOpenAITurnAdmissionError(err) {
		return err
	}
	return &OpenAITurnAdmissionError{Reason: "latest_state_unavailable", cause: err}
}

func borrowPreSendRejection(err error) error {
	err = borrowAdmissionError(err)
	if IsBorrowRouteUnavailable(err) {
		return &borrowPreSendError{error: err}
	}
	return err
}

type borrowHTTPTransportStartedKey struct{}

// BorrowBeforeTransport runs before entering the HTTP client. GotConn must
// continue using BorrowBeforeSend: a connection callback can follow an earlier
// write/retry and cannot establish that this request has never been sent.
func BorrowBeforeTransport(req *http.Request) error {
	err := BorrowBeforeSend(req)
	if req == nil {
		return err
	}
	started, _ := req.Context().Value(borrowHTTPTransportStartedKey{}).(*atomic.Bool)
	if started != nil {
		if err != nil && !started.Load() {
			return borrowPreSendRejection(err)
		}
		started.Store(true)
	}
	return err
}

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
