package service

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func candyQualityReply(answer string) stateProbeReply {
	data, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "content": []any{map[string]string{"type": "output_text", "text": answer}}}}}})
	return stateProbeReply{status: 200, body: "data: " + string(data) + "\n\n"}
}

func TestBorrowQualityOriginalPromptAndCompletion(t *testing.T) {
	body := borrowRouteQualityBody(*DefaultBorrowRouteQualityConfig())
	require.Equal(t, openai.DefaultInstructions, gjson.GetBytes(body, "instructions").String())
	require.Equal(t, intelligenceTestPrompt(&PelicanTestConfig{Prompt: CandyPrompt, QuestionKind: "candy"}), gjson.GetBytes(body, "input.0.content.0.text").String())
	require.Equal(t, "gpt-6-astra", gjson.GetBytes(body, "model").String())
	require.Equal(t, "medium", gjson.GetBytes(body, "reasoning.effort").String())
	for _, tc := range []struct{ data, reason string }{
		{candyQualityReply("21").body, ""}, {candyQualityReply("29").body, "route_quality_failed"},
		{candyQualityReply("121").body, "route_quality_failed"}, {candyQualityReply("21 or 29").body, "route_quality_failed"},
		{"data: {\"type\":\"response.output_text.delta\",\"delta\":\"21\"}\n\ndata: [DONE]\n\n", "route_quality_incomplete"},
		{"data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"rate_limit_exceeded\"}}}\n\n", "route_rate_limited"},
	} {
		_, reason := borrowQualityAnswer([]byte(tc.data))
		require.Equal(t, tc.reason, reason)
	}
}

func TestBorrowQualityGateForNewAndExistingRoutes(t *testing.T) {
	for _, answer := range []string{"21", "29", ""} {
		s, base := borrowFixture()
		r := &routeReprobeRepo{continuePolicyRepo: &continuePolicyRepo{borrowTestRepo: base}}
		s.repo = r
		u := &routeReprobeUpstream{stateProbeUpstream: &stateProbeUpstream{replies: []stateProbeReply{candyQualityReply(answer)}}}
		s.owner.httpUpstream = u
		job := &BorrowRouteHealthJob{Route: base.route, Token: "quality-job", QualityConfig: DefaultBorrowRouteQualityConfig()}
		s.runRouteHealth(t.Context(), r, job)
		require.Equal(t, answer == "21", r.result.Success)
		require.NotNil(t, r.result.Quality)
		require.Zero(t, r.active)
		require.Len(t, u.calls, 1)
		call := u.calls[0]
		require.Equal(t, "Bearer target-secret", call.header.Get("Authorization"))
		require.Contains(t, call.header.Get("Cookie"), "__oailb=donor-cookie")
		require.Empty(t, call.header.Get(openAICodexTurnStateHeader))
		require.Equal(t, "quality", r.requests[0].Phase)
	}
}

func TestBorrowQualityFailureNeverPasses(t *testing.T) {
	for _, reply := range []stateProbeReply{{status: 429, body: "limited"}, {status: 503, body: "busy"}, {status: 200, body: "data: [DONE]\n\n"}, {status: 200, body: candyQualityReply("21").body, cookies: []string{"__oailb=changed"}}} {
		s, base := borrowFixture()
		r := &routeReprobeRepo{continuePolicyRepo: &continuePolicyRepo{borrowTestRepo: base}}
		s.repo = r
		s.owner.httpUpstream = &routeReprobeUpstream{stateProbeUpstream: &stateProbeUpstream{replies: []stateProbeReply{reply}}}
		s.runRouteHealth(t.Context(), r, &BorrowRouteHealthJob{Route: base.route, Token: "quality"})
		require.False(t, r.result.Success)
		require.NotEmpty(t, r.result.Reason)
	}
	ctx, stop := context.WithCancel(t.Context())
	stop()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.com", nil)
	result, _, _ := probeBorrowRouteQuality(req, *DefaultBorrowRouteQualityConfig(), func(*http.Request) (*http.Response, error) { return nil, context.Canceled })
	require.False(t, result.Passed(*DefaultBorrowRouteQualityConfig()))
}
