//go:build astra_experiment

package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type exitSourceRepo struct{ TicketBorrowRepository }

func (r *exitSourceRepo) ObserveCookie(_ context.Context, _ string, ttl time.Duration) (time.Time, error) {
	return time.Now().Add(ttl), nil
}

func TestAstraExitAcquisitionUsesProductionMinimalSourceAndOmitsSecrets(t *testing.T) {
	_, repo := borrowFixture()
	cookie := "x." + base64.RawURLEncoding.EncodeToString([]byte(`{"host":"chat.gateway.unified-9.api.openai.com"}`)) + ".private-cookie"
	up := &stateProbeUpstream{replies: []stateProbeReply{{status: 200, body: stateProbeCompletedStream, cookies: []string{"__oailb=" + cookie + "; Max-Age=230"}}}}
	headers := http.Header{"Authorization": []string{"Bearer source-private-token"}, "Cookie": []string{"__oailb=old-cookie"}, "X-Codex-Turn-State": []string{"old-ticket"}}
	out := AcquireAstraExitCandidate(t.Context(), &exitSourceRepo{repo}, up, headers, "private-proxy", 12, 230, func(context.Context) error { return nil })
	require.NotNil(t, out.Cookie)
	require.True(t, out.Complete)
	require.Equal(t, "chat.gateway.unified-9.api.openai.com", out.Gateway)
	require.Len(t, up.calls, 1)
	require.Contains(t, up.calls[0].body, `"model":"gpt-6-luna"`)
	require.Contains(t, up.calls[0].body, `"instructions":"Reply with OK."`)
	require.Equal(t, "Bearer source-private-token", up.calls[0].header.Get("Authorization"))
	require.Empty(t, up.calls[0].header.Get("Cookie"))
	require.Empty(t, up.calls[0].header.Get(openAICodexTurnStateHeader))
	require.NotEmpty(t, up.calls[0].header.Get("X-Codex-Installation-ID"))
	raw, err := json.Marshal(out)
	require.NoError(t, err)
	for _, secret := range []string{cookie, "source-private-token", "private-proxy", "old-ticket"} {
		require.NotContains(t, string(raw), secret)
	}
}

func TestAstraExitAcquisitionGuardPreventsSending(t *testing.T) {
	_, repo := borrowFixture()
	up := &stateProbeUpstream{}
	out := AcquireAstraExitCandidate(t.Context(), repo, up, make(http.Header), "", 12, 230, func(context.Context) error { return errors.New("cooling") })
	require.Nil(t, out.Cookie)
	require.False(t, out.Complete)
	require.Empty(t, up.calls)
}
