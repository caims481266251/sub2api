package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/vmkernel"
	"github.com/Wei-Shaw/sub2api/internal/vmruntime"
)

func vmKernelControlErrorCode(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "context_deadline_exceeded"
	}
	if errors.Is(err, context.Canceled) {
		return "context_cancelled"
	}
	var attempt *vmkernel.AttemptError
	if errors.As(err, &attempt) {
		switch attempt.Code {
		case "kernel_binding_invalid", "kernel_protocol_unsupported", "kernel_body_invalid", "kernel_transport_failed", "kernel_stream_incomplete", "kernel_response_failed", "kernel_completion_metadata_missing", "kernel_downstream_write_failed":
			return attempt.Code
		}
		if strings.HasPrefix(attempt.Code, "kernel_status_") {
			v := strings.TrimPrefix(attempt.Code, "kernel_status_")
			n, e := strconv.Atoi(v)
			if e == nil && len(v) == 3 && n >= 100 && n <= 599 {
				return "kernel_status_" + strconv.Itoa(n)
			}
		}
		return "kernel_operation_failed"
	}
	var helper *vmruntime.Error
	if errors.As(err, &helper) {
		// Never log an untrusted helper code.
		switch helper.Code {
		case "docker_operation_unknown", "helper_transport_unknown", "guest_generation_mismatch", "guest_observation_invalid", "instance_deleting", "instance_not_found", "runtime_observation_policy_mismatch", "runtime_policy_conflict", "accelerator_probe_failed", "accelerator_probe_mismatch", "artifact_digest_mismatch", "pinned_image_missing":
			return helper.Code
		}
		return "helper_operation_failed"
	}
	if err != nil {
		switch err.Error() {
		case "vm_probe_incomplete", "vm_probe_empty", "vm_probe_model_not_allowed", "vm_binding_not_ready", "vm_binding_mismatch", "vm_token_provider_unavailable", "vm_access_token_missing", "vm_access_token_expired", "vm_access_token_expiry_missing":
			return err.Error()
		}
	}
	return "runtime_operation_failed"
}
func logVMKernelControlFailure(accountID int64, stage string, generation uint64, err error) {
	switch stage {
	case "prepare", "apply-config", "inspect", "probe", "binding", "credentials":
	default:
		stage = "control"
	}
	slog.Warn("vm_kernel_control_failed", "account_id", accountID, "stage", stage, "generation", generation, "code", vmKernelControlErrorCode(err))
}

// Request diagnostics contain only bounded classifications, never raw transport
// errors, proxy URLs, credentials, request bodies or cancellation payloads.
func logVMKernelAttemptFailure(accountID int64, generation uint64, result *vmkernel.Result, err error) {
	reason := "transport_or_stream_error"
	var netErr net.Error
	switch {
	case errors.Is(err, context.Canceled):
		reason = "context_cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		reason = "context_deadline_exceeded"
	case errors.As(err, &netErr) && netErr.Timeout():
		reason = "transport_timeout"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		reason = "unexpected_eof"
	}
	cancel := "not_attempted"
	switch result.CancelStatus {
	case "transport_failed", "response_incomplete", "rejected", "acknowledged":
		cancel = result.CancelStatus
	}
	slog.Warn("vm_kernel_attempt_failed", "account_id", accountID, "generation", generation,
		"attempt_id", result.AttemptID, "code", vmKernelControlErrorCode(err), "reason", reason,
		"duration_ms", result.Duration.Milliseconds(), "cancel_status", cancel, "cancel_http_status", result.CancelHTTPStatus)
}
