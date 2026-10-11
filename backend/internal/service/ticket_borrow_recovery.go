package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/google/uuid"
)

var ErrBorrowRecoveryWaiting = errors.New("target_upstream_waiting")
var ErrBorrowRequestBudget = errors.New("borrow_request_budget")
var ErrBorrowRouteReady = errors.New("target_route_ready")
var ErrBorrowPaused = errors.New("borrow_acquisition_paused")
var errBorrowSourceDeferred = errors.New("borrow_source_deferred")

type BorrowBudgetWaitError struct {
	Scope      string
	Until      time.Time
	RetryAfter time.Duration
}

func (e *BorrowBudgetWaitError) Error() string { return "borrow_request_budget" }
func (e *BorrowBudgetWaitError) Unwrap() error { return ErrBorrowRequestBudget }

type borrowProbeRequestKey struct{}
type borrowConnectedKey struct{}

// Called by the shared transport after connection waiting and admission checks.
func BorrowTargetRequestConnected(req *http.Request) error {
	callback, _ := req.Context().Value(borrowConnectedKey{}).(func(*http.Request) error)
	if callback != nil {
		return callback(req)
	}
	return nil
}
func (s *TicketBorrowService) trackTargetRequest(req *http.Request, q BorrowRequest, proxy string, id int64, n int, profile *tlsfingerprint.Profile, send func(*http.Request, string, int64, int, *tlsfingerprint.Profile) (*http.Response, error)) (*http.Response, error) {
	if q.Kind != "probe" {
		return send(req, proxy, id, n, profile)
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileBorrowProbe))
	q.ID = uuid.NewString()
	q.TargetID = id
	if q.Kind == "probe" {
		if a, ok := BorrowAcquisitionFromContext(req.Context()); ok {
			q.Identity = a.Identity
		}
		environment := borrowEnvironment(req, proxy, profile)
		q.Environment = &environment
		q.Phase = "mint"
		if req.Header.Get(openAICodexTurnStateHeader) != "" {
			q.Phase = "continue"
		}
	}
	if q.Phase == "continue" {
		// Read for every new attempt, not once for an entire acquisition batch.
		settings, err := s.repo.Settings(req.Context())
		if err != nil {
			return nil, err
		}
		q.ContinueTimeoutSeconds = 7
		if settings.ContinueTimeoutSeconds != nil {
			q.ContinueTimeoutSeconds = *settings.ContinueTimeoutSeconds
		}
	}
	if err := s.repo.BeginTargetRequest(req.Context(), q); err != nil {
		return nil, err
	}
	var ctx context.Context
	var cancel context.CancelFunc
	if q.Phase == "continue" {
		ctx, cancel = borrowContinueDeadline(req.Context(), q.ContinueTimeoutSeconds)
	} else {
		ctx, cancel = context.WithCancel(req.Context())
	}
	requestError := func(err error) error {
		if cause := context.Cause(ctx); borrowContinueTimedOut(cause) {
			return cause
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	prior, _ := ctx.Value(borrowBeforeSendKey{}).(func(context.Context) error)
	ctx = context.WithValue(ctx, borrowBeforeSendKey{}, func(ctx context.Context) error {
		if prior != nil {
			if err := prior(ctx); err != nil {
				return err
			}
		}
		return s.repo.CheckTargetRequest(ctx, q)
	})
	ctx = context.WithValue(ctx, borrowConnectedKey{}, func(connected *http.Request) error {
		sent := q
		if sent.Kind == "probe" {
			environment := borrowEnvironment(connected, proxy, profile)
			sent.Environment = &environment
		}
		return s.repo.MarkTargetRequestSent(connected.Context(), sent)
	})
	done := make(chan struct{})
	var once sync.Once
	finish := func(status int, outcome string) {
		once.Do(func() {
			if borrowContinueTimedOut(context.Cause(ctx)) {
				outcome = borrowContinueTimeoutReason
			}
			close(done)
			cancel()
			cleanup, stop := context.WithTimeout(context.WithoutCancel(req.Context()), 3*time.Second)
			defer stop()
			if err := s.repo.EndTargetRequest(cleanup, q, status, outcome); err != nil {
				slog.Warn("borrow_probe_request_cleanup_failed", "request_id", q.ID, "target_id", q.TargetID, "phase", q.Phase)
			}
		})
	}
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				// Cancellation is a stop request, not evidence that transport/body
				// cleanup finished. Let send/Read/Close finalize; an abandoned body
				// retains its bounded lease and is conservatively reaped later.
				return
			case <-ticker.C:
				keep, stop := context.WithTimeout(ctx, 3*time.Second)
				err := s.repo.RenewTargetRequest(keep, q)
				stop()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	resp, err := send(req.WithContext(ctx), proxy, id, n, profile)
	if err != nil {
		err = requestError(err)
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		finish(0, "transport_error")
		return resp, err
	}
	if resp == nil {
		finish(0, "empty_response")
		return nil, ErrBorrowUnavailable
	}
	if resp.Body == nil {
		err = requestError(nil)
		finish(resp.StatusCode, "headers_only")
	} else {
		body := &borrowActivityBody{ReadCloser: resp.Body, requestError: requestError, stop: cancel, finish: func(outcome string) { finish(resp.StatusCode, outcome) }}
		if q.Phase == "continue" {
			// Closing also interrupts readers whose transport does not wake Read on
			// cancellation. Accounting still ends only after Read/Close returns.
			context.AfterFunc(ctx, func() { _ = body.closeBody() })
		}
		resp.Body = body
	}
	return resp, err
}

type borrowActivityBody struct {
	io.ReadCloser
	finish       func(string)
	requestError func(error) error
	stop         context.CancelFunc
	closeOnce    sync.Once
	closeErr     error
}

func (b *borrowActivityBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if b.requestError != nil {
		err = b.requestError(err)
	}
	if err != nil {
		outcome := "read_error"
		if err == io.EOF {
			outcome = "eof"
		}
		// Freeze the completed request's cause before potentially slower cleanup.
		// Cancelling cannot replace a policy deadline that has already fired.
		if b.stop != nil {
			b.stop()
		}
		// Do not admit a replacement while the old response still owns resources.
		_ = b.closeBody()
		b.finish(outcome)
	}
	return n, err
}
func (b *borrowActivityBody) Close() error {
	err := b.closeBody()
	b.finish("closed")
	return err
}

func (b *borrowActivityBody) closeBody() error {
	b.closeOnce.Do(func() { b.closeErr = b.ReadCloser.Close() })
	return b.closeErr
}

func borrowAcquisitionReason(err error) string {
	switch {
	case errors.Is(err, ErrBorrowRouteReady):
		return "target_route_ready"
	case errors.Is(err, ErrBorrowRequestBudget):
		var wait *BorrowBudgetWaitError
		if errors.As(err, &wait) && wait.Scope == "source" {
			return "source_request_budget"
		}
		return "target_request_budget"
	case errors.Is(err, ErrBorrowRecoveryWaiting):
		return "target_upstream_waiting"
	case errors.Is(err, ErrBorrowPaused):
		return "borrow_acquisition_paused"
	case errors.Is(err, ErrBorrowUnavailable):
		return "target_unavailable"
	default:
		return "source_store_unavailable"
	}
}
