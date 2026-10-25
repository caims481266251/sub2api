package service

import (
	"context"
	"fmt"
	"math"
	"strings"
	"unicode"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// Only account-local calling preferences are exposed. Credentials, identity,
// pricing and platform-owned resources are never writable through this API.
type HostingAccountSettings struct {
	Status             *string        `json:"status,omitempty"`
	HostingPoolID      *int64         `json:"hosting_pool_id"`
	Name               *string        `json:"name,omitempty"`
	Concurrency        *int           `json:"concurrency,omitempty"`
	Priority           *int           `json:"priority,omitempty"`
	Notes              *string        `json:"notes"`
	LoadFactor         *int           `json:"load_factor"`
	ExpiresAt          *int64         `json:"expires_at"`
	AutoPauseOnExpired *bool          `json:"auto_pause_on_expired,omitempty"`
	Credentials        map[string]any `json:"credentials"`
	Extra              map[string]any `json:"extra"`
}

var hostingCredentialSettings = map[string]bool{
	"plan_type": true, "model_mapping": true, OpenAIModelMappingModeKey: true, "compact_model_mapping": true,
	"temp_unschedulable_enabled": true, "temp_unschedulable_rules": true,
	accountSchedulingThresholdCredentialKey: true,
}

var hostingBoolSettings = map[string]bool{
	"openai_passthrough": true, "openai_oauth_responses_websockets_v2_enabled": true,
	"openai_oauth_ws_sse_acceleration": true, "openai_ws_force_http": true,
	"openai_responses_flatten_namespaces": true, "codex_cli_only": true,
	"codex_cli_only_allow_app_server": true, "codex_image_generation_bridge": true,
	"openai_excel_bps": true, "openai_excel_bps_omit_unsupported_tools": true,
	ExcelBPSIgnoreEncryptedContentKey: true, "openai_excel_bps_auto_disable_on_403": true,
	ExcelBPSAutoRecoverOn403Key: true,
	"auto_pause_5h_disabled":    true, "auto_pause_7d_disabled": true,
	OpenAIAutoResetCreditEnabledExtraKey: true,
}

var hostingEnumSettings = map[string][]string{
	"openai_oauth_responses_websockets_v2_mode":   {"off", "ctx_pool", "passthrough", "http_bridge"},
	"openai_compact_mode":                         {"auto", "force_on", "force_off"},
	"codex_fingerprint_mode":                      {"off", "device", "session", "full"},
	"codex_image_generation_explicit_tool_policy": {"strip"},
	"openai_excel_bps_config_mode":                {"initial", "defaults"},
}

func hostingExtraSetting(key string) bool {
	if hostingBoolSettings[key] || hostingEnumSettings[key] != nil {
		return true
	}
	switch key {
	case "base_rpm", "auto_pause_5h_threshold", "auto_pause_7d_threshold", "openai_excel_bps_models", ExcelBPS403RecoveryIntervalMinutesKey, AccountExtraUpstreamRequestIDHeader, OpenAIAutoResetCredit5hThresholdExtraKey, OpenAIAutoResetCredit7dThresholdExtraKey:
		return true
	}
	return false
}

func HostedAccountSettings(a *Account) HostingAccountSettings {
	view := HostingAccountSettings{Name: &a.Name, HostingPoolID: a.HostingPoolID, Concurrency: &a.Concurrency, Priority: &a.Priority, Credentials: map[string]any{}, Extra: map[string]any{}}
	view.Status = &a.Status
	view.Notes = a.Notes
	view.LoadFactor = a.LoadFactor
	view.AutoPauseOnExpired = &a.AutoPauseOnExpired
	if a.ExpiresAt != nil {
		v := a.ExpiresAt.Unix()
		view.ExpiresAt = &v
	}
	for key, value := range a.Credentials {
		if hostingCredentialSettings[key] {
			view.Credentials[key] = value
		}
	}
	for key, value := range a.Extra {
		if hostingExtraSetting(key) {
			view.Extra[key] = value
		}
	}
	return view
}

func (s *HostingService) UpdateAccountSettings(ctx context.Context, userID, accountID int64, input HostingAccountSettings) (*Account, error) {
	if _, err := s.requireEnabled(ctx, userID); err != nil {
		return nil, err
	}
	if err := validateHostingAccountSettings(&input); err != nil {
		return nil, err
	}
	if err := s.ResolvePool(ctx, userID, input.HostingPoolID); err != nil {
		return nil, err
	}
	return s.repo.UpdateOwnedSettings(ctx, userID, accountID, input)
}

func invalidHostingSetting(key string) error {
	return infraerrors.BadRequest("HOSTING_ACCOUNT_SETTINGS_INVALID", fmt.Sprintf("托管账号配置无效：%s", key))
}

func validateHostingAccountSettings(input *HostingAccountSettings) error {
	if input.Status != nil && *input.Status != StatusActive && *input.Status != "inactive" && *input.Status != StatusError && *input.Status != StatusDisabled {
		return invalidHostingSetting("status")
	}
	if input.Name != nil {
		name := strings.TrimSpace(*input.Name)
		if name == "" || len([]rune(name)) > 100 {
			return invalidHostingSetting("name")
		}
		input.Name = &name
	}
	if input.Concurrency != nil && (*input.Concurrency < 1 || *input.Concurrency > 1000) {
		return invalidHostingSetting("concurrency")
	}
	if input.Priority != nil && (*input.Priority < 0 || *input.Priority > 1000) {
		return invalidHostingSetting("priority")
	}
	if input.Notes != nil && len([]rune(*input.Notes)) > 2000 {
		return invalidHostingSetting("notes")
	}
	if input.LoadFactor != nil && (*input.LoadFactor < 0 || *input.LoadFactor > 1000) {
		return invalidHostingSetting("load_factor")
	}
	if input.ExpiresAt != nil && (*input.ExpiresAt < 0 || *input.ExpiresAt > 253402300799) {
		return invalidHostingSetting("expires_at")
	}
	for key, value := range input.Credentials {
		if !hostingCredentialSettings[key] {
			return invalidHostingSetting(key)
		}
		if value == nil {
			continue
		}
		if key == "plan_type" {
			plan, ok := value.(string)
			if !ok || len(plan) > 50 || strings.IndexFunc(plan, unicode.IsControl) >= 0 {
				return invalidHostingSetting(key)
			}
		} else if key == OpenAIModelMappingModeKey {
			if err := ValidateModelMappingMode(input.Credentials); err != nil {
				return err
			}
		} else if key == "temp_unschedulable_enabled" {
			if _, ok := value.(bool); !ok {
				return invalidHostingSetting(key)
			}
		} else if key == "temp_unschedulable_rules" {
			if !validHostingTempUnschedulableRules(value) {
				return invalidHostingSetting(key)
			}
		} else if key == accountSchedulingThresholdCredentialKey {
			if !hostingIntegerBetween(value, 1, 100) {
				return invalidHostingSetting(key)
			}
		} else if !validHostingModelMapping(value) {
			return invalidHostingSetting(key)
		}
	}
	for key, value := range input.Extra {
		if !hostingExtraSetting(key) {
			return invalidHostingSetting(key)
		}
		if value == nil {
			continue
		}
		valid := false
		if hostingBoolSettings[key] {
			_, valid = value.(bool)
		} else if options := hostingEnumSettings[key]; options != nil {
			for _, option := range options {
				if value == option {
					valid = true
				}
			}
		} else if key == AccountExtraUpstreamRequestIDHeader {
			if err := ValidateUpstreamRequestIDHeaderExtra(input.Extra); err != nil {
				return err
			}
			if _, exists := input.Extra[key]; !exists {
				input.Extra[key] = nil
			}
			valid = true
		} else if key == "openai_excel_bps_models" {
			models, ok := value.([]any)
			valid = ok && len(models) <= 100
			for _, model := range models {
				name, ok := model.(string)
				valid = valid && ok && validHostingModelName(name, false)
			}
		} else if number, ok := value.(float64); ok && !math.IsNaN(number) && !math.IsInf(number, 0) {
			switch key {
			case "base_rpm":
				valid = number >= 0 && number <= 1000 && math.Trunc(number) == number
			case ExcelBPS403RecoveryIntervalMinutesKey:
				_, valid = excelBPS403RecoveryMinutes(number)
			case OpenAIAutoResetCredit5hThresholdExtraKey, OpenAIAutoResetCredit7dThresholdExtraKey:
				valid = isValidOpenAIAutoResetThreshold(number)
			default:
				valid = number > 0 && number <= 1
			}
		}
		if !valid {
			return invalidHostingSetting(key)
		}
	}
	// Keep the legacy enabled flag in sync with the selected WS mode.
	if raw, exists := input.Extra["openai_oauth_responses_websockets_v2_mode"]; exists {
		if raw == nil {
			input.Extra["openai_oauth_responses_websockets_v2_enabled"] = nil
		} else {
			input.Extra["openai_oauth_responses_websockets_v2_enabled"] = raw != OpenAIWSIngressModeOff
		}
	}
	return nil
}

func validHostingModelMapping(value any) bool {
	mapping, ok := value.(map[string]any)
	if !ok || len(mapping) > 100 {
		return false
	}
	for from, raw := range mapping {
		to, ok := raw.(string)
		if !ok || !validHostingModelName(from, true) || !validHostingModelName(to, false) {
			return false
		}
	}
	return true
}

func validHostingModelName(name string, wildcard bool) bool {
	if name == "" || len(name) > 256 || strings.IndexFunc(name, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return false
	}
	if wildcard {
		name = strings.TrimSuffix(name, "*")
	}
	return !strings.Contains(name, "*")
}

func PrepareHostedAccountSettingsExtra(account *Account, extra map[string]any) map[string]any {
	extra = MergeExcelBPS403Marker(extra, account.Extra)
	return prepareCodexFingerprintExtraForUpdate(account, extra)
}

// Validate calling preferences before refreshing credentials or consuming an OAuth session.
func (s *HostingService) prepareAccountCreationSettings(ctx context.Context, userID int64, input *HostingAccountSettings) error {
	if input == nil {
		return nil
	}
	if err := validateHostingAccountSettings(input); err != nil {
		return err
	}
	return s.ResolvePool(ctx, userID, input.HostingPoolID)
}
