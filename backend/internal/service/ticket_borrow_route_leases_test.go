package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

func activeBorrowLeases(r *borrowTestRepo) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.leases)
}

type borrowReleaseFailureRepo struct {
	*borrowTestRepo
	attempts int
	failures int
	block    bool
}

func (r *borrowReleaseFailureRepo) ReleaseRoute(ctx context.Context, id string) error {
	r.attempts++
	if r.block {
		<-ctx.Done()
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if r.attempts <= r.failures {
		return errors.New("temporary database failure")
	}
	return r.borrowTestRepo.ReleaseRoute(ctx, id)
}

func TestBorrowLeaseReleaseRetriesWithinCleanupAndRecordsOutcome(t *testing.T) {
	for _, failures := range []int{0, 1, 2} {
		t.Run(fmt.Sprint(failures), func(t *testing.T) {
			_, base := borrowFixture()
			base.leases = map[string]string{"lease": "route-a"}
			repo := &borrowReleaseFailureRepo{borrowTestRepo: base, failures: failures}
			collector := requesttiming.New(time.Now(), 0)
			req, _ := requesttiming.StartAttempt(borrowRequest(t).WithContext(requesttiming.With(t.Context(), collector)), 11, 0)
			service := &TicketBorrowService{}
			signal := service.routePublished()
			use := newBorrowRouteUse(req.Context(), repo, "lease", time.Hour, service.notifyRoutePublished)
			use.finishReason("read_error")
			use.finish()
			a := requesttiming.LatestAttempt(req.Context())
			require.Equal(t, min(failures+1, 2), repo.attempts)
			require.Equal(t, repo.attempts, a.BorrowLeaseReleaseAttempts)
			require.Equal(t, "read_error", a.BorrowLeaseEnd)
			require.Equal(t, borrowHash("lease"), a.BorrowLeaseHash)
			if failures < 2 {
				select {
				case <-signal:
				default:
					t.Fatal("released capacity must wake waiting selectors")
				}
				require.Zero(t, activeBorrowLeases(base))
				require.Equal(t, "released", a.BorrowLeaseState)
			} else {
				select {
				case <-signal:
					t.Fatal("failed cleanup must not announce free capacity")
				default:
				}
				require.Equal(t, 1, activeBorrowLeases(base), "persistent DB failure stays visible until TTL")
				require.Equal(t, "release_failed", a.BorrowLeaseState)
			}
		})
	}
}

func TestBorrowLeaseReleaseFailureKeepsOriginalTotalBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, base := borrowFixture()
		base.leases = map[string]string{"lease": "route-a"}
		repo := &borrowReleaseFailureRepo{borrowTestRepo: base, block: true}
		ctx, cancel := context.WithCancel(t.Context())
		// Test the cleanup deadline in isolation: synctest cannot advance its
		// clock while a concurrent sync.Once caller waits on its internal mutex.
		use := &borrowRouteUse{repo: repo, id: "lease", ctx: ctx, cancel: cancel, done: make(chan struct{})}
		started := time.Now()
		use.finish()
		require.Equal(t, 3*time.Second, time.Since(started))
		require.Equal(t, 2, repo.attempts)
		require.Equal(t, 1, activeBorrowLeases(base))
	})
}

func TestTicketBorrowRouteLeaseSpansStreamAndReleasesOnce(t *testing.T) {
	s, repo := borrowFixture()
	req := borrowRequest(t)
	req.Header.Set("session_id", "conversation")
	resp, err := s.ForwardHTTP(req, "", 11, 10, nil, func(req *http.Request, _ string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		require.EqualValues(t, 11, id, "all routes keep the same account concurrency resource")
		require.Equal(t, 10, concurrency)
		require.Equal(t, 1, activeBorrowLeases(repo))
		require.NoError(t, BorrowBeforeSend(req))
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.created\",\"response\":{\"id\":\"response-a\"}}\n\ndata: [DONE]\n\n"))}, nil
	})
	require.NoError(t, err)
	require.Equal(t, 1, activeBorrowLeases(repo), "headers must not release occupancy")
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(data), "response-a")
	require.Zero(t, activeBorrowLeases(repo))
	bound, found, err := repo.LookupSession(t.Context(), 11, borrowResponseKey("response-a"))
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "route-a", bound)
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = resp.Body.Close() }()
	}
	wg.Wait()
	repo.mu.Lock()
	require.Equal(t, 1, repo.releases)
	repo.mu.Unlock()
}

func TestTicketBorrowRouteLeaseErrorAndCancellation(t *testing.T) {
	for _, scenario := range []string{"send-error", "nil-body", "empty-body", "close", "read-error", "cancel", "pre-send-rejected"} {
		t.Run(scenario, func(t *testing.T) {
			s, repo := borrowFixture()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			reader, writer := io.Pipe()
			defer writer.Close()
			resp, err := s.ForwardHTTP(borrowRequest(t).WithContext(ctx), "", 11, 10, nil, func(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
				switch scenario {
				case "send-error":
					return nil, errors.New("transport error")
				case "pre-send-rejected":
					repo.mu.Lock()
					repo.renewError = errors.New("lost lease")
					repo.mu.Unlock()
					return nil, BorrowBeforeSend(req)
				case "nil-body":
					return &http.Response{StatusCode: 200, Header: http.Header{}}, nil
				case "empty-body":
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: http.NoBody}, nil
				default:
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: reader}, nil
				}
			})
			if scenario == "send-error" || scenario == "pre-send-rejected" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			switch scenario {
			case "close":
				require.NoError(t, resp.Body.Close())
			case "read-error":
				_ = writer.CloseWithError(io.ErrUnexpectedEOF)
				_, err = io.ReadAll(resp.Body)
				require.ErrorIs(t, err, io.ErrUnexpectedEOF)
			case "cancel":
				cancel()
				require.Eventually(t, func() bool { return activeBorrowLeases(repo) == 0 }, time.Second, time.Millisecond)
				_, err = resp.Body.Read(make([]byte, 1))
				require.Error(t, err, "cancellation must close a body even when no caller is reading")
			}
			require.Zero(t, activeBorrowLeases(repo))
			repo.mu.Lock()
			require.Equal(t, 1, repo.releases)
			repo.mu.Unlock()
		})
	}
}

type countedBorrowBody struct{ closes atomic.Int32 }

func (b *countedBorrowBody) Close() error { b.closes.Add(1); return nil }

func TestTicketBorrowRouteLeaseRenewalAndLoss(t *testing.T) {
	_, repo := borrowFixture()
	repo.leases = map[string]string{"lease": "route-a"}
	use := newBorrowRouteUse(t.Context(), repo, "lease", 5*time.Millisecond)
	body := &countedBorrowBody{}
	use.attach(body)
	require.Eventually(t, func() bool {
		repo.mu.Lock()
		defer repo.mu.Unlock()
		return repo.renewals >= 2
	}, time.Second, time.Millisecond)
	require.Equal(t, 1, activeBorrowLeases(repo))
	repo.mu.Lock()
	repo.renewError = errors.New("database unavailable")
	repo.mu.Unlock()
	require.Eventually(t, func() bool { return activeBorrowLeases(repo) == 0 }, time.Second, time.Millisecond)
	require.ErrorIs(t, use.ctx.Err(), context.Canceled)
	require.EqualValues(t, 1, body.closes.Load())
	late := &countedBorrowBody{}
	use.attach(late)
	require.EqualValues(t, 1, late.closes.Load(), "a body arriving after lease loss is closed")
}

func TestTicketBorrowRouteChecksDoNotReserve(t *testing.T) {
	s, repo := borrowFixture()
	for range 5 {
		require.NoError(t, s.Check(t.Context(), 11, "gpt-6-astra", "http"))
		_, err := s.Status(t.Context(), 11)
		require.NoError(t, err)
	}
	require.Zero(t, activeBorrowLeases(repo))
}

func TestBorrowBusinessPreservesNormalUsageDraining(t *testing.T) {
	for _, withOwner := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-key-owner", true: "key-owner"}[withOwner], func(t *testing.T) {
			s, repo := borrowFixture()
			client, cancelClient := context.WithCancel(t.Context())
			defer cancelClient()
			if withOwner {
				var closeOwner context.CancelFunc
				client, closeOwner = WithAPIKeyAdmissionOwner(client)
				defer closeOwner()
			}
			upstream, release := detachUpstreamContext(client)
			defer release()
			upstream, releaseNested := detachUpstreamContext(upstream)
			defer releaseNested()
			reader, writer := io.Pipe()
			defer writer.Close()
			var transport context.Context
			resp, err := s.ForwardHTTP(borrowRequest(t).WithContext(upstream), "", 11, 5, nil, func(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
				transport = req.Context()
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: reader}, nil
			})
			require.NoError(t, err)
			readDone := make(chan error, 1)
			go func() { _, err := io.ReadAll(resp.Body); readDone <- err }()
			cancelClient()
			require.NoError(t, upstream.Err(), "normal usage-draining context remains detached")
			select {
			case <-readDone:
				t.Fatal("borrowed stream stopped before normal usage draining completed")
			case <-time.After(20 * time.Millisecond):
			}
			require.NoError(t, transport.Err())
			require.Equal(t, 1, activeBorrowLeases(repo))
			_, err = writer.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_drained\",\"usage\":{\"output_tokens\":5}}}\n\n"))
			require.NoError(t, err)
			require.NoError(t, writer.Close())
			require.NoError(t, <-readDone)
			require.Eventually(t, func() bool { return activeBorrowLeases(repo) == 0 }, time.Second, time.Millisecond)
			require.ErrorIs(t, transport.Err(), context.Canceled)
			require.NoError(t, resp.Body.Close())
			repo.mu.Lock()
			require.Equal(t, 1, repo.releases)
			repo.mu.Unlock()
		})
	}
}

func TestBorrowClientCancellationDuringHeaders(t *testing.T) {
	s, repo := borrowFixture()
	client, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err := s.ForwardHTTP(borrowRequest(t).WithContext(client), "", 11, 5, nil, func(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		cancel()
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-time.After(time.Second):
			t.Fatal("borrowed header wait did not stop")
			return nil, nil
		}
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, activeBorrowLeases(repo))
}
