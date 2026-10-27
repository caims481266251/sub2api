package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/vmkernel"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Reject unsupported execution features before typed conversion can discard them.
func validateVMKernelIngress(account *Account, body []byte) error {
	if err := ValidateVMKernelAccount(account); err != nil {
		return err
	}
	// Request size is governed by the shared ingress policy. Response buffering
	// limits must not reject requests accepted by the ordinary gateway.
	if !gjson.ValidBytes(body) {
		return fmt.Errorf("invalid JSON request")
	}
	for _, key := range []string{"audio", "modalities", "image_generation", "background", "additional_tools", "context_management", "compact"} {
		value := gjson.GetBytes(body, key)
		if value.Exists() && value.Raw != "null" && value.Raw != "false" {
			return fmt.Errorf("VM request feature %s is unsupported", key)
		}
	}
	if v := gjson.GetBytes(body, "generate"); v.Exists() && !v.Bool() {
		return fmt.Errorf("VM warmup generate=false is unsupported")
	}
	for _, tool := range gjson.GetBytes(body, "tools").Array() {
		kind := tool.Get("type").String()
		// Native Responses tools use the ordinary OpenAI business normalizers.
		// The Docker transport must not impose a function-only whitelist.
		if account.Platform != PlatformOpenAI && kind != "" && kind != "function" {
			return fmt.Errorf("VM tool type %s is unsupported", kind)
		}
	}
	return nil
}

// Match ordinary Responses forwarding before presentation and replay caching.
func restoreVMKernelResponsesTools(c *gin.Context, data []byte, eventType string) ([]byte, error) {
	data, err := restoreOpenAIResponsesNamespacePayload(c, data)
	if err != nil {
		return nil, err
	}
	return restoreCodexToolNamesFromSSEContext(c, data, eventType), nil
}

func vmKernelStringPointer(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// Output presentation is downstream of the native attempt. It never initiates
// a second attempt, selects an account, or records usage.
func vmKernelHTTP(ctx context.Context, runtime *VMKernelService, c *gin.Context, account *Account, ingress, provider string, body []byte, stream bool, model string, gateways ...*OpenAIGatewayService) (result *vmkernel.Result, resultErr error) {
	if runtime == nil {
		return nil, fmt.Errorf("VM kernel runtime is unavailable")
	}
	var gateway *OpenAIGatewayService
	var staged http.Header
	stageHeaders := func(headers http.Header) { stageOpenAICodexTurnState(&staged, headers) }
	if provider == "responses" {
		gateway = &OpenAIGatewayService{}
		if len(gateways) > 0 && gateways[0] != nil {
			gateway = gateways[0]
		}
		ctx = gateway.vmKernelResponsesTTFTContext(ctx)
		ctx = gateway.vmKernelResponsesHeaderContext(ctx, c, account, body)
		ctx = vmkernel.WithOnResponseHeaders(ctx, stageHeaders)
	}
	if gateway != nil {
		defer func() {
			recordVMKernelRecentRequest(withRecentRequestScope(ctx, account, codexAccountIdentitySource(c, account)), gateway.recentRequests, account, gjson.GetBytes(body, "model").String(), result, resultErr)
		}()
	}
	emit := func(kind string, data []byte) error {
		if c.Request.Context().Err() != nil {
			return c.Request.Context().Err()
		}
		if gateway != nil && !c.Writer.Written() {
			gateway.relayOpenAICodexTurnState(c, account, staged)
			gateway.noteVMKernelTurnState(c, account, body, extractOpenAICodexTurnState(staged))
		}
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("X-Accel-Buffering", "no")
		if kind != "" {
			if _, err := fmt.Fprintf(c.Writer, "event: %s\n", kind); err != nil {
				c.Set(openAIDownstreamDisconnectedKey, true)
				return err
			}
		}
		if _, err := fmt.Fprintf(c.Writer, "data: %s\n\n", data); err != nil {
			c.Set(openAIDownstreamDisconnectedKey, true)
			return err
		}
		err := FlushGatewayResponse(c.Writer)
		if err != nil {
			c.Set(openAIDownstreamDisconnectedKey, true)
		}
		return err
	}
	presentation := newVMKernelPresentation(ingress, provider, stream, emit)
	consume := func(event vmkernel.Event) error {
		data := event.Data
		if provider == "responses" {
			var err error
			data, err = restoreVMKernelResponsesTools(c, data, event.Type)
			if err != nil {
				return err
			}
		}
		if event.Type == vmkernel.CodexMetadataEvent {
			if ingress != "responses" || !stream {
				return nil
			}
			if err := emit(event.Type, data); err != nil {
				return err
			}
			gateway.noteVMKernelTurnState(c, account, body, vmKernelMetadataState(data))
			return nil
		}
		return presentation.event(event.Type, data)
	}
	var err error
	if provider == "responses" && stream && gateway.cfg != nil && gateway.cfg.Gateway.StreamKeepaliveInterval > 0 {
		result, err = runVMKernelSSE(ctx, c, time.Duration(gateway.cfg.Gateway.StreamKeepaliveInterval)*time.Second,
			func(ctx context.Context, emit func(vmkernel.Event) error) (*vmkernel.Result, error) {
				return runtime.Execute(ctx, account, provider, body, emit)
			}, stageHeaders, consume)
	} else {
		result, err = runtime.Execute(ctx, account, provider, body, consume)
	}
	if err != nil {
		return result, err
	}
	if result == nil || !result.Completed {
		return result, fmt.Errorf("VM kernel did not complete")
	}
	output, err := presentation.finish(model, result.Usage)
	if err != nil {
		return result, err
	}
	if len(presentation.nativeResponse) > 0 {
		// Replay the same complete, restored tool items delivered to the client.
		result.Response = presentation.nativeResponse
	}
	if !stream {
		if gateway != nil {
			gateway.relayOpenAICodexTurnState(c, account, result.Headers)
			gateway.noteVMKernelTurnState(c, account, body, extractOpenAICodexTurnState(result.Headers))
		}
		c.Data(http.StatusOK, "application/json", output)
	}
	return result, nil
}

// KernelClient creates AttemptID immediately before HTTP transport and returns it
// even on a failed dial. Runtime admission/preparation failures return no attempt.
// Reuse its strict completion and per-attempt timing instead of replaying events.
func recordVMKernelRecentRequest(ctx context.Context, cache AccountRecentRequestsCache, account *Account, model string, result *vmkernel.Result, err error) {
	if result == nil || result.AttemptID == "" {
		return
	}
	a := beginAccountRequestAttempt(ctx, cache, account, model)
	if a == nil {
		return
	}
	a.started = time.Now().Add(-result.Duration)
	a.record.AttemptID = recentRequestText(result.AttemptID, 128)
	a.record.RequestID = recentRequestText(result.RequestID, 128)
	if result.Model != "" {
		a.record.Model = recentRequestText(result.Model, 256)
	}
	if result.FirstTokenMs != nil {
		ms := int64(*result.FirstTokenMs)
		a.record.FirstTokenMs = &ms
	}
	if result.Completed {
		a.observe(false, "completed")
	} else if result.ProviderHTTPFailure || len(result.ErrorEvent) > 0 || isOpenAICompatResponsesTerminalEvent(result.Terminal) {
		a.observe(false, "failed")
	}
	a.finish(err)
}

func (s *OpenAIGatewayService) vmKernelResponsesTTFTContext(ctx context.Context) context.Context {
	mode := s.openAITTFTMode(ctx)
	return vmkernel.WithFirstTokenPredicate(ctx, func(event vmkernel.Event) bool {
		return recentResponseStartsTTFT(string(event.Data), event.Type, mode)
	})
}

func (s *OpenAIGatewayService) vmKernelResponsesHeaderContext(ctx context.Context, c *gin.Context, account *Account, body []byte, turnState ...string) context.Context {
	headers := make(http.Header)
	if c != nil && c.Request != nil {
		for _, name := range vmkernel.CodexRequestHeaderNames {
			if value := c.GetHeader(name); value != "" {
				headers.Set(name, value)
			}
		}
	}
	applyCodexAccountIdentityHeaders(headers, codexAccountIdentitySource(c, account), getAPIKeyIDFromContext(c))
	// Prepared metadata is already account-scoped. Per-turn WS metadata takes
	// precedence over the connection handshake; never namespace it twice.
	for _, name := range []string{"x-codex-turn-metadata", "x-codex-installation-id", "x-codex-window-id"} {
		if value := gjson.GetBytes(body, "client_metadata."+name); value.Type == gjson.String && value.String() != "" {
			headers.Set(name, value.String())
		}
	}
	if value := gjson.GetBytes(body, "client_metadata."+openAICodexTurnStateHeader); value.Type == gjson.String && value.String() != "" {
		headers.Set(openAICodexTurnStateHeader, value.String())
	}
	s.guardVMKernelTurnState(c, account, body, headers)
	if len(turnState) > 0 && turnState[0] != "" {
		// Only state from this same session and generation-bound WS is supplied.
		// It is newer than the handshake or a repeated client payload value.
		headers.Set(openAICodexTurnStateHeader, turnState[0])
	}
	if session := vmKernelPreparedSession(body); session != "" {
		headers.Set("session-id", session)
		if raw := headers.Get(openAIWSTurnMetadataHeader); raw != "" {
			headers.Set(openAIWSTurnMetadataHeader, vmKernelScopeTurnMetadataSession(raw, session))
		}
		ctx = vmkernel.WithSessionID(ctx, session)
	}
	headers.Del(responsesLiteHeader)
	// Match the ordinary HTTP adapter's mapped GPT-5.5 Lite exception.
	if gjson.GetBytes(body, "model").String() != "gpt-5.5" &&
		((c != nil && isOpenAIResponsesLiteHeader(c.GetHeader(responsesLiteHeader))) || isOpenAIResponsesLiteWebSocketPayload(body)) {
		headers.Set(responsesLiteHeader, "true")
	}
	return vmkernel.WithHeaders(ctx, headers)
}

func (s *GatewayService) forwardVMKernelMessages(ctx context.Context, c *gin.Context, account *Account, body []byte, ingress string, parsed *ParsedRequest) (*ForwardResult, error) {
	if err := validateVMKernelIngress(account, body); err != nil {
		writeVMKernelRequestError(c, ingress, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, err
	}
	original := gjson.GetBytes(body, "model").String()
	stream := gjson.GetBytes(body, "stream").Bool()
	if !account.IsModelSupported(original) {
		return nil, fmt.Errorf("VM model is not allowed for this account")
	}
	model := claude.NormalizeModelID(account.GetMappedModel(original))
	prepared, err := sjson.SetBytes(body, "model", model)
	if err != nil {
		return nil, err
	}
	prepared, err = vmKernelConvertRequest(s.cfg, ingress, "messages", prepared)
	if err != nil {
		return nil, err
	}
	if err = validateClaude55Request(prepared, model); err != nil {
		return nil, err
	}
	policy := s.evaluateBetaPolicy(ctx, c.GetHeader("anthropic-beta"), account, model)
	if policy.blockErr != nil {
		return nil, policy.blockErr
	}
	// Use only the filtered protocol header, never client Authorization or internal tokens.
	betas := make([]string, 0)
	for _, beta := range strings.Split(c.GetHeader("anthropic-beta"), ",") {
		beta = strings.TrimSpace(beta)
		if beta == "" {
			continue
		}
		if _, filtered := policy.filterSet[beta]; !filtered {
			betas = append(betas, beta)
		}
	}
	headers := make(http.Header)
	if len(betas) > 0 {
		headers.Set("anthropic-beta", strings.Join(betas, ","))
	}
	prepared, err = sjson.SetBytes(prepared, "betas", betas)
	if err != nil {
		return nil, err
	}
	ctx = vmkernel.WithHeaders(ctx, headers)
	if next, changed := s.normalizeClientDatelineIfEnabled(ctx, account, prepared); changed {
		prepared = next
	}
	prepared = enforceCacheControlLimit(prepared)
	if s.shouldInjectAnthropicCacheTTL1h(ctx, account) {
		prepared = injectAnthropicCacheControlTTL1h(prepared)
	}
	if parsed != nil && parsed.OnUpstreamAccepted != nil {
		ctx = vmkernel.WithOnAccepted(ctx, parsed.OnUpstreamAccepted)
	}
	result, callErr := vmKernelHTTP(ctx, s.vmKernel, c, account, ingress, "messages", prepared, stream, original)
	callErr = vmKernelBusinessError(ctx, result, callErr)
	if callErr != nil {
		reportVMKernelProviderError(c, account, ingress, result)
	}
	if result == nil {
		return nil, callErr
	}
	out := &ForwardResult{RequestID: result.ResponseID, UpstreamHeaders: result.Headers, Model: original, UpstreamModel: model,
		UpstreamResponseModel: result.Model, Stream: stream, Duration: result.Duration, FirstTokenMs: result.FirstTokenMs, ClientDisconnect: result.ClientDisconnect,
		ReasoningEffort:            NormalizeClaudeOutputEffort(gjson.GetBytes(prepared, "output_config.effort").String()),
		VMKernelUsageAuthoritative: result.HasUsage && result.ResponseID != ""}
	if result.HasUsage {
		if err := json.Unmarshal(result.Usage, &out.Usage); err != nil {
			return nil, fmt.Errorf("invalid authoritative VM usage: %w", err)
		}
		out.Usage.CacheCreation5mTokens = int(gjson.GetBytes(result.Usage, "cache_creation.ephemeral_5m_input_tokens").Int())
		out.Usage.CacheCreation1hTokens = int(gjson.GetBytes(result.Usage, "cache_creation.ephemeral_1h_input_tokens").Int())
	}
	out.ReasoningEffort = ApplyThinkingEnabledFallback(out.ReasoningEffort, prepared, model)
	out.UpstreamResponseServiceTier = gjson.GetBytes(result.Usage, "speed").String()
	out.ServiceTier = anthropicSpeedServiceTier(account, gjson.GetBytes(body, "speed").String(), model)
	if callErr != nil && !out.VMKernelUsageAuthoritative {
		return nil, callErr
	}
	return out, callErr
}

func (s *OpenAIGatewayService) prepareVMKernelResponses(ctx context.Context, c *gin.Context, account *Account, body []byte, ingress, defaultMappedModel string) ([]byte, string, string, error) {
	cacheSeed := vmKernelPromptCacheSeed(c, body, "")
	beginUpstreamResponseModelObservation(c)
	clearOpenAIResponsesNamespaceNames(c)
	setCodexToolNameReverse(c, nil)
	if _, err := s.prepareCodexAccountIdentitySource(ctx, c, account); err != nil {
		return nil, "", "", err
	}
	if c != nil && c.Request != nil && c.Request.URL != nil {
		path := strings.TrimRight(c.Request.URL.Path, "/")
		if strings.HasSuffix(path, "/compact") || strings.Contains(path, "/images/") || strings.HasSuffix(path, "/count_tokens") {
			return nil, "", "", fmt.Errorf("VM endpoint is unsupported")
		}
	}
	if err := validateVMKernelIngress(account, body); err != nil {
		return nil, "", "", err
	}
	// Continuations are expanded and authorized before this shared preparation.
	if gjson.GetBytes(body, "previous_response_id").String() != "" {
		return nil, "", "", fmt.Errorf("VM continuation must be resolved before execution")
	}
	original := gjson.GetBytes(body, "model").String()
	billing := resolveOpenAIForwardModel(account, original, defaultMappedModel)
	model := normalizeOpenAIModelForUpstream(account, billing)
	if err := validateGPT61SolCompatRequest(body, model); err != nil {
		return nil, "", "", err
	}
	restriction := s.detectCodexClientRestriction(c, account, body)
	if restriction.Enabled && !restriction.Matched {
		MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalPolicyDenied)
		return nil, "", "", &vmKernelRequestPolicyError{Status: http.StatusForbidden, Type: "forbidden_error", Message: CodexClientRestrictionMessage(restriction)}
	}
	var err error
	body, err = filterOpenAIResponsesNoneReasoningEffortForAccount(account, body)
	if err != nil {
		return nil, "", "", err
	}
	if sanitized, changed, err := sanitizeOpenAIResponsesToolSchemasForPlatform(body, account.Platform); err != nil {
		return nil, "", "", err
	} else if changed {
		body = sanitized
	}
	prepared, err := sjson.SetBytes(body, "model", model)
	if err != nil {
		return nil, "", "", err
	}
	// Responses-shaped input on the Chat endpoint is already canonical input.
	convertIngress := ingress
	if ingress == "chat" && gjson.GetBytes(body, "input").Exists() && !gjson.GetBytes(body, "messages").Exists() {
		convertIngress = "responses"
	}
	prepared, err = vmKernelConvertRequest(s.cfg, convertIngress, "responses", prepared)
	if err != nil {
		return nil, "", "", err
	}
	prepared, err = normalizeOpenAIResponsesBusinessBody(s.cfg, account, prepared, model)
	if err != nil {
		return nil, "", "", err
	}
	responsesLite := isOpenAIResponsesLiteHeader(c.GetHeader(responsesLiteHeader)) || isOpenAIResponsesLiteWebSocketPayload(prepared)
	if responsesLite {
		prepared, _, err = normalizeOpenAIResponsesLitePayloadForAccount(prepared, account)
		if err != nil {
			return nil, "", "", err
		}
	}
	// The kernel receives Responses HTTP, including for client WS requests.
	// Use the same namespace policy and opt-in flattening as ordinary HTTP.
	prepared, _, err = normalizeOpenAIResponsesNamespaceBody(c, account, prepared, OpenAIUpstreamTransportHTTPSSE, false, false)
	if err != nil {
		return nil, "", "", err
	}
	if normalized, changed, err := normalizeOpenAIResponsesReasoningContentReplay(prepared); err != nil {
		return nil, "", "", err
	} else if changed {
		prepared = normalized
	}
	if normalized, changed, err := sanitizeOpenAIResponsesInputItemIDs(prepared); err != nil {
		return nil, "", "", err
	} else if changed {
		prepared = normalized
	}
	var requestMap map[string]any
	if err = decodeOpenAIJSONUseNumber(prepared, &requestMap); err != nil {
		return nil, "", "", err
	}
	normalizeOpenAIResponseFormatSchemas(requestMap)
	sanitizeEmptyBase64InputImagesInOpenAIRequestBodyMap(requestMap)
	transformed := applyCodexOAuthTransformWithOptions(requestMap, codexOAuthTransformOptions{
		IsCodexCLI:                          openai.IsCodexOfficialClientByHeaders(c.GetHeader("User-Agent"), c.GetHeader("originator")) || (s.cfg != nil && s.cfg.Gateway.ForceCodexCLI),
		PreserveToolCallIDs:                 ingress == "messages",
		OmitPromotedSystemMessagesFromInput: !strings.EqualFold(gjson.GetBytes(prepared, "text.format.type").String(), "json_object"),
		ResponsesLite:                       responsesLite,
	})
	if transformed.Error != nil {
		return nil, "", "", transformed.Error
	}
	// Match ordinary Codex forwarding after call IDs have been normalized.
	// HTTP and WS continuations already contain their expanded history here.
	if input, ok := requestMap["input"].([]any); ok {
		sanitizeOpenAIResponsesOrphanToolOutputs(requestMap, input, false)
	}
	setCodexToolNameReverse(c, transformed.ToolNameReverse)
	if transformed.NormalizedModel != "" {
		model = transformed.NormalizedModel
	}
	applyCodexClientMetadata(requestMap, account)
	applyCodexAccountIdentityClientMetadataMap(requestMap, codexAccountIdentitySource(c, account), getAPIKeyIDFromContext(c))
	applyVMKernelPromptCacheKey(c, account, requestMap, cacheSeed)
	prepared, err = json.Marshal(requestMap)
	if err != nil {
		return nil, "", "", err
	}
	if model == "gpt-5.5" && isOpenAIResponsesLiteWebSocketPayload(prepared) {
		prepared, err = sjson.DeleteBytes(prepared, "client_metadata."+responsesLiteWSMetadataKey)
		if err != nil {
			return nil, "", "", err
		}
	}
	frame, err := sjson.SetBytes(prepared, "type", "response.create")
	if err != nil {
		return nil, "", "", err
	}
	frame, blocked, err := s.applyOpenAIFastPolicyToWSResponseCreate(ctx, account, model, frame)
	if err != nil {
		return nil, "", "", err
	}
	if blocked != nil {
		MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalPolicyDenied)
		return nil, "", "", blocked
	}
	prepared, err = sjson.DeleteBytes(frame, "type")
	if err != nil {
		return nil, "", "", err
	}
	prepared, err = sjson.SetBytes(prepared, "stream", true)
	if err != nil {
		return nil, "", "", err
	}
	if !gjson.GetBytes(prepared, "instructions").Exists() {
		prepared, err = sjson.SetBytes(prepared, "instructions", defaultCodexSynthInstructions(model))
	}
	SetActualOpenAIUpstreamEndpoint(c, openAIResponsesUpstreamEndpoint)
	return prepared, billing, model, err
}

func vmKernelOpenAIResult(result *vmkernel.Result, body []byte, original, billing, model string, stream bool) (*OpenAIForwardResult, error) {
	if result == nil {
		return nil, nil
	}
	out := &OpenAIForwardResult{RequestID: result.ResponseID, ResponseID: result.ResponseID, UpstreamHeaders: result.Headers, ResponseHeaders: result.Headers,
		Model: original, BillingModel: billing, UpstreamModel: model, UpstreamResponseModel: result.Model, UpstreamEndpoint: openAIResponsesUpstreamEndpoint,
		Stream: stream, Duration: result.Duration, FirstTokenMs: result.FirstTokenMs, ClientDisconnect: result.ClientDisconnect,
		UpstreamTerminalEvent: result.Terminal, UsageUnavailable: !result.HasUsage,
		VMKernelUsageAuthoritative: result.HasUsage && result.ResponseID != "",
		ServiceTier:                vmKernelStringPointer(gjson.GetBytes(body, "service_tier").String()), ReasoningEffort: vmKernelStringPointer(gjson.GetBytes(body, "reasoning.effort").String()),
		streamReadIncomplete: !result.Completed}
	if result.RequestID != "" {
		out.RequestID = result.RequestID
	}
	out.UpstreamResponseServiceTier = gjson.GetBytes(result.Response, "service_tier").String()
	if result.HasUsage {
		if err := json.Unmarshal(result.Usage, &out.Usage); err != nil {
			return nil, err
		}
		if cached := gjson.GetBytes(result.Usage, "input_tokens_details.cached_tokens"); cached.Exists() {
			out.Usage.CacheReadInputTokens = int(cached.Int())
		}
	}
	return out, nil
}

func (s *OpenAIGatewayService) forwardVMKernelResponses(ctx context.Context, c *gin.Context, account *Account, body []byte, ingress, defaultMappedModel string) (*OpenAIForwardResult, error) {
	original := gjson.GetBytes(body, "model").String()
	stream := gjson.GetBytes(body, "stream").Bool()
	var err error
	ctx, body, err = s.prepareVMKernelContinuation(ctx, c, account, body)
	if err != nil {
		writeVMKernelRequestError(c, ingress, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, err
	}
	prepared, billing, model, err := s.prepareVMKernelResponses(ctx, c, account, body, ingress, defaultMappedModel)
	if err != nil {
		reportVMKernelPreparationError(c, ingress, err)
		return nil, err
	}
	result, callErr := vmKernelHTTP(ctx, s.vmKernel, c, account, ingress, "responses", prepared, stream, original, s)
	s.observeVMKernelOpenAIResult(ctx, c, account, model, result)
	callErr = vmKernelBusinessError(ctx, result, callErr)
	if callErr != nil && !reportVMKernelIdleTimeout(c, ingress, callErr, result) {
		reportVMKernelProviderError(c, account, ingress, result)
	}
	if callErr == nil && result != nil && result.Completed && ingress == "responses" {
		// History is auxiliary: losing its cache must never retry or lose billing
		// for a request that has already completed.
		_ = s.rememberVMKernelContinuation(ctx, c, account, vmKernelReplayCacheBody(c, account, body, model), result.Response)
	}
	out, err := vmKernelOpenAIResult(result, prepared, original, billing, model, stream)
	if err != nil {
		return nil, err
	}
	if out != nil && callErr != nil {
		out.streamReadIncomplete = true
	}
	if callErr != nil && (out == nil || !out.VMKernelUsageAuthoritative) {
		return nil, callErr
	}
	return out, callErr
}
