package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type astraTargetProbeStub struct {
	*astraSetupUpstream
	calls   int
	err     error
	request *http.Request
}

func TestAstraGatewaySourceAcquiresWithSelectedModel(t *testing.T) {
	for _, model := range []string{"", "gpt-5.6-sol", "source-alias"} {
		t.Run(model, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Gateway.CodexGatewayPin.SourceAccountIDs = []int64{7}
			if model != "" {
				cfg.Gateway.CodexGatewayPin.SourceAccountModels = map[int64]string{7: model}
			}
			a := stateProbeAccount()
			a.Status = StatusActive
			if model == "source-alias" {
				a.Credentials["model_mapping"] = map[string]any{"source-alias": "gpt-5.6-sol"}
			}
			upstream := &stateProbeUpstream{replies: []stateProbeReply{{status: http.StatusOK, body: "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n" + stateProbeCompletedStream}}}
			svc := &AccountTestService{cfg: cfg, accountRepo: &stateProbeAccountRepo{account: a}, httpUpstream: upstream}
			require.NoError(t, svc.prepareAstraGatewaySource(t.Context(), a.ID))
			require.Len(t, upstream.calls, 1)
			want := cfg.Gateway.CodexGatewayPin.SourceModel(a.ID)
			if model == "source-alias" {
				want = "gpt-5.6-sol"
			}
			require.Equal(t, want, gjson.Get(upstream.calls[0].body, "model").String())
			require.True(t, strings.Contains(upstream.calls[0].body, "Reply with OK only."))
			require.Equal(t, "Bearer tok-7", upstream.calls[0].header.Get("Authorization"))
		})
	}
}

func (p *astraTargetProbeStub) VerifyAstraGatewayTarget(_ context.Context, req *http.Request, _ string, _ int64, _ int) error {
	p.calls++
	p.request = req
	return p.err
}
func TestAstraGatewayVerifyAlwaysUsesStateProbe(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.CodexGatewayPin = config.CodexGatewayPinConfig{Enabled: true, SourceAccountIDs: []int64{8}, TargetAccountIDs: []int64{7}}
	a := stateProbeAccount()
	a.Status = StatusActive
	provider := &astraTargetProbeStub{astraSetupUpstream: &astraSetupUpstream{}}
	svc := &AccountTestService{cfg: cfg, accountRepo: &stateProbeAccountRepo{account: a}, httpUpstream: provider, openaiGatewayService: &OpenAIGatewayService{}}
	for _, kind := range []string{"", "state_probe", "candy"} {
		result, err := svc.TestAstraGateway(t.Context(), "verify", 7, 1, kind)
		require.NoError(t, err)
		require.True(t, result.Success)
		require.Equal(t, "state_probe", result.TestKind)
		require.Empty(t, result.Answer)
		require.Empty(t, result.Expected)
		require.Equal(t, "Bearer tok-7", provider.request.Header.Get("Authorization"))
		require.Equal(t, "acct-7", provider.request.Header.Get("ChatGPT-Account-ID"))
		require.Nil(t, provider.request.Body, "no third question after route validation")
	}
	require.Equal(t, 3, provider.calls)
	provider.err = errors.New("target_probe_degraded")
	result, err := svc.TestAstraGateway(t.Context(), "verify", 7, 1)
	require.NoError(t, err)
	require.False(t, result.Success)
	require.Equal(t, "target_probe_degraded", result.Reason)
}
