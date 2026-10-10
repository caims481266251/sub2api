package handler

import (
	"errors"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// handleBorrowBusinessRecovery runs after the existing output/replay checks.
// handled prevents the legacy same-account/OAuth retry loops from also running.
func (h *OpenAIGatewayHandler) handleBorrowBusinessRecovery(c *gin.Context, failure *service.UpstreamFailoverError, account *service.Account, excluded map[int64]struct{}, switches *int, maxSwitches int, stream, anthropic bool) (handled, retry bool) {
	r := service.BorrowBusinessRecoveryFromContext(c.Request.Context())
	if !r.Active() {
		return false, false
	}
	finish := func() (bool, bool) {
		if !h.stopBorrowBusinessRecovery(c, stream, anthropic) {
			if anthropic {
				h.handleAnthropicFailoverExhausted(c, failure, stream || c.Writer.Written())
			} else {
				h.handleFailoverExhausted(c, failure, stream || c.Writer.Written())
			}
		}
		return true, false
	}
	if failure.StatusCode == http.StatusTooManyRequests {
		r.ReserveRecovery(c.Request.Context(), failure.StatusCode)
		return finish()
	}
	canSame := (failure.RetryableOnSameAccount && account.GetPoolModeRetryCount() > 0) || r.MayRetryBeforeWrite()
	if !failure.ShouldRetryNextAccount() || (r.Pinned() && !canSame) {
		return finish()
	}
	if !r.ReserveRecovery(c.Request.Context(), failure.StatusCode) {
		return finish()
	}
	if !r.Pinned() && *switches < maxSwitches {
		excluded[account.ID] = struct{}{}
		if canSame {
			r.SetFallback(account.ID)
		}
		*switches++
		h.gatewayService.RecordOpenAIAccountSwitch()
		logger.FromContext(c.Request.Context()).Info("borrow.business_recovery", zap.String("action", "select_other_target"), zap.Int64("account_id", account.ID), zap.Int("upstream_status", failure.StatusCode), zap.Int("max_attempts", 2))
		return true, true
	}
	if !canSame {
		return finish()
	}
	logger.FromContext(c.Request.Context()).Info("borrow.business_recovery", zap.String("action", "same_target_new_connection"), zap.Int64("account_id", account.ID), zap.Int("upstream_status", failure.StatusCode), zap.Int("max_attempts", 2))
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-service.BorrowBusinessRecoveryContext(c.Request.Context()).Done():
		return finish()
	case <-timer.C:
		return true, true
	}
}

func borrowBusinessSelectionFallback(c *gin.Context, err error, excluded map[int64]struct{}) bool {
	if !errors.Is(err, service.ErrNoAvailableAccounts) && !service.IsBorrowRouteUnavailable(err) {
		return false
	}
	r := service.BorrowBusinessRecoveryFromContext(c.Request.Context())
	if id := r.TakeFallback(); id != 0 {
		delete(excluded, id)
		logger.FromContext(c.Request.Context()).Info("borrow.business_recovery", zap.String("action", "fallback_same_target_new_connection"), zap.Int64("account_id", id))
		return true
	}
	return false
}

func (h *OpenAIGatewayHandler) stopBorrowBusinessRecovery(c *gin.Context, stream, anthropic bool) bool {
	r := service.BorrowBusinessRecoveryFromContext(c.Request.Context())
	if !errors.Is(r.Err(), service.ErrBorrowRecoveryTimeout) {
		return false
	}
	// Do not append SSE to an already committed JSON error or repeat a stream
	// error that the forwarding layer has already delivered.
	if service.IsResponseCommitted(c) || (c.Writer.Written() && c.Writer.Status() >= 400) {
		return true
	}
	const message = "Upstream recovery timed out before output"
	if anthropic {
		h.anthropicStreamingAwareError(c, http.StatusGatewayTimeout, "api_error", message, stream || c.Writer.Written())
	} else {
		h.handleStreamingAwareErrorWithCode(c, http.StatusGatewayTimeout, "api_error", "borrow_recovery_timeout", message, stream || c.Writer.Written(), false)
	}
	return true
}
