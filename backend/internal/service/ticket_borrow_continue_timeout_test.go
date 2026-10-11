package service

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type continuePolicyRepo struct {
	*borrowTestRepo
	seconds  *int
	requests []BorrowRequest
	outcomes []string
	active   int
}

func (r *continuePolicyRepo) Settings(context.Context) (BorrowSettings, error) {
	return BorrowSettings{Running: true, ContinueTimeoutSeconds: r.seconds}, nil
}
func (r *continuePolicyRepo) BeginTargetRequest(_ context.Context, q BorrowRequest) error {
	r.requests = append(r.requests, q)
	r.active++
	return nil
}
func (r *continuePolicyRepo) EndTargetRequest(_ context.Context, _ BorrowRequest, _ int, outcome string) error {
	r.outcomes = append(r.outcomes, outcome)
	r.active--
	return nil
}

type continueTestBody struct {
	read    func([]byte) (int, error)
	closed  bool
	onClose func()
}

func (b *continueTestBody) Read(p []byte) (int, error) { return b.read(p) }
func (b *continueTestBody) Close() error {
	b.closed = true
	if b.onClose != nil {
		b.onClose()
	}
	return nil
}

type continuePolicyUpstream struct {
	HTTPUpstream
	s    *TicketBorrowService
	send func(*http.Request) (*http.Response, error)
}

func (u *continuePolicyUpstream) DoWithTLS(req *http.Request, proxy string, id int64, n int, p *tlsfingerprint.Profile) (*http.Response, error) {
	return u.s.trackTargetRequest(req, BorrowRequest{Kind: "probe"}, proxy, id, n, p, func(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		return u.send(req)
	})
}
func TestBorrowContinueTimeoutCancelsOnlyContinuationAndNeverRetries(t *testing.T) {
	for _, mode := range []string{"headers", "max_timeout", "body", "close_only", "late_success", "normal", "http401", "http429", "sse429", "parent_cancel"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, base := borrowFixture()
				r := &continuePolicyRepo{borrowTestRepo: base}
				timeout := 7
				if mode == "max_timeout" {
					timeout = 45
					r.seconds = &timeout
				}
				s.repo = r
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				ctx = context.WithValue(ctx, borrowInternalKey{}, true)
				ctx = context.WithValue(ctx, borrowProbeRequestKey{}, BorrowRequest{Kind: "probe"})
				var body *continueTestBody
				calls := 0
				up := &continuePolicyUpstream{s: s, send: func(req *http.Request) (*http.Response, error) {
					calls++
					if req.Header.Get(openAICodexTurnStateHeader) == "" {
						// A slow mint must retain the original timeout, independently of continue.
						time.Sleep(8 * time.Second)
						return &http.Response{StatusCode: 200, Header: http.Header{"X-Codex-Turn-State": []string{"ticket"}}, Body: io.NopCloser(strings.NewReader(stateProbeCompletedStream))}, nil
					}
					if mode == "headers" || mode == "max_timeout" {
						<-req.Context().Done()
						return nil, req.Context().Err()
					}
					if mode == "normal" {
						time.Sleep(6 * time.Second)
						return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(stateProbeCompletedStream))}, nil
					}
					status := 200
					if mode == "http401" {
						status = 401
					}
					if mode == "http429" {
						status = 429
					}
					first := true
					closed := make(chan struct{})
					body = &continueTestBody{onClose: func() { close(closed) }, read: func(p []byte) (int, error) {
						if mode == "sse429" && first {
							first = false
							return copy(p, "data: {\"type\":\"error\",\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"Rate limit exceeded\"}}\n\n"), nil
						}
						if mode == "close_only" {
							<-closed
							return 0, io.EOF
						}
						if mode == "parent_cancel" {
							cancel()
						}
						<-req.Context().Done()
						if mode == "late_success" {
							return copy(p, stateProbeCompletedStream), io.EOF
						}
						return 0, req.Context().Err()
					}}
					return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": []string{"80"}}, Body: body}, nil
				}}
				req, _ := http.NewRequestWithContext(ctx, "POST", chatgptCodexURL, nil)
				req.Header.Set("Cookie", "__oailb=borrowed")
				start := time.Now()
				result := ProbeOpenAICodexStateRoute(ctx, up, req, "", 11, 2, nil)
				require.Equal(t, 2, calls)
				require.Zero(t, result.RetryCount)
				require.Zero(t, r.active)
				require.Len(t, r.outcomes, 2)
				require.Zero(t, r.requests[0].ContinueTimeoutSeconds)
				require.Equal(t, timeout, r.requests[1].ContinueTimeoutSeconds)
				switch mode {
				case "normal":
					require.Equal(t, OpenAICodexStateHealthy, result.Verdict)
					require.Equal(t, 14*time.Second, time.Since(start))
				case "http401":
					require.Equal(t, OpenAICodexStateFailureAccountError, result.Failure)
				case "http429", "sse429":
					require.Equal(t, OpenAICodexStateFailureRateLimited, result.Failure)
				case "parent_cancel":
					require.Equal(t, OpenAICodexStateFailureCancelled, result.Failure)
					require.NotEqual(t, borrowContinueTimeoutReason, r.outcomes[1])
				default:
					require.Equal(t, borrowContinueTimeoutReason, result.Failure)
					require.Equal(t, time.Duration(8+timeout)*time.Second, time.Since(start))
					require.Equal(t, borrowContinueTimeoutReason, r.outcomes[1])
				}
				if mode != "normal" {
					require.NotEqual(t, OpenAICodexStateHealthy, result.Verdict)
				}
				if body != nil {
					require.True(t, body.closed)
				}
			})
		})
	}
}

func TestBorrowContinuePolicyUpdatesOnlyNewRequests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, base := borrowFixture()
		seconds := 7
		r := &continuePolicyRepo{borrowTestRepo: base, seconds: &seconds}
		s.repo = r
		req, _ := http.NewRequestWithContext(t.Context(), "POST", chatgptCodexURL, nil)
		req.Header.Set(openAICodexTurnStateHeader, "ticket")
		var captured []context.Context
		send := func(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
			captured = append(captured, req.Context())
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("done"))}, nil
		}
		first, err := s.trackTargetRequest(req, BorrowRequest{Kind: "probe"}, "", 11, 2, nil, send)
		require.NoError(t, err)
		seconds = 3
		second, err := s.trackTargetRequest(req, BorrowRequest{Kind: "probe"}, "", 11, 2, nil, send)
		require.NoError(t, err)
		d1, _ := captured[0].Deadline()
		d2, _ := captured[1].Deadline()
		require.Equal(t, 4*time.Second, d1.Sub(d2))
		time.Sleep(4 * time.Second)
		_, err = io.ReadAll(first.Body)
		require.NoError(t, err)
		_, err = io.ReadAll(second.Body)
		require.True(t, borrowContinueTimedOut(err))
		require.NoError(t, first.Body.Close())
		require.NoError(t, second.Body.Close())
		require.Zero(t, r.active)
		require.Len(t, r.outcomes, 2)
	})
}

func TestBorrowContinueTimeoutSettingsValidation(t *testing.T) {
	s, _ := borrowFixture()
	for _, n := range []int{0, -1, 46} {
		v := n
		err := s.SaveSettings(t.Context(), BorrowSettings{TTLSeconds: 230, ContinueTimeoutSeconds: &v})
		require.ErrorContains(t, err, "borrow_settings_invalid")
	}
	// Ordinary transport deadlines still follow the existing retry classification.
	require.True(t, openAICodexShotRetryable(t.Context(), openAICodexStateShot{}, context.DeadlineExceeded, false))
	require.False(t, openAICodexShotRetryable(t.Context(), openAICodexStateShot{}, &borrowContinueTimeoutError{Seconds: 7}, false))
	require.False(t, borrowContinueTimedOut(errors.New("unrelated")))
}

type continueRejectedRepo struct {
	*borrowWorkerRepo
	rejected  map[string]bool
	rejectErr error
}

func (r *continueRejectedRepo) CandidateKnown(_ context.Context, _ int64, identity, cookie string) (bool, error) {
	return r.rejected[identity+":"+cookie], nil
}
func (r *continueRejectedRepo) RejectCandidate(_ context.Context, _ int64, identity, cookie string, _ time.Time) error {
	if r.rejectErr != nil {
		return r.rejectErr
	}
	r.rejected[identity+":"+cookie] = true
	return nil
}
func TestBorrowContinueTimeoutRejectsCandidateAcrossBatchesAndAllowsNewRoute(t *testing.T) {
	s, base := borrowFixture()
	base.route = nil
	r := &continueRejectedRepo{borrowWorkerRepo: &borrowWorkerRepo{borrowTestRepo: base, source: boundBorrowTestSource(BorrowSource{ID: 1, Enabled: true, Model: "gpt-6-luna", Secret: BorrowSourceSecret{Credentials: map[string]any{"access_token": "source-token", "chatgpt_account_id": "source"}, DeviceID: "source-device"}})}, rejected: map[string]bool{}}
	s.repo = r
	s.owner.openaiGatewayService = &OpenAIGatewayService{accountRepo: s.owner.accountRepo}
	source := func(host string) stateProbeReply {
		return stateProbeReply{status: 200, cookies: []string{"__oailb=x." + base64.RawURLEncoding.EncodeToString([]byte(`{"host":"`+host+`"}`)) + ".x; Max-Age=230; Secure"}, body: stateProbeCompletedStream}
	}
	mint := stateProbeMint("target-ticket")
	mint.cookies = nil
	up := &stateProbeUpstream{replies: []stateProbeReply{source("chat.gateway.unified-1.api.openai.com"), mint, {err: &borrowContinueTimeoutError{Seconds: 7}}}}
	s.owner.httpUpstream = up
	run := func() {
		s.runTarget(t.Context(), BorrowTarget{AccountID: 11, Revision: 2}, BorrowSettings{Running: true, TTLSeconds: 230}, "worker")
	}
	run()
	require.Len(t, up.calls, 3)
	require.Nil(t, base.route)
	require.Len(t, r.rejected, 1)
	found := false
	for _, e := range base.events {
		if e.Reason == borrowContinueTimeoutReason {
			found = true
		}
	}
	require.True(t, found)
	up.calls = nil
	up.replies = []stateProbeReply{source("chat.gateway.unified-1.api.openai.com")}
	run()
	require.Len(t, up.calls, 1, "same candidate must not trigger another target request")
	require.Nil(t, base.route)
	up.calls = nil
	up.replies = []stateProbeReply{source("chat.gateway.unified-2.api.openai.com"), mint, {status: 200, body: stateProbeCompletedStream}}
	run()
	require.Len(t, up.calls, 3)
	require.NotNil(t, base.route)
}
