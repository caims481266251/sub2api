package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type borrowCompletionRepo struct {
	*borrowTestRepo
	cookieObservations int
	refreshes          int
}

func (r *borrowCompletionRepo) ObserveCookie(_ context.Context, _ string, ttl time.Duration) (time.Time, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cookieObservations++
	return time.Now().Add(ttl), nil
}

func (r *borrowCompletionRepo) RefreshTarget(context.Context, int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshes++
	return nil
}

func borrowChangedResponseHeaders(change string) http.Header {
	headers := http.Header{"Content-Type": {"text/event-stream"}}
	switch change {
	case "ticket":
		headers.Set("X-Codex-Turn-State", "rotated")
	case "cookie":
		headers.Set("Set-Cookie", "__oailb=changed; Path=/")
	case "expired-cookie":
		headers.Set("Set-Cookie", "__oailb=donor-cookie; Expires=Thu, 01 Jan 1970 00:00:00 GMT; Path=/")
	case "deleted-cookie":
		headers.Set("Set-Cookie", "__oailb=donor-cookie; Max-Age=0; Path=/")
	case "shortened-cookie":
		headers.Set("Set-Cookie", "__oailb=donor-cookie; Max-Age=1; Path=/")
	case "extended-cookie":
		headers.Set("Set-Cookie", "__oailb=donor-cookie; Max-Age=3600; Path=/")
	}
	return headers
}

const borrowCompletionPayload = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"done\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_finished\",\"usage\":{\"output_tokens\":1}}}\n\ndata: [DONE]\n\n"

func TestBorrowChangedResponseKeepsAdmissionsUntilExpiryAndDrains(t *testing.T) {
	for _, change := range []string{"ticket", "cookie", "expired-cookie", "deleted-cookie", "shortened-cookie", "extended-cookie"} {
		t.Run(change, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, repo := borrowFixture()
				observations := &borrowCompletionRepo{borrowTestRepo: repo}
				s.repo = observations
				var transports []context.Context
				calls := 0
				deadline := repo.route.ExpiresAt
				send := func(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
					calls++
					transports = append(transports, req.Context())
					require.NoError(t, BorrowBeforeTransport(req))
					cookie, err := req.Cookie("__oailb")
					require.NoError(t, err)
					require.Equal(t, "donor-cookie", cookie.Value, "other responses must not overwrite the published Cookie")
					return &http.Response{StatusCode: 200, Header: borrowChangedResponseHeaders(change), Body: io.NopCloser(strings.NewReader(borrowCompletionPayload))}, nil
				}
				first, err := s.ForwardHTTP(borrowRequest(t), "", 11, 2, nil, send)
				require.NoError(t, err)
				t.Cleanup(func() { _ = first.Body.Close() })
				require.NoError(t, s.Check(t.Context(), 11, "gpt-6-astra", "http"))
				second, err := s.ForwardHTTP(borrowRequest(t), "", 11, 2, nil, send)
				require.NoError(t, err, "changed response metadata must not block the next admission")
				t.Cleanup(func() { _ = second.Body.Close() })
				require.Equal(t, 2, activeBorrowLeases(repo))
				time.Sleep(61 * time.Second)
				synctest.Wait()
				repo.mu.Lock()
				repo.now = time.Now()
				require.Equal(t, deadline, repo.route.ExpiresAt)
				require.Empty(t, repo.revoked)
				require.Empty(t, repo.events, "metadata observations must not be reported as route revocations")
				require.Zero(t, observations.cookieObservations)
				require.Zero(t, observations.refreshes)
				require.GreaterOrEqual(t, repo.renewals, 8, "both admitted requests renew beyond the route deadline")
				repo.mu.Unlock()
				require.ErrorIs(t, s.Check(t.Context(), 11, "gpt-6-astra", "http"), ErrBorrowUnavailable)
				_, err = s.ForwardHTTP(borrowRequest(t), "", 11, 2, nil, send)
				require.ErrorIs(t, err, ErrBorrowUnavailable, "expiry blocks new work")
				for _, transport := range transports {
					require.NoError(t, transport.Err(), "expiry must not cancel an admitted response")
				}
				require.Equal(t, 2, activeBorrowLeases(repo))
				for i, resp := range []*http.Response{first, second} {
					got, err := io.ReadAll(resp.Body)
					require.NoError(t, err)
					require.Equal(t, borrowCompletionPayload, string(got))
					require.NoError(t, resp.Body.Close())
					require.Equal(t, 1-i, activeBorrowLeases(repo))
				}
				require.Equal(t, 2, calls, "no replay after response changes or expiry")
				require.Equal(t, 2, repo.releases)
			})
		})
	}
}

func TestBorrowChangedResponseDoesNotCancelAnotherRequestWaitingForHeaders(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, repo := borrowFixture()
		sent := make(chan context.Context, 1)
		finishHeaders := make(chan struct{})
		result := make(chan *http.Response, 1)
		errors := make(chan error, 1)
		go func() {
			resp, err := s.ForwardHTTP(borrowRequest(t), "", 11, 2, nil, func(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
				if err := BorrowBeforeTransport(req); err != nil {
					return nil, err
				}
				sent <- req.Context()
				select {
				case <-finishHeaders:
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(borrowCompletionPayload))}, nil
				case <-req.Context().Done():
					return nil, req.Context().Err()
				}
			})
			result <- resp
			errors <- err
		}()
		transport := <-sent
		second, err := s.ForwardHTTP(borrowRequest(t), "", 11, 2, nil, func(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
			require.NoError(t, BorrowBeforeTransport(req))
			return &http.Response{StatusCode: 200, Header: borrowChangedResponseHeaders("cookie"), Body: io.NopCloser(strings.NewReader(borrowCompletionPayload))}, nil
		})
		require.NoError(t, err)
		time.Sleep(61 * time.Second)
		synctest.Wait()
		repo.mu.Lock()
		repo.now = time.Now()
		repo.mu.Unlock()
		require.ErrorIs(t, s.Check(t.Context(), 11, "gpt-6-astra", "http"), ErrBorrowUnavailable)
		require.NoError(t, transport.Err(), "a sent request with no response headers yet must remain active")
		require.Equal(t, 2, activeBorrowLeases(repo))
		close(finishHeaders)
		first := <-result
		require.NoError(t, <-errors)
		for _, resp := range []*http.Response{first, second} {
			got, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, borrowCompletionPayload, string(got))
			require.NoError(t, resp.Body.Close())
		}
		require.Zero(t, activeBorrowLeases(repo))
		require.Empty(t, repo.revoked)
	})
}

func TestBorrowChangedResponsePreservesGatewayCompletionAndUsage(t *testing.T) {
	borrow, repo := borrowFixture()
	calls := 0
	upstream := &borrowAdmissionTestUpstream{borrow: borrow, send: func(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		calls++
		require.NoError(t, BorrowBeforeSend(req))
		resp := openAICompatSSECompletedResponse("resp_finish_after_change", "gpt-6-astra")
		resp.Header.Set("Set-Cookie", "__oailb=changed; Path=/")
		return resp, nil
	}}
	gateway := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream, borrowRoutes: borrow, accountRepo: borrow.owner.accountRepo}
	account, err := borrow.owner.accountRepo.GetByID(t.Context(), 11)
	require.NoError(t, err)
	body := []byte(`{"model":"gpt-6-astra","stream":true,"input":"hello"}`)
	c, rec := newOpenAIAstraProRetryContext(body)
	result, err := gateway.Forward(t.Context(), c, account, body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "response.completed")
	require.Contains(t, rec.Body.String(), "resp_finish_after_change")
	require.EqualValues(t, 5, result.Usage.InputTokens)
	require.EqualValues(t, 2, result.Usage.OutputTokens)
	require.Equal(t, 1, calls)
	require.Empty(t, repo.revoked)
	require.Zero(t, activeBorrowLeases(repo))
}
