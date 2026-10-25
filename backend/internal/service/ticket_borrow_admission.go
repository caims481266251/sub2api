package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
)

// ErrBorrowResponseRejected marks a non-error response whose borrowed routing
// identity changed. It is terminal: the business request may already have executed.
var ErrBorrowResponseRejected = errors.New("borrow_response_rejected")

// Keep errors.Is(ErrBorrowUnavailable) and the existing admission/retry policy,
// while retaining the actual veto through all scheduler implementations.
var ErrBorrowContinuationBindingMissing = fmt.Errorf("borrow response binding missing: %w", ErrBorrowUnavailable)
var ErrBorrowNoEligibleRoute = fmt.Errorf("no eligible borrow route: %w", ErrBorrowUnavailable)
var ErrBorrowEndpointUnsupported = fmt.Errorf("borrow endpoint unsupported: %w", ErrBorrowUnavailable)

// Keep the route veto for diagnostics without changing account reselection or
// permitting recovery from a storage error.
type BorrowRouteAdmissionError struct{ Reason string }

func (e *BorrowRouteAdmissionError) Error() string { return "borrow route unavailable: " + e.Reason }
func (e *BorrowRouteAdmissionError) Unwrap() error { return ErrBorrowNoEligibleRoute }

func BorrowAdmissionReason(err error) string {
	var route *BorrowRouteAdmissionError
	switch {
	case errors.Is(err, ErrBorrowEndpointUnsupported):
		return "endpoint_unsupported"
	case errors.Is(err, ErrBorrowContinuationBindingMissing):
		return "response_binding_missing"
	case errors.As(err, &route):
		return route.Reason
	case errors.Is(err, ErrBorrowNoEligibleRoute):
		return "no_eligible_route"
	case IsBorrowRouteUnavailable(err):
		return "borrow_route_unavailable"
	default:
		return "latest_state_unavailable"
	}
}

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

type borrowSelectionFailure struct {
	mu  sync.Mutex
	err error
}

// Preserve a borrow veto when legacy or advanced selection reduces candidates
// to ErrNoAvailableAccounts. This only annotates a failed selection; account
// ordering, successful selections and the existing retry policy are unchanged.
func withBorrowSelectionFailure(ctx context.Context) (context.Context, func(error) error) {
	failure := &borrowSelectionFailure{}
	return context.WithValue(ctx, borrowSelectionFailureKey{}, failure), func(err error) error {
		failure.mu.Lock()
		defer failure.mu.Unlock()
		if failure.err != nil && errors.Is(err, ErrNoAvailableAccounts) {
			return fmt.Errorf("%w: %w", err, failure.err)
		}
		return err
	}
}

func observeBorrowSelectionFailure(ctx context.Context, err error) {
	if failure, ok := ctx.Value(borrowSelectionFailureKey{}).(*borrowSelectionFailure); ok && err != nil {
		failure.mu.Lock()
		defer failure.mu.Unlock()
		// A control-plane failure must not acquire the migration permission of
		// another candidate's capacity veto.
		if failure.err == nil || !IsBorrowRouteUnavailable(err) {
			failure.err = borrowAdmissionError(err)
		}
	}
}
