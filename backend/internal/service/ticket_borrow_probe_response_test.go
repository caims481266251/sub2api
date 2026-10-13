package service

import (
	"context"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Never ends until the request is cancelled, like an upstream stream that
// emits a completed event but keeps the HTTP connection open.
type borrowHangingProbeBody struct {
	ctx    context.Context
	prefix *strings.Reader
	closed bool
}

func (b *borrowHangingProbeBody) Read(p []byte) (int, error) {
	if b.prefix.Len() > 0 {
		return b.prefix.Read(p)
	}
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}
func (b *borrowHangingProbeBody) Close() error { b.closed = true; return nil }

func TestBorrowProbeRejectsChangedTicketWithoutWaitingForOutput(t *testing.T) {
	for _, prefix := range []string{"", "data: {\"type\":\"response.created\"}\n\n"} {
		t.Run(prefix, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.WithValue(t.Context(), borrowProbeResponseKey{}, true), 2*time.Second)
			defer cancel()
			var body *borrowHangingProbeBody
			started := time.Now()
			shot, err := fireOpenAICodexStateShotRequest(ctx, http.Header{}, "gpt-6-astra", "old", "__oailb=fixed", func(req *http.Request) (*http.Response, error) {
				body = &borrowHangingProbeBody{ctx: req.Context(), prefix: strings.NewReader(prefix)}
				return &http.Response{StatusCode: 200, Header: http.Header{"X-Codex-Turn-State": []string{"new"}}, Body: body}, nil
			})
			require.NoError(t, err)
			require.Equal(t, "ticket_changed", shot.earlyRejected)
			require.True(t, body.closed)
			require.Less(t, time.Since(started), time.Second)
			require.NoError(t, ctx.Err())
		})
	}
}

func TestBorrowProbeTerminalDoesNotWaitForEOF(t *testing.T) {
	for _, prefix := range []string{stateProbeCompletedStream, stateProbeFailedStream} {
		ctx, cancel := context.WithTimeout(context.WithValue(t.Context(), borrowProbeResponseKey{}, true), 2*time.Second)
		var body *borrowHangingProbeBody
		started := time.Now()
		shot, err := fireOpenAICodexStateShotRequest(ctx, http.Header{}, "gpt-6-astra", "old", "", func(req *http.Request) (*http.Response, error) {
			body = &borrowHangingProbeBody{ctx: req.Context(), prefix: strings.NewReader(prefix)}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: body}, nil
		})
		require.NoError(t, err)
		require.Less(t, time.Since(started), time.Second)
		require.True(t, body.closed)
		if prefix == stateProbeCompletedStream {
			require.NoError(t, shot.streamErr)
		} else {
			require.Error(t, shot.streamErr)
		}
		cancel()
	}
}

func TestBorrowProbeEarlyRejectionRetainsHealthErrors(t *testing.T) {
	for _, tc := range []struct {
		status        int
		body, failure string
	}{
		{429, `{"error":{"code":"rate_limit_exceeded"}}`, OpenAICodexStateFailureRateLimited},
		{401, `{"error":{"code":"invalid_token"}}`, OpenAICodexStateFailureAccountError},
		{200, "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"usage_limit_reached\"}}}\n\n", OpenAICodexStateFailureRateLimited},
		{200, "data: {\"type\":\"error\",\"error\":{\"code\":\"invalid_token\"}}\n\n", OpenAICodexStateFailureAccountError},
	} {
		ctx := context.WithValue(t.Context(), borrowProbeResponseKey{}, true)
		shot, err := fireOpenAICodexStateShotRequest(ctx, http.Header{}, "gpt-6-astra", "old", "", func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: tc.status, Header: http.Header{"X-Codex-Turn-State": []string{"new"}}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
		})
		result := &OpenAICodexStateProbeResult{}
		require.False(t, result.shotUsable(ctx, "", shot, err))
		require.Equal(t, tc.failure, result.Failure)
	}
}

func TestBorrowRenewalLeadIncludesLongSearch(t *testing.T) {
	require.Equal(t, 110, BorrowRenewalLead(110, 190000))
	require.Equal(t, 20, BorrowRenewalLead(110, 12000))
	require.Equal(t, 15, BorrowRenewalLead(110, 0))
}

func TestBorrowProbeEarlyReaderDoesNotChangeOrdinaryProbe(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	shot, err := fireOpenAICodexStateShotRequest(ctx, http.Header{}, "gpt-6-astra", "old", "", func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: &borrowHangingProbeBody{ctx: req.Context(), prefix: strings.NewReader(stateProbeCompletedStream)}}, nil
	})
	require.Error(t, err, "ordinary probe did not opt in to early terminal reading")
	require.Empty(t, shot.earlyRejected)
}
