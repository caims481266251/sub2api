package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/vmkernel"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
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
		logVMKernelPreDispatchFailure(ctx, attempt.Code, result)
		return &UpstreamFailoverError{StatusCode: http.StatusServiceUnavailable,
			ResponseBody: []byte(`{"error":{"type":"upstream_unavailable","message":"VM execution is temporarily unavailable"}}`)}
	default:
		return err
	}
}

// The ordinary HTTP/stream error handlers own provider authentication, rate-limit
// and cooling policies. Local kernel authentication failures are not provider 401s.
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
		if result.ProviderHTTPFailure {
			// Preserve HTTP quota headers just as the ordinary HTTP path does.
			// Stream-only semantic filtering is not applicable to this rejection,
			// even when the public ingress is WebSocket or SSE.
			status := vmKernelProviderErrorStatus(result.ErrorEvent)
			s.rateLimitService.observeQualityStatus(ctx, account, status)
			s.handleOpenAIAccountUpstreamError(ctx, account, status, result.Headers, result.ErrorEvent, model)
		} else {
			s.handleOpenAIWSFailureAccountSideEffects(ctx, account, model, result.Headers, result.ErrorEvent)
		}
	}
}

func vmKernelProviderErrorStatus(payload []byte) int {
	// A settled provider response's explicit status takes precedence over
	// heuristics on its message/code, including request errors such as HTTP 400.
	for _, path := range openAIStreamErrorStatusPaths {
		value := gjson.GetBytes(payload, path)
		if status := value.Int(); value.Type == gjson.Number && value.Float() == float64(status) && status >= 400 && status <= 599 {
			return int(status)
		}
	}
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

func writeVMKernelRequestError(c *gin.Context, ingress string, status int, errType, message string, results ...*vmkernel.Result) {
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
	MarkOpsStreamFailure(c, errType, errType, message, status)
	if ingress == "responses" {
		source, _ := json.Marshal(map[string]any{"error": map[string]string{"type": errType, "code": errType, "message": message}})
		var responseID, model string
		createdAt := int64(0)
		if len(results) > 0 && results[0] != nil {
			responseID, model = results[0].ResponseID, results[0].Model
			createdAt = gjson.GetBytes(results[0].Response, "created_at").Int()
		}
		if createdAt <= 0 {
			createdAt = time.Now().Unix()
		}
		_, err := fmt.Fprint(c.Writer, buildOpenAIResponseFailedSSE(responseID, model, createdAt, source, message))
		if err == nil {
			err = FlushGatewayResponse(c.Writer)
		}
		if err != nil {
			c.Set(openAIDownstreamDisconnectedKey, true)
		}
		MarkResponseCommitted(c)
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
	MarkResponseCommitted(c)
}

func reportVMKernelIdleTimeout(c *gin.Context, ingress string, err error, results ...*vmkernel.Result) bool {
	var attempt *vmkernel.AttemptError
	if !errors.As(err, &attempt) || attempt.Code != "kernel_stream_idle_timeout" {
		return false
	}
	writeVMKernelRequestError(c, ingress, http.StatusGatewayTimeout, "stream_timeout", "Kernel response stream timed out", results...)
	return true
}

func reportVMKernelProviderError(c *gin.Context, account *Account, ingress string, result *vmkernel.Result) {
	if result == nil || len(result.ErrorEvent) == 0 {
		return
	}
	if account.Platform == PlatformOpenAI {
		reportVMKernelOpenAIProviderError(c, account, ingress, result)
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
	writeVMKernelRequestError(c, ingress, status, errType, message, result)
}

// Preserve provider error semantics at the Docker transport boundary. This
// only presents the settled failure; account state and retry policy are owned
// by observeVMKernelOpenAIResult and vmKernelBusinessError.
func reportVMKernelOpenAIProviderError(c *gin.Context, account *Account, ingress string, result *vmkernel.Result) {
	if c == nil || (c.Request != nil && c.Request.Context().Err() != nil) {
		return
	}
	source := gjson.GetBytes(result.ErrorEvent, "response.error")
	if !source.IsObject() {
		source = gjson.GetBytes(result.ErrorEvent, "error")
	}
	nested := source.IsObject()
	if !nested {
		source = gjson.ParseBytes(result.ErrorEvent)
	}
	fields := make(map[string]any)
	for _, key := range []string{"type", "code", "param"} {
		if key == "type" && !nested {
			continue // The top-level SSE type describes the event, not the error.
		}
		value := source.Get(key)
		if value.Exists() && (value.Type == gjson.String || value.Type == gjson.Null) {
			fields[key] = value.Value()
		}
	}
	message := sanitizeUpstreamErrorMessage(extractOpenAISSEErrorMessage(result.ErrorEvent))
	if message == "" {
		message = "Upstream request failed"
	}
	fields["message"] = message
	for _, key := range []string{"status", "status_code", "resets_at", "resets_in_seconds"} {
		value := source.Get(key)
		if value.Type == gjson.Number {
			fields[key] = value.Value()
		}
	}
	upstreamStatus := vmKernelProviderErrorStatus(result.ErrorEvent)
	status, errType, ruleMessage, matched := applyErrorPassthroughRule(c, account.Platform, upstreamStatus, result.ErrorEvent, upstreamStatus, source.Get("type").String(), message)
	if matched {
		fields["type"], fields["message"] = errType, ruleMessage
	}
	setOpsUpstreamError(c, upstreamStatus, message, "")
	if !c.Writer.Written() {
		if retry := result.Headers.Get("Retry-After"); retry != "" {
			c.Header("Retry-After", retry)
		}
		envelope := gin.H{"error": fields}
		if ingress == "messages" {
			envelope["type"] = "error"
		}
		c.JSON(status, envelope)
		MarkResponseCommitted(c)
		return
	}
	MarkOpsStreamFailure(c, source.Get("type").String(), source.Get("code").String(), message, upstreamStatus)
	envelope := gin.H{"error": fields}
	event := ""
	if ingress == "responses" {
		id := result.ResponseID
		if id == "" {
			id = "resp_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		}
		createdAt := gjson.GetBytes(result.Response, "created_at").Int()
		if createdAt <= 0 {
			createdAt = time.Now().Unix()
		}
		response := gin.H{"id": id, "object": "response", "created_at": createdAt, "status": "failed", "output": []any{}, "error": fields}
		if result.Model != "" {
			response["model"] = result.Model
		}
		event = "response.failed"
		envelope = gin.H{"type": event, "sequence_number": openAIResponsesFailedSequenceNumber(result.ErrorEvent), "response": response}
	} else if ingress == "messages" {
		event, envelope["type"] = "error", "error"
	}
	raw, _ := json.Marshal(envelope)
	var writeErr error
	if event != "" {
		_, writeErr = fmt.Fprintf(c.Writer, "event: %s\n", event)
	}
	if writeErr == nil {
		_, writeErr = fmt.Fprintf(c.Writer, "data: %s\n\n", raw)
	}
	if writeErr == nil {
		writeErr = FlushGatewayResponse(c.Writer)
	}
	if writeErr != nil {
		c.Set(openAIDownstreamDisconnectedKey, true)
	}
	MarkResponseCommitted(c)
}
