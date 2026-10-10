package config

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestCodexGatewayPinConfigValidation(t *testing.T) {
	require.NoError(t, (CodexGatewayPinConfig{}).Validate())
	for _, c := range []CodexGatewayPinConfig{
		{Enabled: true},
		{Enabled: true, SourceAccountIDs: []int64{299}},
		{Enabled: true, SourceAccountIDs: []int64{299}, TargetAccountIDs: []int64{299}},
		{Enabled: true, SourceAccountIDs: []int64{-1}, TargetAccountIDs: []int64{300}},
	} {
		require.Error(t, c.Validate())
	}
	require.NoError(t, (CodexGatewayPinConfig{Enabled: true, SourceAccountIDs: []int64{299}, TargetAccountIDs: []int64{300}}).Validate())
}

func TestCodexWSAnchorConfigValidation(t *testing.T) {
	require.NoError(t, (CodexWSAnchorConfig{}).Validate())
	require.Error(t, (CodexWSAnchorConfig{Enabled: true}).Validate())
	require.Error(t, (CodexWSAnchorConfig{Enabled: true, AccountIDs: []int64{300, 300}}).Validate())
	require.NoError(t, (CodexWSAnchorConfig{Enabled: true, AccountIDs: []int64{299, 300}}).Validate())
}

func TestCodexGatewaySourceModels(t *testing.T) {
	c := CodexGatewayPinConfig{SourceAccountIDs: []int64{299, 298}, SourceAccountModels: map[int64]string{299: "gpt-5.6-sol"}}
	require.NoError(t, c.Validate())
	require.Equal(t, "gpt-5.6-sol", c.SourceModel(299))
	require.Equal(t, "gpt-6-astra", c.SourceModel(298))
	resolved, err := ResolveAstraDependencies(AstraRoutingSettings{CookiePool: c})
	require.NoError(t, err)
	resolved.CookiePool.SourceAccountModels[299] = "gpt-6-sol"
	require.Equal(t, "gpt-5.6-sol", c.SourceModel(299), "resolved settings must not share the model map")
	for _, model := range []string{"", "gpt-*", " gpt-5.6-sol", "gpt-5.6\nsol", "gpt-5.6\u00a0sol", "gpt-5.6\x00sol", string(make([]byte, 257))} {
		c.SourceAccountModels[299] = model
		require.ErrorContains(t, c.Validate(), "astra_source_model_invalid")
	}
	c.SourceAccountModels = map[int64]string{300: "gpt-5.6-sol"}
	require.ErrorContains(t, c.Validate(), "astra_source_model_invalid", "only selected sources can have a model")
}
