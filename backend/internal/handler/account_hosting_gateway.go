package handler

import (
	"context"
	"errors"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// Called only at selection failures reached through the existing safe replay loop.
func (h *OpenAIGatewayHandler) tryHostedFallback(c *gin.Context, ctx context.Context, selectionErr error, model string, stream, started, websocket bool) (bool, error) {
	key, ok := middleware.GetAPIKeyFromContext(c)
	if !ok || key.RouteMode != service.RouteModeHosted || key.HostingFallback {
		return false, nil
	}
	if !errors.Is(selectionErr, service.ErrNoAvailableAccounts) && !errors.Is(selectionErr, service.ErrNoAvailableCompactAccounts) && !errors.Is(selectionErr, service.ErrOpenAIRPMExhausted) {
		return false, nil
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if websocket {
		if !service.OpenAIWSIngressCanFailover(ctx, c) {
			return false, nil
		}
	} else if started || c.Writer.Written() {
		return false, nil
	}
	if err := h.apiKeyService.SwitchHostedFallback(ctx); err != nil {
		return false, err
	}
	if !key.Group.ModelAllowlist.Allows(model) {
		return false, infraerrors.NotFound("MODEL_NOT_ALLOWED", "兜底分组不支持请求的模型")
	}
	denied, err := h.apiKeyService.GetUserGroupDeniedModels(c.Request.Context(), key.UserID)
	if err != nil {
		return false, err
	}
	key.User.UserGroupDeniedModels = denied[key.Group.ID]
	if service.FirstUserGroupDeniedModel(key.User.UserGroupDeniedModels, []string{model}) != "" {
		return false, infraerrors.NotFound("MODEL_NOT_ALLOWED", "该模型对当前用户不可用")
	}
	if key.Group.StreamOnly && !stream {
		return false, infraerrors.BadRequest("STREAM_REQUIRED", "兜底分组仅允许流式请求")
	}
	if key.Group.ClaudeCodeOnly && !isMessagesRequest(c) {
		return false, infraerrors.Forbidden("CLAUDE_CODE_ONLY", "兜底分组仅允许 Claude Code /v1/messages 调用")
	}
	if isMessagesRequest(c) && !key.Group.AllowMessagesDispatch {
		return false, infraerrors.Forbidden("MESSAGES_DISPATCH_NOT_ALLOWED", "兜底分组未开放 /v1/messages 调用")
	}
	if permissions := service.APIKeyQueueRequestPermissionsFromContext(ctx); permissions.Capabilities != 0 {
		if permissions.Capabilities&service.APIKeyQueueCapabilityImageGeneration != 0 && !service.GroupAllowsImageGeneration(key.Group) {
			return false, infraerrors.Forbidden("IMAGE_GENERATION_NOT_ALLOWED", service.ImageGenerationPermissionMessage())
		}
	}
	// The primary attempt already consumed the user RPM admission. Check the
	// fallback's group RPM and monetary limits without counting the user twice.
	if err := h.billingCacheService.CheckHostingFallbackEligibility(ctx, key.User, key, key.Group); err != nil {
		return false, err
	}
	ctx = context.WithValue(ctx, ctxkey.Group, key.Group)
	pricingCtx, _ := h.gatewayService.WithOpenAIRequestPricingContext(ctx, key.GroupID)
	c.Request = c.Request.WithContext(pricingCtx)
	return true, nil
}

// Exhausting the private pool's retry budget enters the same safe fallback
// branch as an empty pool, without acquiring another private account slot.
func (h *OpenAIGatewayHandler) selectHostedAccount(key *service.APIKey, switches, maxSwitches int, ctx context.Context, groupID *int64, previousResponseID, sessionHash, model string, excluded map[int64]struct{}, transport service.OpenAIUpstreamTransport, capability service.OpenAIEndpointCapability, compact, previousCanMove, tokenCost bool, platform ...string) (*service.AccountSelectionResult, service.OpenAIAccountScheduleDecision, error) {
	if canTryHostedFallback(key) && switches > maxSwitches {
		return nil, service.OpenAIAccountScheduleDecision{}, service.ErrNoAvailableAccounts
	}
	return h.gatewayService.SelectAccountWithSchedulerForCapability(ctx, groupID, previousResponseID, sessionHash, model, excluded, transport, capability, compact, previousCanMove, tokenCost, platform...)
}

func isMessagesRequest(c *gin.Context) bool {
	return c != nil && c.Request != nil && strings.HasSuffix(strings.TrimRight(c.Request.URL.Path, "/"), "/messages")
}

func hostingGatewayError(err error) (int, string, string) {
	return infraerrors.Code(err), infraerrors.Reason(err), infraerrors.Message(err)
}

func canTryHostedFallback(key *service.APIKey) bool {
	return key != nil && key.RouteMode == service.RouteModeHosted && !key.HostingFallback && key.HostingFallbackGroupID != nil
}

// Return true when normal failover must stop. Hosted primary exhaustion is
// converted to selection failure on the next safe iteration.
func markHostedPoolExhausted(key *service.APIKey, switches *int, max int) bool {
	if canTryHostedFallback(key) {
		*switches = max + 1
		return false
	}
	return true
}

func snapshotHostedKey(key *service.APIKey) *service.APIKey {
	if key == nil || key.RouteMode != service.RouteModeHosted {
		return key
	}
	clone := *key
	if key.HostingRate != nil {
		rate := *key.HostingRate
		clone.HostingRate = &rate
	}
	return &clone
}

func shouldRecordHostedPartialUsage(key *service.APIKey, result *service.OpenAIForwardResult) bool {
	return key != nil && key.RouteMode == service.RouteModeHosted && result != nil && !result.UsageUnavailable &&
		(result.Usage.InputTokens > 0 || result.Usage.OutputTokens > 0 || result.Usage.CacheCreationInputTokens > 0 || result.Usage.CacheReadInputTokens > 0)
}
