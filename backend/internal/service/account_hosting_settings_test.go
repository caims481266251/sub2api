package service

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestHostingAccountSettingsValidationAndOpenAIReuse(t *testing.T) {
	var input HostingAccountSettings
	require.NoError(t, json.Unmarshal([]byte(`{"name":" My Codex ","concurrency":5,"priority":10,"credentials":{"model_mapping":{"client-model":"gpt-5.4"},"model_mapping_mode":"aliases","compact_model_mapping":{"client-model":"gpt-5.4"}},"extra":{"openai_oauth_responses_websockets_v2_mode":"ctx_pool","openai_compact_mode":"force_on","openai_responses_flatten_namespaces":true,"base_rpm":15,"openai_excel_bps":true,"openai_excel_bps_config_mode":"initial","openai_excel_bps_models":["gpt-5.4"],"openai_excel_bps_403_recovery_interval_minutes":10080}}`), &input))
	require.NoError(t, validateHostingAccountSettings(&input))
	require.Equal(t, "My Codex", *input.Name)
	a := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: input.Credentials, Extra: input.Extra}
	require.True(t, a.IsOpenAIModelMappingAliases())
	require.Equal(t, "gpt-5.4", a.GetMappedModel("client-model"))
	require.True(t, a.IsExcelBPSEnabledForModel("client-model"))
	require.Equal(t, "force_on", a.GetOpenAICompactMode())
	require.True(t, a.IsOpenAIResponsesFlattenNamespacesEnabled())
	require.Equal(t, 15, a.GetBaseRPM())
	require.Equal(t, true, input.Extra["openai_oauth_responses_websockets_v2_enabled"])
	delete(a.Extra, "openai_excel_bps")
	require.Equal(t, "ctx_pool", a.ResolveOpenAIResponsesWebSocketV2Mode("off"))
}

func TestHostingAccountSettingsRejectPrivilegedAndInvalidValues(t *testing.T) {
	for _, raw := range []string{
		`{"credentials":{"access_token":"attack"}}`,
		`{"credentials":{"refresh_token":null}}`,
		`{"credentials":{"plan_type":42}}`,
		`{"status":"forged"}`,
		`{"credentials":{"chatgpt_account_id":"other"}}`,
		`{"extra":{"codex_5h_used_percent":0}}`,
		`{"extra":{"codex_fingerprint_seed":"forged"}}`,
		`{"extra":{"openai_excel_bps_403_target_group_id":9}}`,
		`{"extra":{"openai_prism_browser":true}}`,
		`{"extra":{"openai_prism_browser_models":["gpt-6.1-sol"]}}`,
		`{"extra":{"openai_excel_bps_mihomo":true}}`,
		`{"extra":{"openai_excel_bps_proxy_source":"ip_pool"}}`,
		`{"extra":{"openai_long_context_billing_enabled":false}}`,
		`{"extra":{"openai_passthrough":"true"}}`,
		`{"extra":{"openai_oauth_responses_websockets_v2_mode":"unknown"}}`,
		`{"extra":{"base_rpm":1.5}}`,
		`{"extra":{"auto_pause_5h_threshold":1.1}}`,
		`{"extra":{"openai_excel_bps_models":[42]}}`,
		`{"extra":{"openai_excel_bps_403_recovery_interval_minutes":0}}`,
		`{"credentials":{"model_mapping":{"gpt-*":"gpt-*"}}}`,
		`{"credentials":{"model_mapping_mode":"other"}}`,
		`{"name":" ","concurrency":0}`,
	} {
		t.Run(raw, func(t *testing.T) {
			var input HostingAccountSettings
			require.NoError(t, json.Unmarshal([]byte(raw), &input))
			require.Error(t, validateHostingAccountSettings(&input))
		})
	}
}

func TestHostingAccountSettingsViewDoesNotExposeSecretsOrRuntime(t *testing.T) {
	a := &Account{Credentials: map[string]any{"access_token": "access-secret", "refresh_token": "refresh-secret", "id_token": "id-secret", "chatgpt_account_id": "upstream-id", "model_mapping_mode": "aliases"}, Extra: map[string]any{"codex_fingerprint_seed": "private-seed", "codex_5h_used_percent": 99, "openai_passthrough": true, "openai_prism_browser": true, "openai_prism_browser_models": []string{"gpt-6.1-sol"}, "openai_excel_bps_mihomo": true, "openai_excel_bps_proxy_source": "ip_pool"}}
	raw, err := json.Marshal(HostedAccountSettings(a))
	require.NoError(t, err)
	for _, secret := range []string{"access-secret", "refresh-secret", "id-secret", "upstream-id", "private-seed", "codex_5h_used_percent", "openai_prism_browser", "openai_excel_bps_mihomo", "openai_excel_bps_proxy_source"} {
		require.NotContains(t, string(raw), secret)
	}
	require.Contains(t, string(raw), "model_mapping_mode")
	require.Contains(t, string(raw), "openai_passthrough")
}

func TestHostingAccountSettingsExtendedOpenAIOptions(t *testing.T) {
	var input HostingAccountSettings
	require.NoError(t, json.Unmarshal([]byte(`{"notes":"自用账号","load_factor":8,"expires_at":2000000000,"auto_pause_on_expired":true,"credentials":{"account_scheduling_threshold":85,"temp_unschedulable_enabled":true,"temp_unschedulable_rules":[{"error_code":429,"keywords":["too many requests"],"duration_minutes":30,"description":"限额冷却"}]},"extra":{"openai_excel_bps":true,"upstream_request_id_header":" x-request-id ","auto_reset_credit_enabled":true,"auto_reset_credit_5h_threshold":0,"auto_reset_credit_7d_threshold":0.9}}`), &input))
	require.NoError(t, validateHostingAccountSettings(&input))
	a := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: input.Credentials, Extra: input.Extra}
	require.Len(t, a.GetTempUnschedulableRules(), 1)
	require.Equal(t, 30, a.GetTempUnschedulableRules()[0].DurationMinutes)
	require.Equal(t, "x-request-id", UpstreamRequestIDHeaderName(a))
	require.True(t, ResolveOpenAIAutoResetCreditConfig(a).Enabled)
	require.Zero(t, ResolveOpenAIAutoResetCreditConfig(a).Threshold5h)
	require.Equal(t, .9, ResolveOpenAIAutoResetCreditConfig(a).Threshold7d)
	now := time.Now()
	decision := EvaluateAccountSchedulingThreshold(&Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: input.Credentials, Extra: map[string]any{"codex_5h_used_percent": 90, "codex_5h_reset_at": now.Add(time.Hour).Format(time.RFC3339)}}, nil, now)
	require.True(t, decision.ShouldPause)
	for _, raw := range []string{
		`{"load_factor":-1}`, `{"expires_at":-1}`,
		`{"credentials":{"account_scheduling_threshold":101}}`,
		`{"credentials":{"temp_unschedulable_rules":[{"error_code":429,"keywords":[],"duration_minutes":30}]}}`,
		`{"credentials":{"temp_unschedulable_rules":[{"error_code":429,"keywords":["limit"],"duration_minutes":0}]}}`,
		`{"extra":{"openai_prism_browser_models":["unsupported-model"]}}`,
		`{"extra":{"upstream_request_id_header":"x-id\r\nattack"}}`,
		`{"extra":{"auto_reset_credit_5h_threshold":0.0001}}`,
		`{"extra":{"codex_auto_reset_credit_state":{"status":"success"}}}`,
	} {
		var bad HostingAccountSettings
		require.NoError(t, json.Unmarshal([]byte(raw), &bad))
		require.Error(t, validateHostingAccountSettings(&bad), raw)
	}
}

func TestHostingAccountSettingsClearRequestIDHeader(t *testing.T) {
	for _, raw := range []string{`{"extra":{"upstream_request_id_header":"   "}}`, `{"extra":{"upstream_request_id_header":null}}`} {
		var input HostingAccountSettings
		require.NoError(t, json.Unmarshal([]byte(raw), &input))
		require.NoError(t, validateHostingAccountSettings(&input))
		require.Contains(t, input.Extra, AccountExtraUpstreamRequestIDHeader)
		require.Nil(t, input.Extra[AccountExtraUpstreamRequestIDHeader], "keep a tombstone so merging removes the saved header")
	}
}

func TestHostingAccountSettingsStatusAndPlanReuse(t *testing.T) {
	var input HostingAccountSettings
	require.NoError(t, json.Unmarshal([]byte(`{"status":"inactive","credentials":{"plan_type":"pro"}}`), &input))
	require.NoError(t, validateHostingAccountSettings(&input))
	require.Equal(t, "inactive", *input.Status)
	require.Equal(t, "pro", input.Credentials["plan_type"])
}
