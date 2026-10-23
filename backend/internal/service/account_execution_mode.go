package service

import (
	"context"
	"reflect"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	AccountExecutionModeNormal       = "normal"
	AccountExecutionModeDockerKernel = "docker_kernel"
	// Internal name retained to reuse the existing kernel business pipeline.
	AccountExecutionModeVMKernel = AccountExecutionModeDockerKernel
)

// EffectiveExecutionMode preserves compatibility with pre-migration snapshots.
// Runtime admission must still re-read authoritative state before dispatch.
func (a *Account) EffectiveExecutionMode() string {
	if a == nil || a.ExecutionMode == "" {
		return AccountExecutionModeNormal
	}
	return a.ExecutionMode
}
func (a *Account) IsVMKernel() bool {
	return a != nil && (a.ExecutionMode == AccountExecutionModeVMKernel || a.ExecutionMode == "vm_kernel")
}

func NormalizeAccountExecutionMode(mode string) (string, error) {
	if mode == "" {
		return AccountExecutionModeNormal, nil
	}
	if mode != AccountExecutionModeNormal && mode != AccountExecutionModeVMKernel {
		return "", infraerrors.BadRequest("INVALID_EXECUTION_MODE", "execution_mode must be normal or docker_kernel")
	}
	return mode, nil
}

func ValidateAccountExecutionModeUpdate(account *Account, requested *string) error {
	if requested == nil {
		return nil
	}
	mode, err := NormalizeAccountExecutionMode(*requested)
	if err != nil {
		return err
	}
	if account == nil || mode != account.EffectiveExecutionMode() {
		return infraerrors.BadRequest("EXECUTION_MODE_IMMUTABLE", "execution_mode cannot be changed on an existing account")
	}
	return nil
}

// ValidateVMKernelAccount checks only account compatibility, not deployment readiness.
func ValidateVMKernelAccount(account *Account) error {
	if account == nil {
		return infraerrors.BadRequest("INVALID_ACCOUNT", "account is required")
	}
	if _, err := NormalizeAccountExecutionMode(account.ExecutionMode); err != nil {
		return err
	}
	if !account.IsVMKernel() {
		return nil
	}
	invalid := func() error {
		return infraerrors.BadRequest("VM_KERNEL_ACCOUNT_UNSUPPORTED", "Docker kernel requires a standard OpenAI OAuth account without special execution settings")
	}
	if (account.Platform != PlatformAnthropic && account.Platform != PlatformOpenAI) || account.Type != AccountTypeOAuth || account.ParentAccountID != nil {
		return invalid()
	}
	if account.IsSIWCEnabled() || account.IsExcelOAuth() || account.IsOpenAIAgentIdentity() || account.IsOpenAIPersonalAccessToken() {
		return invalid()
	}
	for _, key := range []string{"base_url", "account_mode", "plugin_id", "plugin_instance_id"} {
		if strings.TrimSpace(account.GetCredential(key)) != "" {
			return invalid()
		}
	}
	for key, value := range account.Extra {
		special := strings.HasPrefix(key, "openai_excel_bps") || strings.HasPrefix(key, "openai_prism") || strings.HasPrefix(key, "prism_") || strings.HasPrefix(key, "openai_copilot") || strings.HasPrefix(key, "plugin_") || key == "openai_siwc"
		if !special {
			continue
		}
		if value == nil || reflect.ValueOf(value).IsZero() {
			continue
		}
		v := reflect.ValueOf(value)
		if (v.Kind() == reflect.Slice || v.Kind() == reflect.Map) && v.Len() == 0 {
			continue
		}
		return invalid()
	}
	return nil
}

// WithVMKernelExcluded restricts endpoints unsupported by guest kernels while
// preserving the ordinary scheduler and its sticky/account policy checks.
func WithVMKernelExcluded(ctx context.Context) context.Context {
	return context.WithValue(ctx, vmKernelExcludedContextKey{}, true)
}

type vmKernelExcludedContextKey struct{}

func vmKernelExcludedFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	excluded, _ := ctx.Value(vmKernelExcludedContextKey{}).(bool)
	return excluded
}
