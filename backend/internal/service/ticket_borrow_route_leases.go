package service

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
)

// A reservation spans connection waiting and the entire response body, not
// just response headers. Database leases survive across application instances.
type borrowRouteUse struct {
	repo       BorrowRouteLeaseRepository
	id         string
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
	once       sync.Once
	bodyMu     sync.Mutex
	body       io.Closer
	onReleased func()
}

func newBorrowRouteUse(ctx context.Context, repo BorrowRouteLeaseRepository, id string, renewEvery time.Duration, released ...func()) *borrowRouteUse {
	ctx = requesttiming.WithBorrowLease(ctx, borrowHash(id))
	ctx, cancel := context.WithCancel(ctx)
	u := &borrowRouteUse{repo: repo, id: id, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	if len(released) > 0 {
		u.onReleased = released[0]
	}
	requesttiming.BorrowLeaseEvent(ctx, "reserved", 0)
	go func() {
		ticker := time.NewTicker(renewEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				u.finishReason("context_done")
				return
			case <-u.done:
				return
			case <-ticker.C:
				check, stop := context.WithTimeout(ctx, 3*time.Second)
				err := repo.RenewRoute(check, id)
				stop()
				if err != nil {
					if ctx.Err() != nil {
						u.finishReason("context_done")
						return
					}
					// Never keep sending after losing the occupancy reservation.
					u.finishReason("renew_failed")
					return
				}
			}
		}
	}()
	return u
}

func (u *borrowRouteUse) attach(body io.Closer) {
	u.bodyMu.Lock()
	select {
	case <-u.done:
		u.bodyMu.Unlock()
		_ = body.Close()
	default:
		u.body = body
		u.bodyMu.Unlock()
	}
}

func (u *borrowRouteUse) finish() {
	u.finishReason("closed")
}

func (u *borrowRouteUse) finishReason(reason string) {
	u.once.Do(func() {
		requesttiming.BorrowLeaseEvent(u.ctx, reason, 0)
		if reason == "renew_failed" {
			slog.WarnContext(u.ctx, "borrow_route_lease_renew_failed", "lease_hash", borrowHash(u.id), "trace_id", requesttiming.TraceID(u.ctx))
		}
		close(u.done)
		u.cancel()
		u.bodyMu.Lock()
		body := u.body
		u.body = nil
		u.bodyMu.Unlock()
		if body != nil {
			_ = body.Close()
		}
		cleanup, stop := context.WithTimeout(context.WithoutCancel(u.ctx), 3*time.Second)
		defer stop()
		// DELETE is idempotent: one short retry repairs a transient failure (or
		// lost acknowledgement) without renewing a finished request's occupancy.
		// Keep the original total cleanup budget; persistent failure uses the TTL.
		for attempt := 1; attempt <= 2 && cleanup.Err() == nil; attempt++ {
			try, cancel := context.WithTimeout(cleanup, 1500*time.Millisecond)
			err := u.repo.ReleaseRoute(try, u.id)
			cancel()
			if err == nil {
				if u.onReleased != nil {
					u.onReleased()
				}
				requesttiming.BorrowLeaseEvent(u.ctx, "released", attempt)
				slog.InfoContext(cleanup, "borrow_route_lease_released", "lease_hash", borrowHash(u.id), "trace_id", requesttiming.TraceID(u.ctx), "reason", reason, "attempts", attempt)
				return
			}
			requesttiming.BorrowLeaseEvent(u.ctx, "release_failed", attempt)
		}
		slog.WarnContext(cleanup, "borrow_route_lease_release_failed", "lease_hash", borrowHash(u.id), "trace_id", requesttiming.TraceID(u.ctx), "reason", reason)
	})
}

type borrowLeasedBody struct {
	io.ReadCloser
	use *borrowRouteUse
}

func (b *borrowLeasedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		reason := "read_error"
		if err == io.EOF {
			reason = "eof"
		}
		b.use.finishReason(reason)
	}
	return n, err
}

func (b *borrowLeasedBody) Close() error {
	b.use.finish()
	return nil
}
