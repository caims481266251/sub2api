package service

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestAccountExecutionModeDefaultsAndValidation(t *testing.T) {
	for _, tc := range []struct {
		input, want string
		invalid     bool
	}{{"", "normal", false}, {"normal", "normal", false}, {"vm_kernel", "vm_kernel", false}, {"vm", "", true}, {" normal", "", true}} {
		got, err := NormalizeAccountExecutionMode(tc.input)
		if (err != nil) != tc.invalid || got != tc.want {
			t.Fatalf("mode %q: got %q, err=%v", tc.input, got, err)
		}
	}
	a, err := buildAccountForCreate(&CreateAccountInput{Platform: PlatformAnthropic, Type: AccountTypeOAuth}, map[string]any{})
	if err != nil || a.ExecutionMode != AccountExecutionModeNormal {
		t.Fatalf("legacy create: account=%+v err=%v", a, err)
	}
}

func TestAccountExecutionModeUpdatePreservesAndRejectsSwitch(t *testing.T) {
	a := &Account{ExecutionMode: AccountExecutionModeVMKernel}
	if err := ValidateAccountExecutionModeUpdate(a, nil); err != nil || !a.IsVMKernel() {
		t.Fatal("omitted update changed VM mode", err)
	}
	normal, vm := "normal", "vm_kernel"
	if err := ValidateAccountExecutionModeUpdate(a, &normal); err == nil {
		t.Fatal("allowed VM to normal")
	}
	if err := ValidateAccountExecutionModeUpdate(&Account{}, &vm); err == nil {
		t.Fatal("allowed normal to VM")
	}
	if err := ValidateAccountExecutionModeUpdate(a, &vm); err != nil {
		t.Fatal(err)
	}
}

func TestValidateVMKernelAccountCombinations(t *testing.T) {
	for _, platform := range []string{PlatformAnthropic, PlatformOpenAI} {
		a := &Account{ExecutionMode: AccountExecutionModeVMKernel, Platform: platform, Type: AccountTypeOAuth}
		if err := ValidateVMKernelAccount(a); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name   string
		change func(*Account)
	}{
		{"platform", func(a *Account) { a.Platform = PlatformGemini }},
		{"apikey", func(a *Account) { a.Type = AccountTypeAPIKey }},
		{"custom endpoint", func(a *Account) { a.Credentials = map[string]any{"base_url": "https://example.invalid"} }},
		{"bps pending", func(a *Account) {
			a.Extra = map[string]any{"openai_excel_bps": true, "openai_excel_bps_authorization_pending": true}
		}},
		{"prism", func(a *Account) { a.Extra = map[string]any{"openai_prism_browser_models": []any{"model"}} }},
		{"plugin", func(a *Account) { a.Extra = map[string]any{"plugin_id": "x"} }},
		{"shadow", func(a *Account) { id := int64(1); a.ParentAccountID = &id }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Account{ExecutionMode: AccountExecutionModeVMKernel, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
			tc.change(a)
			if err := ValidateVMKernelAccount(a); err == nil {
				t.Fatal("unsupported VM accepted")
			}
			a.ExecutionMode = "normal"
			if err := ValidateVMKernelAccount(a); err != nil {
				t.Fatal("normal behavior changed", err)
			}
		})
	}
}

func TestVMKernelCreateRequiresEnabledDeploymentAndSupportsTwoFA(t *testing.T) {
	svc := &adminServiceImpl{}
	proxyID := int64(7)
	input := &CreateAccountInput{Name: "VM OAuth", Platform: PlatformOpenAI, Type: AccountTypeOAuth, ExecutionMode: AccountExecutionModeVMKernel,
		ProxyID: &proxyID, Concurrency: 12, InitialOpenAITwoFA: true, SkipDefaultGroupBind: true}
	_, err := svc.CreateAccount(t.Context(), input)
	require.Error(t, err)
	svc.cfg = &config.Config{VMKernel: config.VMKernelConfig{Enabled: true}}
	svc.accountRepo = &upstreamBillingProbeAccountRepo{}
	account, err := svc.CreateAccount(t.Context(), input)
	require.NoError(t, err)
	require.Equal(t, 12, account.Concurrency)
	require.Equal(t, AccountExecutionModeVMKernel, account.ExecutionMode)
}

func TestVMKernelCreatePreservesOriginalAutoConfigurationAndAdmission(t *testing.T) {
	for _, admission := range []bool{false, true} {
		for _, mode := range []string{AccountExecutionModeNormal, AccountExecutionModeVMKernel} {
			t.Run(mode+"/admission="+map[bool]string{false: "off", true: "on"}[admission], func(t *testing.T) {
				policy := DefaultOAuthAutoConfig()
				policy.Enabled, policy.Concurrency, policy.GroupIDs = true, 9, []int64{5}
				policy.QualityRule = initialQualityRuleFixture()
				policy.OpenAITwoFAAdmission.Enabled = admission
				policy.OpenAITwoFAAdmission.IsolationGroupID = 5
				raw, err := json.Marshal(policy)
				require.NoError(t, err)
				repo := &autoConfigAccountRepo{}
				svc := &adminServiceImpl{cfg: &config.Config{VMKernel: config.VMKernelConfig{Enabled: true}}, accountRepo: repo,
					settingService: NewSettingService(&accountOpsSettingsStub{raw: string(raw)}, nil),
					groupRepo:      autoConfigGroups{group: &Group{ID: 5, Platform: PlatformOpenAI, Status: StatusActive}}}
				proxyID := int64(7)
				account, err := svc.CreateAccount(t.Context(), &CreateAccountInput{Name: "OAuth", Platform: PlatformOpenAI, Type: AccountTypeOAuth,
					ExecutionMode: mode, ProxyID: &proxyID, Concurrency: 12, InitialOpenAITwoFA: true, SkipDefaultGroupBind: true, SkipMixedChannelCheck: true})
				require.NoError(t, err)
				require.NotNil(t, account.InitialQualityPlan)
				require.Equal(t, !admission, account.Schedulable)
				if admission {
					require.NotNil(t, account.InitialAdmissionPlan)
					require.Equal(t, 1, account.Concurrency, "the original admission policy remains authoritative")
				} else {
					require.Nil(t, account.InitialAdmissionPlan)
					require.Equal(t, 9, account.Concurrency, "VM must preserve the original automatic configuration")
				}
			})
		}
	}
}

func TestVMKernelRejectsHostManagementProbes(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, ExecutionMode: AccountExecutionModeVMKernel}
	if reason := openAICodexStateProbeUnsupportedReason(account, "gpt-6-sol", false); reason == "" {
		t.Fatal("VM accepted host state probe")
	}
	if QualityBPSEligible(account) {
		t.Fatal("quality plan may enable host BPS for VM")
	}
}

func TestVMKernelCreateRequiresProxyAndPreservesConcurrency(t *testing.T) {
	input := &CreateAccountInput{Platform: PlatformOpenAI, Type: AccountTypeOAuth, ExecutionMode: AccountExecutionModeVMKernel, Concurrency: 10}
	if _, err := buildAccountForCreate(input, map[string]any{}); err == nil {
		t.Fatal("VM accepted missing proxy")
	}
	proxyID := int64(7)
	input.ProxyID = &proxyID
	account, err := buildAccountForCreate(input, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if account.Concurrency != 10 {
		t.Fatalf("VM concurrency=%d", account.Concurrency)
	}
}

func TestVMKernelAccountConcurrencyUpdatesUseOriginalPaths(t *testing.T) {
	proxyID := int64(7)
	account := &Account{ID: 72, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ExecutionMode: AccountExecutionModeVMKernel, ProxyID: &proxyID, Concurrency: 8, Status: StatusActive}
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	svc := &adminServiceImpl{accountRepo: repo}
	concurrency := 24
	updated, err := svc.UpdateAccount(t.Context(), account.ID, &UpdateAccountInput{Concurrency: &concurrency})
	require.NoError(t, err)
	require.Equal(t, 24, updated.Concurrency)
	updated, err = svc.UpdateAccount(t.Context(), account.ID, &UpdateAccountInput{Name: "renamed"})
	require.NoError(t, err)
	require.Equal(t, 24, updated.Concurrency, "unrelated edits must not reset concurrency")
	concurrency = 32
	result, err := svc.BulkUpdateAccounts(t.Context(), &BulkUpdateAccountsInput{AccountIDs: []int64{account.ID}, Concurrency: &concurrency})
	require.NoError(t, err)
	require.Equal(t, 1, result.Success)
	require.Len(t, repo.bulkUpdates, 1)
	require.Equal(t, 32, *repo.bulkUpdates[0].Concurrency)
}

type vmAccountDeletionRepo struct {
	*upstreamBillingProbeAdminRepo
	deleteErr error
	deleted   []int64
}

func (r *vmAccountDeletionRepo) Delete(_ context.Context, id int64) error {
	if r.deleteErr != nil {
		return r.deleteErr
	}
	r.deleted = append(r.deleted, id)
	delete(r.accounts, id)
	return nil
}
func TestVMKernelAccountDeletionDoesNotDependOnHelperAvailability(t *testing.T) {
	for _, fail := range []bool{false, true} {
		account := &Account{ID: 654321, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ExecutionMode: AccountExecutionModeVMKernel}
		repo := &vmAccountDeletionRepo{upstreamBillingProbeAdminRepo: &upstreamBillingProbeAdminRepo{
			upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{account.ID: account}}}}
		if fail {
			repo.deleteErr = errors.New("database unavailable")
		}
		err := (&adminServiceImpl{accountRepo: repo}).DeleteAccount(t.Context(), account.ID)
		if fail {
			require.ErrorIs(t, err, repo.deleteErr)
			require.Empty(t, repo.deleted)
			require.Contains(t, repo.accounts, account.ID)
		} else {
			require.NoError(t, err)
			require.Equal(t, []int64{account.ID}, repo.deleted)
			require.NotContains(t, repo.accounts, account.ID)
		}
	}
}

// SOCKS is an egress choice, not another account execution mode. Keep the
// original OAuth account and concurrency semantics for both SOCKS schemes.
func TestVMKernelAccountsAcceptOriginalProxyProtocols(t *testing.T) {
	for _, platform := range []string{PlatformOpenAI, PlatformAnthropic} {
		for _, protocol := range []string{"http", "https", "socks5", "socks5h"} {
			t.Run(platform+"/"+protocol, func(t *testing.T) {
				proxyID := int64(91)
				account := &Account{
					ID: 92, Platform: platform, Type: AccountTypeOAuth,
					ExecutionMode: AccountExecutionModeVMKernel,
					Status:        StatusActive, Schedulable: true, Concurrency: 30,
					ProxyID: &proxyID,
					Proxy:   &Proxy{ID: proxyID, Protocol: protocol, Host: "proxy.example.invalid", Port: 1080, Status: StatusActive},
				}
				require.NoError(t, ValidateVMKernelAccount(account))
				require.True(t, vmAccountRequestEligible(account, false))
				require.Equal(t, 30, account.Concurrency)
				require.Equal(t, protocol, account.Proxy.Protocol, "validation must not rewrite saved proxy configuration")
				require.Equal(t, protocol+"://proxy.example.invalid:1080", account.Proxy.URL())
				// Proxy expiry/status is still enforced by the original eligibility
				// path regardless of HTTP or SOCKS, with no direct fallback.
				account.Proxy.Status = StatusDisabled
				require.False(t, vmAccountRequestEligible(account, false))
			})
		}
	}
}
