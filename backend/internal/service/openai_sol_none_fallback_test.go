package service

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestGPT61SolNoneFallsBackBeforeResponsesForwarding(t *testing.T) {
	for _, oauth := range []bool{true, false} {
		for _, effort := range []string{"none", "low", "medium", "high", "xhigh", "max"} {
			t.Run(fmt.Sprintf("oauth=%v/%s", oauth, effort), func(t *testing.T) {
				body := []byte(fmt.Sprintf(`{"model":"sol","input":"do not alter the word none","store":false,"reasoning":{"effort":%q,"summary":"auto"}}`, effort))
				upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_none_fallback", "gpt-6.1-sol")}
				svc := newOpenAIRejectedFieldTestService(upstream)
				account := rawGPT56ResponsesAPIKeyAccount("sol", "gpt-6.1-sol")
				if oauth {
					account = rawGPT56ResponsesOAuthAccount("sol", "gpt-6.1-sol")
				}
				c, _ := newOpenAIAstraProRetryContext(body)
				result, err := svc.Forward(context.Background(), c, account, body)
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Len(t, upstream.bodies, 1)
				want := effort
				if effort == "none" {
					want = "high"
				}
				require.Equal(t, want, gjson.GetBytes(upstream.lastBody, "reasoning.effort").String())
				require.Equal(t, want, *result.ReasoningEffort)
				require.Equal(t, effort, gjson.GetBytes(body, "reasoning.effort").String(), "caller input remains unchanged")
			})
		}
	}
}

func TestGPT61SolNoneBodyScopeAndWebSocket(t *testing.T) {
	for _, model := range []string{"gpt-6.1-sol", "openai/gpt-6.1-sol", "gpt-6-sol", "gpt-6-luna", "gpt-5.4"} {
		body := []byte(fmt.Sprintf(`{"model":%q,"reasoning":{"effort":"none","summary":"auto"},"temperature":0.5,"input":"none"}`, model))
		out, _, err := normalizeOpenAIResponsesReasoningMode(body, model)
		require.NoError(t, err)
		want := "none"
		if model == "gpt-6.1-sol" || model == "openai/gpt-6.1-sol" {
			want = "high"
			require.False(t, gjson.GetBytes(out, "temperature").Exists())
		}
		require.Equal(t, want, gjson.GetBytes(out, "reasoning.effort").String())
		require.Equal(t, "none", gjson.GetBytes(out, "input").String())
	}
	body := []byte(`{"model":"sol","reasoning":{"effort":"none"},"input":"hello"}`)
	out, _, err := normalizeOpenAIResponsesWebSocketCompatibilityBody(body, rawGPT56ResponsesOAuthAccount("sol", "gpt-6.1-sol"), false)
	require.NoError(t, err)
	require.Equal(t, "high", gjson.GetBytes(out, "reasoning.effort").String())
	unchanged := []byte(`{"model":"gpt-6.1-sol","input":"none"}`)
	out, changed, err := normalizeGPT61SolReasoningBody(unchanged, "gpt-6.1-sol")
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, unchanged, out)
}

func TestGPT61SolNoneCompatibilityForwarding(t *testing.T) {
	for _, endpoint := range []string{"chat", "messages", "messages-disabled"} {
		t.Run(endpoint, func(t *testing.T) {
			body := []byte(`{"model":"sol","max_tokens":100,"messages":[{"role":"user","content":"hello"}],"reasoning_effort":"none","output_config":{"effort":"none"},"stream":false}`)
			if endpoint == "messages-disabled" {
				body = []byte(`{"model":"sol","max_tokens":100,"messages":[{"role":"user","content":"hello"}],"thinking":{"type":"disabled"},"output_config":{"effort":"max"},"stream":false}`)
			}
			upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_compat_none", "gpt-6.1-sol")}
			svc := newOpenAIRejectedFieldTestService(upstream)
			c, _ := newOpenAIAstraProRetryContext(body)
			account := rawGPT56ResponsesOAuthAccount("sol", "gpt-6.1-sol")
			var result *OpenAIForwardResult
			var err error
			if endpoint == "chat" {
				result, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
			} else {
				result, err = svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")
			}
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Len(t, upstream.bodies, 1)
			require.Equal(t, "high", gjson.GetBytes(upstream.lastBody, "reasoning.effort").String())
			require.Equal(t, "high", *result.ReasoningEffort)
		})
	}
}
