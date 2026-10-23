package service

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/vmruntime"
)

func TestVMKernelRuntimeSummarySanitizesPrivateState(t *testing.T) {
	account := &Account{ID: 987654322, Platform: PlatformOpenAI, ExecutionMode: AccountExecutionModeVMKernel, Concurrency: 12, Credentials: map[string]any{"access_token": "test-token"}}
	t.Cleanup(func() { vmKernelReadiness.Delete(account.ID) })
	if VMKernelRuntimeSummary(&Account{}) != nil {
		t.Fatal("ordinary account gained VM status")
	}
	if summary := VMKernelRuntimeSummary(account); summary.Phase != "pending" || summary.Ready {
		t.Fatal("missing status must be pending")
	}
	vmKernelReadiness.Store(account.ID, vmRuntimeStatus{state: vmruntime.State{Phase: "kernel_ready", Ready: true, KernelSocket: "/secret/kernel.sock", InternalToken: "secret-token", ErrorCode: "private-error-detail"}, credentials: vmCredentialRevision(account), proxy: vmProxyRevision(account), checked: time.Now()})
	summary := VMKernelRuntimeSummary(account)
	if !summary.Ready || summary.Capacity != 12 || summary.ErrorCode != "runtime_operation_failed" {
		t.Fatalf("bad summary: %+v", summary)
	}
	encoded, _ := json.Marshal(summary)
	for _, private := range []string{"secret", "private-error-detail", "credential", "socket", "revision", "token"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("private state leaked: %s", encoded)
		}
	}
	var shape map[string]any
	if err := json.Unmarshal(encoded, &shape); err != nil {
		t.Fatal(err)
	}
	if len(shape) != 4 {
		t.Fatalf("unexpected public fields: %s", encoded)
	}
}

func TestVMKernelRuntimeSummaryAcceleratorIsAnAllowlistedObservation(t *testing.T) {
	account := &Account{ID: 987654323, ExecutionMode: AccountExecutionModeVMKernel}
	t.Cleanup(func() { vmKernelReadiness.Delete(account.ID) })
	for _, actual := range []string{"tcg", "kvm", "/private/runtime/path"} {
		vmKernelReadiness.Store(account.ID, vmRuntimeStatus{state: vmruntime.State{ActualAccelerator: actual}, checked: time.Now()})
		summary := VMKernelRuntimeSummary(account)
		if actual == "tcg" || actual == "kvm" {
			if summary.ActualAccelerator != actual {
				t.Fatalf("observed accelerator was lost")
			}
		} else if summary.ActualAccelerator != "" {
			t.Fatal("unrecognized accelerator leaked")
		}
	}
}

func TestVMKernelRuntimeSummaryPreservesConfiguredConcurrency(t *testing.T) {
	account := &Account{ID: 987654324, Platform: PlatformOpenAI, ExecutionMode: AccountExecutionModeVMKernel, Credentials: map[string]any{"access_token": "test-token"}}
	t.Cleanup(func() { vmKernelReadiness.Delete(account.ID) })
	for _, concurrency := range []int{0, 12, 32} {
		account.Concurrency = concurrency
		vmKernelReadiness.Delete(account.ID)
		if summary := VMKernelRuntimeSummary(account); summary.Capacity != concurrency || summary.Ready {
			t.Fatalf("pending configuration lost: %+v", summary)
		}
		vmKernelReadiness.Store(account.ID, vmRuntimeStatus{state: vmruntime.State{Phase: "kernel_ready", Ready: true}, credentials: vmCredentialRevision(account), proxy: vmProxyRevision(account), checked: time.Now()})
		if summary := VMKernelRuntimeSummary(account); summary.Capacity != concurrency || !summary.Ready {
			t.Fatalf("ready configuration lost: %+v", summary)
		}
	}
}

func TestVMKernelRuntimeSummaryKeepsRequestOutcomeOutOfAccountBadge(t *testing.T) {
	account := &Account{ID: 987654329, Platform: PlatformOpenAI, ExecutionMode: AccountExecutionModeVMKernel, Credentials: map[string]any{"access_token": "test-token"}}
	t.Cleanup(func() { vmKernelReadiness.Delete(account.ID) })
	vmKernelReadiness.Store(account.ID, vmRuntimeStatus{state: vmruntime.State{Phase: "error", Ready: false, ActualAccelerator: "tcg", ErrorCode: "kernel_attempt_unknown"}, credentials: vmCredentialRevision(account), proxy: vmProxyRevision(account), checked: time.Now()})
	summary := VMKernelRuntimeSummary(account)
	if summary.Ready || summary.Phase != "pending" || summary.ErrorCode != "" || summary.ActualAccelerator != "tcg" {
		t.Fatalf("request failure became account runtime error: %+v", summary)
	}
	vmKernelReadiness.Store(account.ID, vmRuntimeStatus{state: vmruntime.State{Phase: "error", Ready: false, ErrorCode: "kvm_not_enabled"}, checked: time.Now()})
	if summary = VMKernelRuntimeSummary(account); summary.Ready || summary.ErrorCode != "kvm_not_enabled" {
		t.Fatalf("local runtime failure lost: %+v", summary)
	}
}
