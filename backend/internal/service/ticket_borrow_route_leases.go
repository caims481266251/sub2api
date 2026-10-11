package service

import (
	"context"
	"io"
	"sync"
	"time"
)

type borrowClientLifetimeKey struct{}

// A reservation spans connection waiting and the entire response body, not
// just response headers. Database leases survive across application instances.
type borrowRouteUse struct {
	repo   BorrowRouteLeaseRepository
	id     string
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
	bodyMu sync.Mutex
	body   io.Closer
}

func newBorrowRouteUse(ctx context.Context, repo BorrowRouteLeaseRepository, id string, renewEvery time.Duration) *borrowRouteUse {
	ctx, cancel := context.WithCancel(ctx)
	u := &borrowRouteUse{repo: repo, id: id, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	client := ctx
	if original, ok := ctx.Value(borrowClientLifetimeKey{}).(context.Context); ok {
		client = original
	}
	go func() {
		ticker := time.NewTicker(renewEvery)
		defer ticker.Stop()
		for {
			select {
			case <-client.Done():
				u.finish()
				return
			case <-ctx.Done():
				u.finish()
				return
			case <-u.done:
				return
			case <-ticker.C:
				check, stop := context.WithTimeout(ctx, 3*time.Second)
				err := repo.RenewRoute(check, id)
				stop()
				if err != nil {
					// Never keep sending after losing the occupancy reservation.
					u.finish()
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
	u.once.Do(func() {
		close(u.done)
		u.cancel()
		u.bodyMu.Lock()
		body := u.body
		u.body = nil
		u.bodyMu.Unlock()
		if body != nil {
			_ = body.Close()
		}
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		// An unavailable database leaves a bounded lease, never a permanent count.
		_ = u.repo.ReleaseRoute(cleanup, u.id)
	})
}

type borrowLeasedBody struct {
	io.ReadCloser
	use *borrowRouteUse
}

func (b *borrowLeasedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.use.finish()
	}
	return n, err
}

func (b *borrowLeasedBody) Close() error {
	b.use.finish()
	return nil
}
