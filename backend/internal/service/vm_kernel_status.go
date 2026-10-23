package service

import "time"

// VMKernelStatus is the complete public management projection. Private runtime
// tokens, socket paths, instance bindings and credential revisions stay local.
type VMKernelStatus struct {
	Phase             string `json:"phase"`
	Ready             bool   `json:"ready"`
	ErrorCode         string `json:"error_code,omitempty"`
	Capacity          int    `json:"capacity"`
	ActualAccelerator string `json:"actual_accelerator,omitempty"`
}

func VMKernelRuntimeSummary(account *Account) *VMKernelStatus {
	if !account.IsVMKernel() {
		return nil
	}
	summary := &VMKernelStatus{Phase: "pending", Capacity: account.Concurrency}
	value, ok := vmKernelReadiness.Load(account.ID)
	if !ok {
		return summary
	}
	status, ok := value.(vmRuntimeStatus)
	if !ok {
		return summary
	}
	if status.state.ActualAccelerator == "kvm" || status.state.ActualAccelerator == "tcg" {
		summary.ActualAccelerator = status.state.ActualAccelerator
	}
	switch status.state.Phase {
	case "pending", "preparing", "kernel_ready", "stopped", "removed", "deleting", "error", "create", "start", "stop", "restart", "remove", "rebuild", "inspect", "health", "credentials", "proxy":
		summary.Phase = status.state.Phase
	default:
		summary.Phase = "pending"
	}
	summary.Ready = VMKernelRuntimeReady(account)
	if status.state.Ready && !summary.Ready {
		if time.Since(status.checked) >= 30*time.Second {
			summary.Phase = "stale"
		} else {
			summary.Phase = "preparing"
		}
	}
	if status.state.DeleteRequested {
		summary.Phase = "deleting"
		summary.Ready = false
	}
	switch status.state.ErrorCode {
	case "":
	case "kernel_attempt_unknown":
		// An unresolved request is not a local VM fault. Preserve the runtime's
		// admission decision without displaying model-call errors on accounts.
		if !summary.Ready && !status.state.DeleteRequested {
			summary.Phase = "pending"
		}
	case "account_already_bound", "artifact_digest_invalid", "artifact_digest_mismatch", "artifact_manifest_required", "artifact_missing", "artifact_not_regular", "artifact_path_invalid", "binding_mismatch", "credential_projection_required", "docker_inspect_invalid", "docker_operation_unknown", "guest_generation_mismatch", "guest_observation_invalid", "helper_already_running", "helper_closed", "helper_response_invalid", "helper_transport_unknown", "immutable_spec_mismatch", "inspect_generation_mismatch", "instance_deleting", "instance_not_found", "internal_token_corrupt", "internal_token_missing", "invalid_helper_configuration", "invalid_projection", "invalid_request", "invalid_spec", "kvm_not_enabled", "kvm_probe_failed", "linux_amd64_required", "manifest_invalid", "manifest_inventory_unreadable", "manifest_unreadable", "mutation_requires_new_generation", "pinned_image_missing", "projection_operation_invalid", "projection_requires_new_generation", "projection_revision_conflict", "projection_revision_corrupt", "proxy_ipv4_required", "proxy_projection_required", "proxy_resolution_failed", "proxy_revision_conflict", "proxy_url_invalid", "refresh_credential_forbidden", "runtime_must_be_stopped", "runtime_ownership_conflict", "runtime_removal_unconfirmed", "runtime_still_running", "runtime_template_unverified", "stale_generation", "symlink_forbidden", "template_not_allowed", "template_poc_not_verified", "unsupported_operation":
		summary.ErrorCode = status.state.ErrorCode
	default:
		summary.ErrorCode = "runtime_operation_failed"
	}
	return summary
}
