package handler

import (
	"context"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

const borrowHTTPFallbackOriginalAccountKey = "borrow_http_fallback_original_account"

// Runs only at selection failure or a proven first, pre-send rejection. A
// subsequent upstream failure permanently closes this migration opportunity.
func (h *OpenAIGatewayHandler) retryBorrowHTTPContinuation(c *gin.Context, err error, groupID *int64, body *[]byte, sessionHash *string, excluded map[int64]struct{}, switches *int, maxSwitches int, firstUnsent bool) bool {
	target, response, refusal := service.BorrowHTTPContinuationStatus(c.Request.Context())
	if target == 0 || !service.IsBorrowRouteUnavailable(err) {
		return false
	}
	log := logger.FromContext(c.Request.Context()).With(zap.Int64("original_account_id", target), zap.String("response_hash", response), zap.String("admission_reason", service.BorrowAdmissionReason(err)))
	if !firstUnsent || c.Request.Context().Err() != nil || c.Writer.Written() || service.IsResponseCommitted(c) || *switches >= maxSwitches {
		refusal = "request_or_switch_budget_closed"
	} else if refusal == "" {
		var next *http.Request
		var effective []byte
		next, effective, refusal = h.gatewayService.MoveBorrowHTTPContinuation(c.Request, groupID, *body)
		if next != nil {
			c.Request = next.WithContext(context.WithValue(next.Context(), ctxkey.AccountID, int64(0)))
			c.Set(opsAccountIDKey, int64(0))
			*body = effective
			*sessionHash = h.gatewayService.GenerateSessionHash(c, effective)
			excluded[target] = struct{}{}
			*switches++
			h.gatewayService.RecordOpenAIAccountSwitch()
			c.Set(borrowHTTPFallbackOriginalAccountKey, target)
			log.Info("openai.borrow_http_continuation_fallback", zap.String("action", "select_same_group"), zap.Int("switch_count", *switches), zap.Bool("upstream_write_possible", false))
			return true
		}
	}
	log.Info("openai.borrow_http_continuation_fallback_refused", zap.String("reason", refusal), zap.Int("switch_count", *switches))
	return false
}

func (h *OpenAIGatewayHandler) finishBorrowHTTPContinuationFailure(c *gin.Context, err error, stream bool) bool {
	target, _, _ := service.BorrowHTTPContinuationStatus(c.Request.Context())
	if target == 0 || c.Request.Context().Err() != nil || service.IsResponseCommitted(c) || c.Writer.Written() {
		return false
	}
	if !service.IsBorrowRouteUnavailable(err) {
		if !service.IsOpenAITurnAdmissionError(err) {
			return false
		}
		logger.FromContext(c.Request.Context()).Warn("openai.borrow_http_continuation_admission_failed", zap.Int64("original_account_id", target), zap.String("admission_reason", service.BorrowAdmissionReason(err)), zap.Error(err))
		status, code, message := openAIAdmissionClientError(err)
		h.handleStreamingAwareError(c, status, code, message, stream)
		return true
	}
	markOpsRoutingCapacityLimited(c)
	h.handleStreamingAwareErrorWithCode(c, http.StatusServiceUnavailable, "api_error", "borrow_continuation_unavailable", "The original continuation is temporarily unavailable and could not be moved safely. Retry later or start a new request with complete conversation context.", stream, false)
	return true
}

func logBorrowHTTPContinuationOutcome(c *gin.Context, switches *int) {
	if original := c.GetInt64(borrowHTTPFallbackOriginalAccountKey); original > 0 {
		logger.FromContext(c.Request.Context()).Info("openai.borrow_http_continuation_outcome",
			zap.Int64("original_account_id", original), zap.Int64("final_account_id", c.GetInt64(opsAccountIDKey)),
			zap.Int("switch_count", *switches), zap.Int("http_status", c.Writer.Status()))
	}
}
