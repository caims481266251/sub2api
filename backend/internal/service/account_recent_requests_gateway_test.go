//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAccountRecentRequestsGatewayStreamingOutcomes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, passthrough := range []bool{false, true} {
		for _, tc := range []struct{ name, wire, outcome string }{
			{"completed", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" + "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ok\",\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}}\n\n", "success"},
			{"empty_completed_rejected", "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_empty\",\"status\":\"completed\"}}\n\n", "failure"},
			{"failed", "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"failure\"}}}\n\n", "failure"},
		} {
			t.Run(tc.name+map[bool]string{true: "_passthrough", false: "_native"}[passthrough], func(t *testing.T) {
				cache := &recentRequestsTestCache{}
				upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(tc.wire))}}
				svc := newOpenAIImageGenerationControlTestService(upstream)
				svc.SetRecentRequests(cache)
				c, _ := newOpenAIImageGenerationControlTestContext(true, "codex_cli_rs/0.144.1")
				account := newOpenAIImageGenerationControlTestAccount()
				account.Extra = map[string]any{"openai_passthrough": passthrough}
				_, _ = svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-5.6-sol","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`))
				require.Len(t, cache.records, 1)
				require.Equal(t, tc.outcome, cache.records[0].Outcome)
				if tc.outcome == "success" {
					require.NotNil(t, cache.records[0].FirstTokenMs)
				} else {
					require.Nil(t, cache.records[0].FirstTokenMs, "empty terminal/error is not first output")
				}
			})
		}
	}
}

func TestAccountRecentRequestsRawChatRoleOnlyDoesNotSetTTFT(t *testing.T) {
	cache := &recentRequestsTestCache{}
	wire := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n" + "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n" + "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" + "data: [DONE]\n\n"
	upstream := &httpUpstreamRecorder{}
	svc := newOpenAIImageGenerationControlTestService(upstream)
	c, _ := newOpenAIImageGenerationControlTestContext(true, "codex_cli_rs/0.144.1")
	account := newOpenAIImageGenerationControlTestAccount()
	req, a := beginRecentHTTPRequest(c.Request, cache, account)
	resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(wire))}
	a.response(req, resp, nil)
	_, err := svc.streamRawChatCompletions(c, resp, account, "gpt-test", "gpt-test", "gpt-test", nil, nil, time.Now(), 1)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Len(t, cache.records, 1)
	require.Equal(t, "success", cache.records[0].Outcome)
	require.NotNil(t, cache.records[0].FirstTokenMs)
}

func TestAccountRecentRequestsTransportAttemptsAndLocalAdmission(t *testing.T) {
	cache := &recentRequestsTestCache{}
	rpm := &openAIRPMTestCache{counts: map[int64]int{}}
	account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "test-account"}, Extra: map[string]any{"base_rpm": 2}}
	calls := 0
	svc := &OpenAIGatewayService{recentRequests: cache, rpmCache: rpm, httpUpstream: &codexTicketFuncUpstream{do: func(req *http.Request) (*http.Response, error) {
		calls++
		a := recentAttemptFrom(req.Context())
		require.NotNil(t, a)
		if calls == 1 {
			a.started = time.Now().Add(-40 * time.Second)
			return nil, errors.New("dial failed")
		}
		observeRecentProtocolEvent(req.Context(), []byte(`{"type":"response.output_text.delta","delta":"hello"}`), "response.output_text.delta", OpenAITTFTModeSemantic)
		observeRecentProtocolEvent(req.Context(), []byte(`{"type":"response.completed","response":{"status":"completed"}}`), "response.completed", OpenAITTFTModeSemantic)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: http.NoBody}, nil
	}}}
	for i := 0; i < 3; i++ {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://example.invalid/v1/responses", strings.NewReader(`{"model":"gpt-6.1-sol"}`))
		require.NoError(t, err)
		resp, err := svc.doOpenAIProxyAttempt(req, account, runtimeProxyEgress{proxyID: 0})
		switch i {
		case 0:
			require.Error(t, err)
		case 1:
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
		case 2:
			require.ErrorIs(t, err, ErrOpenAIRPMExhausted)
		}
	}
	require.Equal(t, 2, calls, "local RPM rejection never reaches transport")
	require.Len(t, cache.records, 2)
	require.Equal(t, "failure", cache.records[0].Outcome)
	require.GreaterOrEqual(t, cache.records[0].DurationMs, int64(40_000))
	require.Equal(t, "success", cache.records[1].Outcome)
	require.Less(t, *cache.records[1].FirstTokenMs, int64(10_000), "retry does not inherit earlier 40 seconds")
	require.NotEqual(t, cache.records[0].AttemptID, cache.records[1].AttemptID)
}

func TestAccountRecentRequestsBPSObservesBeforeBridgeClosesBody(t *testing.T) {
	for _, terminal := range []bool{true, false} {
		t.Run(map[bool]string{true: "completed", false: "truncated"}[terminal], func(t *testing.T) {
			cache := &recentRequestsTestCache{}
			wire := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"
			if terminal {
				wire += "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_bps\",\"status\":\"completed\",\"output\":[]}}\n\n"
			}
			_, bridge, err := basispoints.Prepare([]byte(`{"model":"gpt-6.1-sol","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`), "test", nil)
			require.NoError(t, err)
			req, a := beginRecentHTTPRequest(httptest.NewRequest(http.MethodPost, "/", nil), cache, recentRequestsTestAccount())
			resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(wire))}
			a.response(req, resp, nil)
			reader := &recentOpenAIEventReader{ReadCloser: resp.Body, ctx: recentRequestResponseContext(context.Background(), resp), mode: OpenAITTFTModeSemantic}
			converted := bridge.Stream(reader)
			_, err = io.ReadAll(converted)
			if terminal {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			_ = converted.Close()
			require.Len(t, cache.records, 1)
			require.Equal(t, map[bool]string{true: "success", false: "failure"}[terminal], cache.records[0].Outcome)
			require.NotNil(t, cache.records[0].FirstTokenMs)
		})
	}
}
