package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBorrowedRouteReusesStateProbeVerdict(t *testing.T) {
	for _, tc := range []struct {
		name        string
		first, next stateProbeReply
		verdict     OpenAICodexStateVerdict
		failure     string
		calls       int
	}{
		{"unchanged", stateProbeMint("mint-secret"), stateProbeReply{status: 200, body: stateProbeCompletedStream}, OpenAICodexStateHealthy, "", 2},
		{"echo", stateProbeMint("mint-secret"), stateProbeReply{status: 200, state: "mint-secret", body: stateProbeCompletedStream}, OpenAICodexStateHealthy, "", 2},
		{"new ticket", stateProbeMint("mint-secret"), stateProbeReply{status: 200, state: "new-secret", body: stateProbeCompletedStream}, OpenAICodexStateDegraded, "", 2},
		{"missing ticket", stateProbeReply{status: 200, body: stateProbeCompletedStream}, stateProbeReply{}, OpenAICodexStateInconclusive, OpenAICodexStateFailureNoTicket, 3},
		{"rate limited", stateProbeMint("mint-secret"), stateProbeReply{status: 429, body: `{"error":{"message":"limited"}}`}, OpenAICodexStateInconclusive, OpenAICodexStateFailureRateLimited, 4},
		{"stream failed", stateProbeMint("mint-secret"), stateProbeReply{status: 200, state: "new-secret", body: stateProbeFailedStream}, OpenAICodexStateInconclusive, OpenAICodexStateFailureStreamError, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The source Cookie stays fixed; the target's own mint supplies __cflb.
			tc.first.cookies = []string{"__cflb=target-lb; Path=/; Secure"}
			replies := []stateProbeReply{tc.first, tc.next}
			if tc.failure == OpenAICodexStateFailureNoTicket {
				replies = []stateProbeReply{tc.first, tc.first, tc.first}
			} else if tc.calls == 4 {
				replies = append(replies, tc.next, tc.next)
			}
			up := &stateProbeUpstream{replies: replies}
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, chatgptCodexURL, nil)
			req.Header.Set("Authorization", "Bearer target-secret")
			req.Header.Set("ChatGPT-Account-ID", "target-id")
			req.Header.Set("Cookie", "__oailb=borrowed-secret; custom=not-for-probe")
			req.Header.Set(openAICodexTurnStateHeader, "business-state")
			req.Header.Set(responsesLiteHeaderKey, "1")
			result := ProbeOpenAICodexStateRoute(t.Context(), up, req, "leased-exit", 202, 1, nil)
			require.Equal(t, tc.verdict, result.Verdict)
			require.Equal(t, tc.failure, result.Failure)
			require.Len(t, up.calls, tc.calls)
			for i, c := range up.calls {
				require.Equal(t, "leased-exit", c.proxy)
				require.False(t, c.close)
				require.Equal(t, "Bearer target-secret", c.header.Get("Authorization"))
				require.Equal(t, "target-id", c.header.Get("ChatGPT-Account-ID"))
				require.Contains(t, c.header.Get("Cookie"), "__oailb=borrowed-secret")
				require.NotContains(t, c.header.Get("Cookie"), "custom=")
				require.Empty(t, c.header.Get(responsesLiteHeaderKey))
				require.Contains(t, c.body, "Reply with OK.")
				require.NotContains(t, c.body, "糖果")
				if i == 0 || tc.failure == OpenAICodexStateFailureNoTicket {
					require.Empty(t, c.header.Get(openAICodexTurnStateHeader))
				} else {
					require.Equal(t, "mint-secret", c.header.Get(openAICodexTurnStateHeader))
					require.Contains(t, c.header.Get("Cookie"), "__cflb=target-lb")
				}
			}
			raw, err := json.Marshal(result)
			require.NoError(t, err)
			for _, secret := range []string{"mint-secret", "new-secret", "target-secret", "borrowed-secret", "business-state"} {
				require.NotContains(t, string(raw), secret)
			}
			require.Equal(t, "business-state", req.Header.Get(openAICodexTurnStateHeader))
		})
	}
}

func TestBorrowedRouteRejectsReassignmentAndCancellation(t *testing.T) {
	for _, cookie := range []string{"__oailb=changed; Path=/", "__oailb=borrowed; Max-Age=0; Path=/", "__oailb=borrowed; Expires=Thu, 01 Jan 1970 00:00:00 GMT; Path=/"} {
		for _, step := range []int{0, 1} {
			replies := []stateProbeReply{stateProbeMint("mint"), {status: 200, body: stateProbeCompletedStream}}
			replies[0].cookies = nil
			replies[step].cookies = []string{cookie}
			up := &stateProbeUpstream{replies: replies}
			req, _ := http.NewRequest(http.MethodPost, chatgptCodexURL, nil)
			req.Header.Set("Cookie", "__oailb=borrowed")
			result := ProbeOpenAICodexStateRoute(t.Context(), up, req, "exit", 202, 1, nil)
			require.Equal(t, OpenAICodexStateInconclusive, result.Verdict)
			require.Equal(t, "route_changed", result.Failure)
			require.Len(t, up.calls, step+1)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	up := &stateProbeUpstream{replies: []stateProbeReply{{err: context.Canceled}}}
	req, _ := http.NewRequest(http.MethodPost, chatgptCodexURL, strings.NewReader(""))
	req.Header.Set("Cookie", "__oailb=borrowed")
	result := ProbeOpenAICodexStateRoute(ctx, up, req, "exit", 202, 1, nil)
	require.Equal(t, OpenAICodexStateInconclusive, result.Verdict)
	require.Equal(t, OpenAICodexStateFailureCancelled, result.Failure)
}

func TestBorrowedRouteRetriesTransientPhase(t *testing.T) {
	for _, tc := range []struct {
		name   string
		failed stateProbeReply
		mint   bool
	}{
		{"network mint", stateProbeReply{err: errors.New("connection reset")}, true},
		{"timeout mint", stateProbeReply{err: context.DeadlineExceeded}, true},
		{"missing mint ticket", stateProbeReply{status: 200, body: stateProbeCompletedStream}, true},
		{"rate limit continuation", stateProbeReply{status: 429}, false},
		{"unavailable continuation", stateProbeReply{status: 503}, false},
		{"stream continuation", stateProbeReply{status: 200, body: stateProbeFailedStream}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mint := stateProbeMint("target-ticket")
			mint.cookies = []string{"__cflb=target-lb; Path=/"}
			cont := stateProbeReply{status: 200, body: stateProbeCompletedStream}
			replies := []stateProbeReply{mint, tc.failed, cont}
			if tc.mint {
				replies = []stateProbeReply{tc.failed, mint, cont}
			}
			up := &stateProbeUpstream{replies: replies}
			req, _ := http.NewRequest(http.MethodPost, chatgptCodexURL, nil)
			req.Header.Set("Cookie", "__oailb=borrowed")
			req.Header.Set("Authorization", "Bearer target")
			req.Header.Set("ChatGPT-Account-ID", "target-id")
			result := ProbeOpenAICodexStateRoute(t.Context(), up, req, "fixed-exit", 202, 1, nil)
			require.Equal(t, OpenAICodexStateHealthy, result.Verdict)
			require.Equal(t, 1, result.RetryCount)
			require.Len(t, up.calls, 3)
			for _, call := range up.calls {
				require.Equal(t, "fixed-exit", call.proxy)
				require.Equal(t, "Bearer target", call.header.Get("Authorization"))
				require.Equal(t, "target-id", call.header.Get("ChatGPT-Account-ID"))
				require.Contains(t, call.header.Get("Cookie"), "__oailb=borrowed")
				require.Equal(t, up.calls[0].body, call.body)
			}
			if !tc.mint {
				for _, call := range up.calls[1:] {
					require.Equal(t, "target-ticket", call.header.Get(openAICodexTurnStateHeader))
					require.Contains(t, call.header.Get("Cookie"), "__cflb=target-lb")
				}
			}
		})
	}
}

func TestBorrowedRouteDoesNotRetryPermanentRejection(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 302} {
		up := &stateProbeUpstream{replies: []stateProbeReply{{status: status}}}
		req, _ := http.NewRequest(http.MethodPost, chatgptCodexURL, nil)
		req.Header.Set("Cookie", "__oailb=borrowed")
		result := ProbeOpenAICodexStateRoute(t.Context(), up, req, "exit", 202, 1, nil)
		require.Equal(t, OpenAICodexStateInconclusive, result.Verdict)
		require.Zero(t, result.RetryCount)
		require.Len(t, up.calls, 1)
	}
}

func TestBorrowedRouteRetryBackoffHonorsDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	calls := 0
	result := &OpenAICodexStateProbeResult{}
	_, err := retryOpenAICodexStateRouteShot(ctx, result, true, func(context.Context) (openAICodexStateShot, error) {
		calls++
		return openAICodexStateShot{}, errors.New("connection reset")
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, 1, calls)
	require.Zero(t, result.RetryCount)
}
