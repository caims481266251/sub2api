package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCodexProbeStructuredSignals(t *testing.T) {
	now := time.Unix(1800000000, 0)
	for _, tc := range []struct {
		name, body, header, failure string
		wait                        time.Duration
	}{
		{"code", `{"error":{"code":"rate_limit_exceeded"}}`, "", OpenAICodexStateFailureRateLimited, time.Minute},
		{"type quota", `{"error":{"type":"usage_limit_reached"}}`, "", OpenAICodexStateFailureRateLimited, 5 * time.Minute},
		{"auth", `{"error":{"type":"authentication_error"}}`, "", OpenAICodexStateFailureAccountError, 5 * time.Minute},
		{"nested code and type", `{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded","type":"usage_limit_reached","resets_at":1800003600}}}`, "120", OpenAICodexStateFailureRateLimited, time.Hour},
		{"seconds string", `{"error":{"type":"insufficient_quota","resets_in_seconds":"700"}}`, "600", OpenAICodexStateFailureRateLimited, 700 * time.Second},
		{"retry date", `{"error":{"code":"rate_limit_exceeded"}}`, now.Add(900 * time.Second).UTC().Format(http.TimeFormat), OpenAICodexStateFailureRateLimited, 900 * time.Second},
		{"retry seconds", `{"error":{"code":"rate_limited","retry_after_seconds":65.5}}`, "", OpenAICodexStateFailureRateLimited, 65500 * time.Millisecond},
		{"auth wins", `{"error":{"code":"invalid_api_key","type":"rate_limit_error"}}`, "600", OpenAICodexStateFailureAccountError, 10 * time.Minute},
		{"huge reset", `{"error":{"code":"usage_limit_reached","resets_in_seconds":1e30}}`, "", OpenAICodexStateFailureRateLimited, 24 * time.Hour},
		{"past reset", `{"error":{"type":"rate_limit_error","resets_at":100}}`, "", OpenAICodexStateFailureRateLimited, time.Minute},
		{"nonfinite reset", `{"error":{"code":"rate_limit_error","resets_in_seconds":"NaN"}}`, "", OpenAICodexStateFailureRateLimited, time.Minute},
		{"malformed reset", `{"error":{"type":"rate_limit_error","resets_at":{}}}`, "", OpenAICodexStateFailureRateLimited, time.Minute},
		{"overload", `{"error":{"code":"server_is_overloaded"}}`, "600", "", 0},
		{"message only", `{"error":{"message":"rate_limit_exceeded authentication_error"}}`, "600", "", 0},
		{"output text", `{"type":"response.output_text.delta","delta":"usage_limit_reached","code":"rate_limit_exceeded"}`, "600", "", 0},
		{"success", `{"type":"response.completed","response":{"status":"completed"}}`, "600", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, body := range []string{tc.body, "data: " + tc.body + "\n\n"} {
				kind, delay := codexProbeSignal([]byte(body), http.Header{"Retry-After": []string{tc.header}}, now)
				require.Equal(t, tc.failure, kind)
				require.Equal(t, tc.wait, delay)
			}
		})
	}
}

func TestBorrowedRouteStreamLimitsStopWithoutRetry(t *testing.T) {
	for _, kind := range []string{"rate_limit_exceeded", "usage_limit_reached", "authentication_error"} {
		for _, mintFails := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/mint=%t", kind, mintFails), func(t *testing.T) {
				ctx := context.WithValue(t.Context(), borrowInternalKey{}, true)
				req, _ := http.NewRequestWithContext(ctx, http.MethodPost, chatgptCodexURL, nil)
				req.Header.Set("Cookie", "__oailb=borrowed")
				failed := stateProbeReply{status: 200, body: `data: {"type":"response.failed","response":{"error":{"type":"` + kind + `","resets_in_seconds":700,"message":"private credential"}}}` + "\n\n"}
				replies := []stateProbeReply{failed}
				if !mintFails {
					mint := stateProbeMint("target-ticket")
					mint.cookies = nil
					replies = []stateProbeReply{mint, failed}
				}
				up := &stateProbeUpstream{replies: replies}
				out := ProbeOpenAICodexStateRoute(ctx, up, req, "", 11, 1, nil)
				want, _ := codexProbeErrorClass(kind)
				require.Equal(t, want, out.Failure)
				require.Equal(t, OpenAICodexStateInconclusive, out.Verdict)
				require.Equal(t, 700*time.Second, out.RetryAfter)
				require.Zero(t, out.RetryCount)
				require.Len(t, up.calls, len(replies))
				raw, err := json.Marshal(out)
				require.NoError(t, err)
				require.NotContains(t, string(raw), "private credential")
			})
		}
	}
}

type probeSignalBrokenBody struct{ io.Reader }

func (b probeSignalBrokenBody) Close() error { return nil }
func (b probeSignalBrokenBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func TestCodexProbeStreamLimitSurvivesReadFailure(t *testing.T) {
	shot, err := fireOpenAICodexStateShotRequest(t.Context(), http.Header{}, "gpt-6-astra", "", "", func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: probeSignalBrokenBody{strings.NewReader("data: {\"type\":\"error\",\"code\":\"rate_limit_exceeded\"}\n\n")}}, nil
	})
	require.NoError(t, err)
	require.Error(t, shot.streamErr)
	out := &OpenAICodexStateProbeResult{}
	require.False(t, out.shotUsable(t.Context(), "", shot, errors.New("connection closed")))
	require.Equal(t, OpenAICodexStateFailureRateLimited, out.Failure)
}
