//go:build astra_experiment

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAstraBusinessControlUsesFreshTicketAndKeepsAnswerSeparate(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		_, repo := borrowFixture()
		second := ""
		if replacement {
			second = "different-private-ticket"
		}
		body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"29\"}\n\n" + stateProbeCompletedStream
		up := &stateProbeUpstream{replies: []stateProbeReply{{status: 200, state: "fresh-private-ticket", body: body}, {status: 200, state: second, body: body}}}
		route := &BorrowRoute{ID: "route-control", TargetID: 11, SourceID: 1, ExpiresAt: time.Now().Add(time.Minute), Secret: BorrowRouteSecret{Cookie: http.Cookie{Name: "__oailb", Value: "private-route"}, Headers: http.Header{"Authorization": []string{"Bearer private-token"}, "X-Codex-Turn-State": []string{"old-private-ticket"}}, Proxy: "private-proxy"}}
		out := ProbeAstraBusinessControl(t.Context(), repo, up, route, "control-a", "candy prompt without answer", func(context.Context) error { return nil })
		require.Len(t, up.calls, 2)
		require.Empty(t, up.calls[0].header.Get(openAICodexTurnStateHeader))
		require.Equal(t, "fresh-private-ticket", up.calls[1].header.Get(openAICodexTurnStateHeader))
		require.NotEqual(t, up.calls[0].header.Get("session_id"), up.calls[1].header.Get("session_id"))
		require.Equal(t, up.calls[0].body, up.calls[1].body)
		require.Contains(t, up.calls[0].body, "candy prompt without answer")
		require.Equal(t, "29", out.Shots[0].Text)
		require.Equal(t, replacement, out.Result.NewTicket)
		want := OpenAICodexStateHealthy
		if replacement {
			want = OpenAICodexStateDegraded
		}
		require.Equal(t, want, out.Result.Verdict, "wrong semantic answer must not change protocol verdict")
		serialized, err := json.Marshal(out)
		require.NoError(t, err)
		for _, secret := range []string{"private-token", "private-route", "private-ticket", "private-proxy"} {
			require.NotContains(t, string(serialized), secret)
		}
	}
}

func TestAstraBusinessControlHonorsGuardWithoutDispatchOrRetry(t *testing.T) {
	_, repo := borrowFixture()
	up := &stateProbeUpstream{}
	route := &BorrowRoute{TargetID: 11, Secret: BorrowRouteSecret{Cookie: http.Cookie{Name: "__oailb", Value: "route"}}}
	out := ProbeAstraBusinessControl(t.Context(), repo, up, route, "guarded", "prompt", func(context.Context) error { return errors.New("blocked") })
	require.Empty(t, up.calls)
	require.Equal(t, OpenAICodexStateInconclusive, out.Result.Verdict)
	require.Len(t, out.Shots, 1)
	require.Zero(t, out.Result.RetryCount)
}

func TestAstraExitControlChallengesEachPassExactlyOnceOnItsOwnTicket(t *testing.T) {
	for _, reject := range []bool{false, true} {
		_, repo := borrowFixture()
		second := ""
		if reject {
			second = "replacement"
		}
		body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"21\"}\n\n" + stateProbeCompletedStream
		up := &stateProbeUpstream{replies: []stateProbeReply{{status: 200, state: "new-target-ticket", body: stateProbeCompletedStream}, {status: 200, state: second, body: stateProbeCompletedStream}, {status: 200, body: body}}}
		route := &BorrowRoute{TargetID: 11, SourceID: 2, CookieHash: "candidate-hash", ExpiresAt: time.Now().Add(time.Minute), Secret: BorrowRouteSecret{Cookie: http.Cookie{Name: "__oailb", Value: "private-cookie"}, Headers: http.Header{"Authorization": []string{"Bearer private-target-token"}}, Proxy: "fixed-private-proxy"}}
		out := ProbeAstraExitControl(t.Context(), repo, up, route, "exit-arm", "candy without answer", func(context.Context) error { return nil })
		if reject {
			require.Len(t, up.calls, 2)
			require.Nil(t, out.Business)
		} else {
			require.Len(t, up.calls, 3)
			require.Equal(t, "new-target-ticket", up.calls[2].header.Get(openAICodexTurnStateHeader))
			require.NotEqual(t, up.calls[1].header.Get("session_id"), up.calls[2].header.Get("session_id"))
			require.NotContains(t, up.calls[0].body, "candy without answer")
			require.Contains(t, up.calls[2].body, "candy without answer")
			require.True(t, out.Business.Complete)
			require.False(t, out.Business.NewTicket)
			require.Equal(t, "candy", out.Shots[2].Phase)
			require.Equal(t, "21", out.Shots[2].Text)
		}
		raw, err := json.Marshal(out)
		require.NoError(t, err)
		require.NotContains(t, string(raw), "private-cookie")
		require.NotContains(t, string(raw), "private-target-token")
		require.NotContains(t, string(raw), "fixed-private-proxy")
	}
}
