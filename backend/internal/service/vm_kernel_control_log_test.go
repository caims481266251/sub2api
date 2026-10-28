package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/vmkernel"
	"github.com/Wei-Shaw/sub2api/internal/vmruntime"
)

func TestVMKernelPreDispatchLogPreservesMappingAndRedactsSecrets(t *testing.T) {
	var out bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&out, nil)))
	defer slog.SetDefault(old)
	const secret = "secret-token-body-proxy"
	const attemptID = "5cb660a8996749d0b53dda4cc4f3b043"
	for _, code := range []string{"kernel_transport_failed", "vm_runtime_busy", "vm_runtime_unavailable", "vm_runtime_not_ready", "vm_binding_not_ready"} {
		out.Reset()
		result := &vmkernel.Result{AttemptID: attemptID, RequestID: secret, ErrorEvent: json.RawMessage(secret), Response: json.RawMessage(secret)}
		err := vmKernelBusinessError(context.Background(), result, &vmkernel.AttemptError{Code: code, SafeToRetry: true, Cause: errors.New(secret)})
		var mapped *UpstreamFailoverError
		if !errors.As(err, &mapped) || mapped.StatusCode != http.StatusServiceUnavailable || string(mapped.ResponseBody) != `{"error":{"type":"upstream_unavailable","message":"VM execution is temporarily unavailable"}}` {
			t.Fatalf("mapping changed for %s: %v", code, err)
		}
		var row map[string]any
		if err := json.Unmarshal(out.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		if row["msg"] != "vm_kernel_pre_dispatch_failed" || row["stage"] != "pre_dispatch" || row["code"] != code || row["attempt_id"] != attemptID || strings.Contains(out.String(), secret) {
			t.Fatalf("incorrect or private diagnostic: %s", out.String())
		}
	}
	out.Reset()
	logVMKernelPreDispatchFailure(context.Background(), secret, &vmkernel.Result{AttemptID: secret})
	if out.Len() != 0 {
		t.Fatal("untrusted diagnostic code was logged")
	}
	for _, invalidID := range []string{secret, strings.Repeat("z", 32), strings.ToUpper(attemptID), "5cb660a8-9967-49d0-b53d-da4cc4f3b043"} {
		out.Reset()
		logVMKernelPreDispatchFailure(context.Background(), "kernel_transport_failed", &vmkernel.Result{AttemptID: invalidID})
		if strings.Contains(out.String(), invalidID) || strings.Contains(out.String(), "attempt_id") {
			t.Fatal("non-kernel attempt identifier was logged")
		}
	}
	for _, result := range []*vmkernel.Result{nil, {Dispatched: true}} {
		out.Reset()
		err := &vmkernel.AttemptError{Code: "kernel_transport_failed", SafeToRetry: false, Cause: errors.New(secret)}
		if vmKernelBusinessError(context.Background(), result, err) != err || out.Len() != 0 {
			t.Fatal("unsafe attempt mapping or logging changed")
		}
	}
}

func TestVMKernelAdmissionLockTimeoutLogsSafeStage(t *testing.T) {
	s, account, fixture := vmLifecycleSetup(t)
	s.prepare(context.Background(), account)
	slot := s.slot(account.ID)
	slot.mu.Lock()
	defer slot.mu.Unlock()
	var out bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&out, nil)))
	defer slog.SetDefault(old)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	result, err := s.Execute(ctx, account, "responses", []byte(`{"input":"secret-token-body-proxy"}`), nil)
	var attempt *vmkernel.AttemptError
	if result != nil || !errors.As(err, &attempt) || attempt.Code != "vm_runtime_busy" || !attempt.SafeToRetry {
		t.Fatalf("admission timeout boundary changed: result=%v err=%v", result, err)
	}
	var row map[string]any
	if err := json.Unmarshal(out.Bytes(), &row); err != nil {
		t.Fatal(err)
	}
	if row["msg"] != "vm_kernel_admission_failed" || row["stage"] != "admission_lock_timeout" || row["code"] != "vm_runtime_busy" || row["account_id"] != float64(account.ID) || strings.Contains(out.String(), "secret-token-body-proxy") {
		t.Fatalf("incorrect or private admission diagnostic: %s", out.String())
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.kernelCalls != 0 {
		t.Fatal("timed out admission dispatched inference")
	}
}

func TestVMKernelControlFailureLogRedactsSecrets(t *testing.T) {
	var out bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&out, nil)))
	defer slog.SetDefault(old)
	secret := "secret-token-body-revision"
	for _, err := range []error{errors.New(secret), &vmkernel.AttemptError{Code: "kernel_transport_failed", Cause: errors.New(secret)}, &vmkernel.AttemptError{Code: "kernel_stream_idle_timeout", Cause: errors.New(secret)}, &vmkernel.AttemptError{Code: "kernel_stream_idle_timeout" + secret}, &vmkernel.AttemptError{Code: secret}, &vmkernel.AttemptError{Code: "kernel_status_401"}, &vmkernel.AttemptError{Code: "kernel_status_401" + secret}, &vmruntime.Error{Code: secret}, context.DeadlineExceeded, context.Canceled} {
		logVMKernelControlFailure(42, secret, 7, err)
	}
	raw := out.String()
	if strings.Contains(raw, secret) {
		t.Fatal("private error detail leaked")
	}
	for _, safe := range []string{"kernel_transport_failed", "kernel_stream_idle_timeout", "kernel_status_401", "context_deadline_exceeded", "context_cancelled", "\"stage\":\"control\"", "\"generation\":7"} {
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
	result.CancelStatus, result.CancelHTTPStatus = "unavailable", 404
	logVMKernelAttemptFailure(42, 7, result, &vmkernel.AttemptError{Code: "kernel_stream_incomplete"})
	logVMKernelAttemptFailure(42, 7, result, fmt.Errorf("wrapped: %w", &vmkernel.AttemptError{Code: "kernel_stream_idle_timeout", Cause: errors.New(secret)}))
	result.CancelStatus = secret
	logVMKernelAttemptFailure(42, 7, result, &vmkernel.AttemptError{Code: secret, Cause: errors.New(secret)})
	raw := out.String()
	if strings.Contains(raw, secret) || strings.Contains(raw, "vm_kernel_control_failed") {
		t.Fatal("request diagnostic leaked data or became runtime health failure")
	}
	for _, expected := range []string{"vm_kernel_attempt_failed", "context_deadline_exceeded", "acknowledged", "unavailable", "not_attempted"} {
		if !strings.Contains(raw, expected) {
			t.Errorf("missing %s", expected)
		}
	}
	var idle struct {
		Code             string `json:"code"`
		Reason           string `json:"reason"`
		CancelStatus     string `json:"cancel_status"`
		CancelHTTPStatus int    `json:"cancel_http_status"`
	}
	if err := json.Unmarshal([]byte(strings.Split(strings.TrimSpace(raw), "\n")[2]), &idle); err != nil {
		t.Fatal(err)
	}
	if idle.Code != "kernel_stream_idle_timeout" || idle.Reason != "stream_idle_timeout" || idle.CancelStatus != "unavailable" || idle.CancelHTTPStatus != 404 {
		t.Fatalf("idle timeout lost its diagnostic or cancellation classification: %+v", idle)
	}
}

func TestVMKernelAttemptCancellationLogRequiresClientAndTransportEvidence(t *testing.T) {
	var out bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&out, nil)))
	defer slog.SetDefault(old)
	for _, tc := range []struct {
		name              string
		client, transport bool
		cause             error
		level, message    string
	}{
		{"client_cancelled", true, true, context.Canceled, "INFO", "vm_kernel_attempt_cancelled"},
		{"missing_client_evidence", false, true, context.Canceled, "WARN", "vm_kernel_attempt_failed"},
		{"missing_cleanup_evidence", true, false, context.Canceled, "WARN", "vm_kernel_attempt_failed"},
		{"deadline_is_still_failure", true, true, context.DeadlineExceeded, "WARN", "vm_kernel_attempt_failed"},
		{"stream_fault_is_still_failure", false, true, errors.New("secret-upstream-detail"), "WARN", "vm_kernel_attempt_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out.Reset()
			result := &vmkernel.Result{AttemptID: "fixture", CancelStatus: "stream_closed", ClientDisconnect: tc.client, ExecutionTransportCanceled: tc.transport}
			logVMKernelAttemptFailure(42, 7, result, &vmkernel.AttemptError{Code: "kernel_stream_incomplete", Cause: tc.cause})
			var row map[string]any
			if err := json.Unmarshal(out.Bytes(), &row); err != nil {
				t.Fatal(err)
			}
			if row["level"] != tc.level || row["msg"] != tc.message || row["cancel_status"] != "stream_closed" || row["execution_transport_canceled"] != tc.transport || row["cancel_http_status"] != float64(0) {
				t.Fatalf("incorrect cancellation diagnostic: %s", out.String())
			}
			if strings.Contains(out.String(), "secret-upstream-detail") || strings.Contains(out.String(), "vm_kernel_control_failed") {
				t.Fatal("request cancellation leaked raw detail or became container health failure")
			}
		})
	}
}
