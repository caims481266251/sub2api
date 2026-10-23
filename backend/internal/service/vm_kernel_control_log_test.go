package service

import (
	"bytes"
	"context"
	"errors"
	"github.com/Wei-Shaw/sub2api/internal/vmkernel"
	"github.com/Wei-Shaw/sub2api/internal/vmruntime"
	"log/slog"
	"strings"
	"testing"
)

func TestVMKernelControlFailureLogRedactsSecrets(t *testing.T) {
	var out bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&out, nil)))
	defer slog.SetDefault(old)
	secret := "secret-token-body-revision"
	for _, err := range []error{errors.New(secret), &vmkernel.AttemptError{Code: "kernel_transport_failed", Cause: errors.New(secret)}, &vmkernel.AttemptError{Code: secret}, &vmkernel.AttemptError{Code: "kernel_status_401"}, &vmkernel.AttemptError{Code: "kernel_status_401" + secret}, &vmruntime.Error{Code: secret}, context.DeadlineExceeded, context.Canceled} {
		logVMKernelControlFailure(42, secret, 7, err)
	}
	raw := out.String()
	if strings.Contains(raw, secret) {
		t.Fatal("private error detail leaked")
	}
	for _, safe := range []string{"kernel_transport_failed", "kernel_status_401", "context_deadline_exceeded", "context_cancelled", "\"stage\":\"control\"", "\"generation\":7"} {
		if !strings.Contains(raw, safe) {
			t.Errorf("missing safe diagnostic %s", safe)
		}
	}
}

func TestVMKernelAttemptFailureLogKeepsRequestErrorsOutOfHealth(t *testing.T) {
	var out bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&out, nil)))
	defer slog.SetDefault(old)
	secret := "secret-token-body-proxy"
	result := &vmkernel.Result{AttemptID: "fixture", CancelStatus: "acknowledged", CancelHTTPStatus: 200}
	logVMKernelAttemptFailure(42, 7, result, &vmkernel.AttemptError{Code: "kernel_transport_failed", Cause: context.DeadlineExceeded})
	result.CancelStatus = secret
	logVMKernelAttemptFailure(42, 7, result, &vmkernel.AttemptError{Code: secret, Cause: errors.New(secret)})
	raw := out.String()
	if strings.Contains(raw, secret) || strings.Contains(raw, "vm_kernel_control_failed") {
		t.Fatal("request diagnostic leaked data or became runtime health failure")
	}
	for _, expected := range []string{"vm_kernel_attempt_failed", "context_deadline_exceeded", "acknowledged", "not_attempted"} {
		if !strings.Contains(raw, expected) {
			t.Errorf("missing %s", expected)
		}
	}
}
