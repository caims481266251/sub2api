package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/vmkernel"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Preparation is shared by HTTP and already upgraded WebSocket requests.
// Return policy metadata without writing through the HTTP response writer.
type vmKernelRequestPolicyError struct {
	Status  int
	Type    string
	Message string
}

func (e *vmKernelRequestPolicyError) Error() string { return e.Message }

func reportVMKernelPreparationError(c *gin.Context, ingress string, err error) {
	var policy *vmKernelRequestPolicyError
	var fast *OpenAIFastBlockedError
	switch {
	case errors.As(err, &policy):
		writeVMKernelRequestError(c, ingress, policy.Status, policy.Type, policy.Message)
	case errors.As(err, &fast):
		MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalPolicyDenied)
		writeVMKernelRequestError(c, ingress, http.StatusForbidden, "permission_error", fast.Message)
	default:
		writeVMKernelRequestError(c, ingress, http.StatusBadRequest, "invalid_request_error", err.Error())
	}
}

// Only an explicitly unsubmitted transport attempt can re-enter the existing
// account failover loop. Validation failures and unknown execution never replay.
func vmKernelBusinessError(ctx context.Context, result *vmkernel.Result, err error) error {
	if err == nil || ctx.Err() != nil || (result != nil && result.Dispatched) {
		return err
	}
	var attempt *vmkernel.AttemptError
	if !errors.As(err, &attempt) || !attempt.SafeToRetry {
		return err
	}
	switch attempt.Code {
	case "kernel_transport_failed", "vm_runtime_busy", "vm_runtime_unavailable", "vm_runtime_not_ready", "vm_binding_not_ready":
		return &UpstreamFailoverError{StatusCode: http.StatusServiceUnavailable,
			ResponseBody: []byte(`{"error":{"type":"upstream_unavailable","message":"VM execution is temporarily unavailable"}}`)}
	default:
		return err
	}
}

// The ordinary stream error classifier owns provider authentication, rate-limit
// and cooling policies. Kernel HTTP authentication errors are not provider 401s.
func (s *OpenAIGatewayService) observeVMKernelOpenAIResult(ctx context.Context, c *gin.Context, account *Account, model string, result *vmkernel.Result) {
	if result == nil {
		return
	}
	if snapshot := ParseCodexRateLimitHeaders(result.Headers); snapshot != nil {
		s.updateCodexUsageSnapshot(ctx, account.ID, snapshot)
	}
	if len(result.ErrorEvent) != 0 && vmKernelProviderErrorStatus(result.ErrorEvent) == http.StatusUnauthorized && s.excelOAuthReauth != nil {
		stateCtx, cancel := openAIAccountStateContext(ctx)
		defer cancel()
		if s.excelOAuthReauth.handleContainerUnauthorized(stateCtx, account, result.DispatchedCredentialsHash) {
			return
		}
	}
	if len(result.ErrorEvent) != 0 && s.rateLimitService != nil {
		s.handleOpenAIWSFailureAccountSideEffects(ctx, account, model, result.Headers, result.ErrorEvent)
	}
}

func vmKernelProviderErrorStatus(payload []byte) int {
	status := openAIStreamFailureStatus(payload, extractOpenAISSEErrorMessage(payload))
	if status != http.StatusBadGateway {
		return status
	}
	// Some kernels forward token_revoked/token_invalidated without a numeric
	// status. Recognize structured credential errors, never arbitrary message text.
	if openAIStreamCredentialAuthFailure(payload) {
		return http.StatusUnauthorized
	}
	if status = openAIWSPayloadTransientStatus(payload); status != 0 {
		return status
	}
	if nested := gjson.GetBytes(payload, "response.error"); nested.IsObject() {
		raw, _ := json.Marshal(map[string]json.RawMessage{"error": json.RawMessage(nested.Raw)})
		return openAIWSErrorHTTPStatus(raw)
	}
	return openAIWSErrorHTTPStatus(payload)
}

func writeVMKernelRequestError(c *gin.Context, ingress string, status int, errType, message string) {
	if c == nil || (c.Request != nil && c.Request.Context().Err() != nil) {
		return
	}
	if !c.Writer.Written() {
		if ingress == "messages" {
			writeAnthropicError(c, status, errType, message)
		} else {
			writeChatCompletionsError(c, status, errType, message)
		}
		return
	}
	envelope := map[string]any{"error": map[string]string{"type": errType, "message": message}}
	if ingress != "chat" {
		envelope["type"] = "error"
	}
	raw, _ := json.Marshal(envelope)
	if ingress == "messages" {
		_, _ = fmt.Fprint(c.Writer, "event: error\n")
	}
	_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", raw)
	c.Writer.Flush()
}

func reportVMKernelProviderError(c *gin.Context, account *Account, ingress string, result *vmkernel.Result) {
	if result == nil || len(result.ErrorEvent) == 0 {
		return
	}
	status := vmKernelProviderErrorStatus(result.ErrorEvent)
	message := sanitizeUpstreamErrorMessage(extractOpenAISSEErrorMessage(result.ErrorEvent))
	if message == "" {
		message = "Upstream request failed"
	}
	status, errType, message, _ := applyErrorPassthroughRule(c, account.Platform, status, result.ErrorEvent, status, "upstream_error", message)
	setOpsUpstreamError(c, status, message, "")
	if retry := result.Headers.Get("Retry-After"); retry != "" && !c.Writer.Written() {
		c.Header("Retry-After", retry)
	}
	writeVMKernelRequestError(c, ingress, status, errType, message)
}
