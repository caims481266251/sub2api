package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type borrowActivityTestRepo struct {
	*borrowTestRepo
	mu                 sync.Mutex
	began, ended, sent int
	blocked            bool
}

func (r *borrowActivityTestRepo) BeginTargetRequest(context.Context, BorrowRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.blocked {
		return ErrBorrowPaused
	}
	r.began++
	return nil
}
func (r *borrowActivityTestRepo) EndTargetRequest(context.Context, BorrowRequest, int, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ended++
	return nil
}
func (r *borrowActivityTestRepo) MarkTargetRequestSent(context.Context, BorrowRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent++
	return nil
}
func TestTicketBorrowActivityLivesUntilBodyEndsAndFinishesOnce(t *testing.T) {
	s, base := borrowFixture()
	r := &borrowActivityTestRepo{borrowTestRepo: base}
	s.repo = r
	req := borrowRequest(t)
	resp, err := s.ForwardHTTP(borrowActivityProbe(req), "", 11, 100, nil, func(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		require.NoError(t, BorrowTargetRequestConnected(req))
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("complete response"))}, nil
	})
	require.NoError(t, err)
	require.Equal(t, 1, r.began)
	require.Equal(t, 1, r.sent)
	require.Zero(t, r.ended)
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	r.mu.Lock()
	require.Equal(t, 1, r.ended)
	r.mu.Unlock()
}
func TestTicketBorrowActivityReservationFailureSendsNothing(t *testing.T) {
	s, base := borrowFixture()
	r := &borrowActivityTestRepo{borrowTestRepo: base, blocked: true}
	s.repo = r
	_, err := s.ForwardHTTP(borrowActivityProbe(borrowRequest(t)), "", 11, 100, nil, func(*http.Request, string, int64, int, *tlsfingerprint.Profile) (*http.Response, error) {
		t.Fatal("unexpected outbound request")
		return nil, nil
	})
	require.ErrorIs(t, err, ErrBorrowPaused)
}
func TestBorrowedRouteDoesNotRetryRecoveryGate(t *testing.T) {
	for _, gate := range []error{ErrBorrowPaused, ErrBorrowRecoveryWaiting} {
		count := 0
		result := &OpenAICodexStateProbeResult{}
		_, err := retryOpenAICodexStateRouteShot(t.Context(), result, true, func() (openAICodexStateShot, error) { count++; return openAICodexStateShot{}, gate })
		require.ErrorIs(t, err, gate)
		require.Equal(t, 1, count)
	}
}

type borrowDeferredScanRepo struct {
	*borrowWorkerRepo
	advanced int
}

func (r *borrowDeferredScanRepo) AcquireJob(context.Context, string, int64, string) (bool, error) {
	return false, nil
}
func (r *borrowDeferredScanRepo) AdvanceScan(_ context.Context, _ int64, _ string, count int) error {
	r.advanced += count
	return nil
}
func TestTicketBorrowDiagnosticDoesNotSkipUnacquiredSource(t *testing.T) {
	s, base := borrowFixture()
	r := &borrowDeferredScanRepo{borrowWorkerRepo: &borrowWorkerRepo{borrowTestRepo: base, source: BorrowSource{ID: 1, Enabled: true}}}
	s.repo = r
	s.owner.openaiGatewayService = &OpenAIGatewayService{accountRepo: s.owner.accountRepo}
	s.runTarget(t.Context(), BorrowTarget{AccountID: 11, Revision: 2, ScanSources: []int64{1}}, BorrowSettings{Running: true, TTLSeconds: 230}, "worker")
	require.Zero(t, r.advanced)
}
func TestTicketBorrowActivityTransportFailureEndsReservation(t *testing.T) {
	s, base := borrowFixture()
	r := &borrowActivityTestRepo{borrowTestRepo: base}
	s.repo = r
	_, err := s.ForwardHTTP(borrowActivityProbe(borrowRequest(t)), "", 11, 100, nil, func(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	})
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	r.mu.Lock()
	defer r.mu.Unlock()
	require.Equal(t, 1, r.began)
	require.Equal(t, 1, r.ended)
	require.Zero(t, r.sent)
}

func TestTicketBorrowCancellationWaitsForTransportToFinish(t *testing.T) {
	s, base := borrowFixture()
	r := &borrowActivityTestRepo{borrowTestRepo: base}
	s.repo = r
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered, cancelled, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_, _ = s.ForwardHTTP(borrowActivityProbe(borrowRequest(t).WithContext(ctx)), "", 11, 100, nil, func(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
			close(entered)
			<-req.Context().Done()
			close(cancelled)
			<-release
			return nil, req.Context().Err()
		})
	}()
	<-entered
	cancel()
	<-cancelled
	r.mu.Lock()
	require.Zero(t, r.ended, "cancel alone must not declare the target idle")
	r.mu.Unlock()
	close(release)
	<-done
	r.mu.Lock()
	defer r.mu.Unlock()
	require.Equal(t, 1, r.ended)
}

func TestTicketBorrowRetryAfterCannotOverflowIntoEarlyRetry(t *testing.T) {
	now := time.Date(2026, 10, 10, 0, 0, 0, 500000000, time.UTC)
	require.Equal(t, 24*time.Hour, borrowRetryAfter("9223372036854775807", now))
	require.Equal(t, 61*time.Second, borrowRetryAfter("61", now))
	require.Equal(t, 61500*time.Millisecond, borrowRetryAfter(now.Add(62*time.Second).Format(http.TimeFormat), now))
	require.Equal(t, time.Minute, borrowRetryAfter("invalid", now))
}

func borrowActivityProbe(req *http.Request) *http.Request {
	ctx := context.WithValue(req.Context(), borrowInternalKey{}, true)
	ctx = context.WithValue(ctx, borrowProbeRequestKey{}, BorrowRequest{Kind: "probe"})
	return req.WithContext(ctx)
}
func TestTicketBorrowBusinessIgnoresProbeReservationFailure(t *testing.T) {
	s, base := borrowFixture()
	r := &borrowActivityTestRepo{borrowTestRepo: base, blocked: true}
	s.repo = r
	resp, err := s.ForwardHTTP(borrowRequest(t), "", 11, 1, nil, func(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		require.NoError(t, BorrowTargetRequestConnected(req))
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: http.NoBody}, nil
	})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Zero(t, r.began)
	require.Zero(t, r.sent)
	require.Zero(t, r.ended)
}
