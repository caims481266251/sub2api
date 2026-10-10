//go:build astra_experiment

package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAstraMechanismControls(t *testing.T) {
	for _, borrowed := range []bool{false, true} {
		for _, stable := range []bool{false, true} {
			up := &stateProbeUpstream{replies: []stateProbeReply{
				{status: 503, body: `{"error":{"message":"busy"}}`},
				{status: 200, state: "target-ticket", cookies: []string{"__oailb=route; Path=/", "__cflb=target-lb; Path=/"}, body: stateProbeCompletedStream},
				{status: 200, body: stateProbeCompletedStream},
			}}
			req, _ := http.NewRequest("POST", chatgptCodexURL, nil)
			req.Header.Set("Authorization", "Bearer target-only")
			req.Header.Set("X-Codex-Turn-State", "stale-ticket")
			var cookie *http.Cookie
			if borrowed {
				cookie = &http.Cookie{Name: "__oailb", Value: "route", Path: "/", Domain: "chatgpt.com", MaxAge: 3600, Expires: time.Now().Add(time.Hour), Secure: true, HttpOnly: true, SameSite: http.SameSiteNoneMode}
			}
			out := ProbeAstraExperiment(context.Background(), up, req, "exit", 11, nil, cookie, stable)
			require.Equal(t, OpenAICodexStateHealthy, out.Result.Verdict)
			require.Equal(t, 1, out.Result.RetryCount)
			require.Len(t, up.calls, 3)
			for i, c := range up.calls {
				require.Equal(t, "Bearer target-only", c.header.Get("Authorization"))
				require.Equal(t, "exit", c.proxy)
				require.Equal(t, up.calls[0].body, c.body)
				if i < 2 {
					require.Empty(t, c.header.Get("X-Codex-Turn-State"))
					if borrowed {
						require.Equal(t, "__oailb=route", c.header.Get("Cookie"))
					} else {
						require.Empty(t, c.header.Get("Cookie"))
					}
				}
			}
			require.Equal(t, "target-ticket", up.calls[2].header.Get("X-Codex-Turn-State"))
			require.Contains(t, up.calls[2].header.Get("Cookie"), "__oailb=route")
			require.Contains(t, up.calls[2].header.Get("Cookie"), "__cflb=target-lb")
			if stable {
				require.Equal(t, up.calls[0].header.Get("session_id"), up.calls[2].header.Get("session_id"))
			} else {
				require.NotEqual(t, up.calls[0].header.Get("session_id"), up.calls[2].header.Get("session_id"))
			}
		}
	}
	for _, borrowed := range []bool{false, true} {
		up := &stateProbeUpstream{replies: []stateProbeReply{{status: 200, state: "ticket", cookies: []string{"__oailb=route; Path=/"}, body: stateProbeCompletedStream}, {status: 200, cookies: []string{"__oailb=changed; Path=/"}, body: stateProbeCompletedStream}}}
		req, _ := http.NewRequest("POST", chatgptCodexURL, nil)
		var cookie *http.Cookie
		if borrowed {
			cookie = &http.Cookie{Name: "__oailb", Value: "route"}
		}
		out := ProbeAstraExperiment(context.Background(), up, req, "exit", 11, nil, cookie, false)
		require.Equal(t, "route_changed", out.Result.Failure)
		require.Equal(t, 0, out.Result.RetryCount)
	}
}

func TestAstraMechanismConnectionExperimentRetainsVerdict(t *testing.T) {
	for _, reuse := range []bool{false, true} {
		for _, changed := range []bool{false, true} {
			continuation := stateProbeReply{status: 200, body: stateProbeCompletedStream}
			want := OpenAICodexStateHealthy
			if changed {
				continuation.state = "different-ticket"
				want = OpenAICodexStateDegraded
			}
			mint := stateProbeMint("target-ticket")
			mint.cookies = []string{"__cflb=target-lb; Path=/"}
			up := &stateProbeUpstream{replies: []stateProbeReply{mint, continuation}}
			req, _ := http.NewRequest("POST", chatgptCodexURL, nil)
			req.Header.Set("Authorization", "Bearer target")
			out := ProbeAstraExperimentWithOptions(t.Context(), up, req, "", 11, nil, &http.Cookie{Name: "__oailb", Value: "route"}, AstraExperimentOptions{KeepAlive: reuse})
			require.Equal(t, want, out.Result.Verdict)
			require.Len(t, up.calls, 2)
			for _, call := range up.calls {
				require.Equal(t, !reuse, call.close)
				require.Equal(t, "Bearer target", call.header.Get("Authorization"))
				require.Contains(t, call.header.Get("Cookie"), "__oailb=route")
			}
			require.Equal(t, up.calls[0].body, up.calls[1].body)
			require.Equal(t, "target-ticket", up.calls[1].header.Get(openAICodexTurnStateHeader))
		}
	}
}

func TestAstraMechanismLimitsExcludeSecrets(t *testing.T) {
	h := http.Header{}
	h.Set("Set-Cookie", "secret")
	h.Set(openAICodexTurnStateHeader, "ticket")
	h.Set("x-codex-primary-used-percent", "21.5")
	h.Set("x-codex-secondary-used-percent", "NaN")
	h.Set("x-codex-primary-reset-after-seconds", "raw-secret")
	h.Set("x-codex-secondary-window-minutes", "10080")
	require.Equal(t, map[string]float64{"x-codex-primary-used-percent": 21.5, "x-codex-secondary-window-minutes": 10080}, AstraExperimentLimits(h))
}

func TestAstraMechanismNegativeControlUsesRequestedModel(t *testing.T) {
	up := &stateProbeUpstream{replies: []stateProbeReply{stateProbeMint("ticket"), {status: 200, body: stateProbeCompletedStream}}}
	req, _ := http.NewRequest("POST", chatgptCodexURL, nil)
	out := ProbeAstraExperimentWithOptions(t.Context(), up, req, "", 11, nil, nil, AstraExperimentOptions{Model: "gpt-6-luna"})
	require.Equal(t, "gpt-6-luna", out.Result.Model)
	require.Len(t, up.calls, 2)
	for _, call := range up.calls {
		require.Contains(t, call.body, `"model":"gpt-6-luna"`)
		require.NotContains(t, call.body, `"model":"gpt-6-astra"`)
	}
}

func TestAstraMechanismBalancerContinuity(t *testing.T) {
	for _, tc := range []struct{ response, expected string }{
		{"", "source-lb"},
		{"__cflb=target-lb; Path=/", "target-lb"},
		{"__cflb=deleted; Max-Age=0; Path=/", ""},
	} {
		mint := stateProbeReply{status: 200, state: "target-ticket", body: stateProbeCompletedStream}
		if tc.response != "" {
			mint.cookies = []string{tc.response}
		}
		up := &stateProbeUpstream{replies: []stateProbeReply{mint, {status: 200, body: stateProbeCompletedStream}}}
		req, _ := http.NewRequest("POST", chatgptCodexURL, nil)
		req.Header.Set("Authorization", "Bearer target")
		out := ProbeAstraExperimentWithOptions(t.Context(), up, req, "", 11, nil, &http.Cookie{Name: "__oailb", Value: "source-route", Path: "/"}, AstraExperimentOptions{SeedBalancer: &http.Cookie{Name: "__cflb", Value: "source-lb", Domain: "chatgpt.com", MaxAge: 3600}})
		require.Equal(t, OpenAICodexStateHealthy, out.Result.Verdict)
		require.Len(t, up.calls, 2)
		require.Equal(t, "__oailb=source-route; __cflb=source-lb", up.calls[0].header.Get("Cookie"))
		cont := &http.Request{Header: up.calls[1].header}
		lb, err := cont.Cookie("__cflb")
		if tc.expected == "" {
			require.ErrorIs(t, err, http.ErrNoCookie)
		} else {
			require.NoError(t, err)
			require.Equal(t, tc.expected, lb.Value)
		}
		require.Equal(t, "Bearer target", up.calls[1].header.Get("Authorization"))
		require.Equal(t, "target-ticket", up.calls[1].header.Get(openAICodexTurnStateHeader))
	}
}
