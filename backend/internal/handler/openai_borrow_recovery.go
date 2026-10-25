package handler

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

func isOpenAIModelFailover(last *service.UpstreamFailoverError) bool {
	return last != nil && last.StatusCode == http.StatusBadRequest && service.IsOpenAICompatibleModelNotFound400(last.ResponseBody)
}

func openAIModelFailoverSelectionContext(ctx context.Context, last *service.UpstreamFailoverError) context.Context {
	if isOpenAIModelFailover(last) {
		return service.WithoutBorrowRouteWait(ctx)
	}
	return ctx
}

// handleBorrowBusinessRecovery runs after the existing output/replay checks.
// handled prevents the legacy same-account/OAuth retry loops from also running.
func (h *OpenAIGatewayHandler) handleBorrowBusinessRecovery(c *gin.Context, failure *service.UpstreamFailoverError, account *service.Account, excluded map[int64]struct{}, switches *int, maxSwitches int, stream, anthropic bool) (handled, retry bool) {
	r := service.BorrowBusinessRecoveryFromContext(c.Request.Context())
	if !r.Active() {
		return false, false
	}
	defer service.RefreshBorrowRecoveryError(c)
	finish := func() (bool, bool) {
		service.RefreshBorrowRecoveryError(c)
		if !h.stopBorrowBusinessRecovery(c, stream, anthropic) && !h.finishBorrowCapacityFailure(c, failure, stream, anthropic) {
			if anthropic {
				h.handleAnthropicFailoverExhausted(c, failure, stream || c.Writer.Written())
			} else {
				h.handleFailoverExhausted(c, failure, stream || c.Writer.Written())
			}
		}
		return true, false
	}
	canSame := failure.RetryableOnSameAccount || r.MayRetryBeforeWrite()
	if (!failure.ShouldRetryNextAccount() && !canSame) || (r.Pinned() && !canSame) || (!canSame && *switches >= maxSwitches) {
		return finish()
	}
	if !r.ReserveRecovery(c.Request.Context(), failure.StatusCode) {
		return finish()
	}
	ctx := service.BorrowBusinessRecoveryContext(c.Request.Context())
	c.Request = c.Request.WithContext(ctx)
	// Defer waiting until the selected account is known. The transport boundary
	// enforces the original wait if selection falls back to this account, while
	// an independent ordinary provider can proceed after only the short backoff.
	r.SetRecoveryDelay(account, failure, borrowRecoveryDelay(failure, time.Now()))
	if failure.ShouldRetryNextAccount() && !r.Pinned() && *switches < maxSwitches {
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
	logger.FromContext(c.Request.Context()).Info("borrow.business_recovery", zap.String("action", "same_target_reselect_route"), zap.Int64("account_id", account.ID), zap.Int("upstream_status", failure.StatusCode), zap.Int("max_attempts", 2))
	return true, true
}

func borrowBusinessSelectionFallback(c *gin.Context, err error, excluded map[int64]struct{}) bool {
	if !errors.Is(err, service.ErrNoAvailableAccounts) && !service.IsBorrowRouteUnavailable(err) {
		return false
	}
	r := service.BorrowBusinessRecoveryFromContext(c.Request.Context())
	if id := r.TakeFallback(); id != 0 && r.FallbackAvailable(c.Request.Context(), id) {
		delete(excluded, id)
		logger.FromContext(c.Request.Context()).Info("borrow.business_recovery", zap.String("action", "fallback_same_target_reselect_route"), zap.Int64("account_id", id))
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
	const message = "Upstream recovery timed out before send"
	if anthropic {
		h.anthropicStreamingAwareError(c, http.StatusGatewayTimeout, "api_error", message, stream || c.Writer.Written())
	} else {
		h.handleStreamingAwareErrorWithCode(c, http.StatusGatewayTimeout, "api_error", "borrow_recovery_timeout", message, stream || c.Writer.Written(), false)
	}
	return true
}

// Honor Retry-After without truncating a long upstream wait to an unsafe retry.
// The request recovery deadline stops the wait when it exceeds our budget.
func borrowRecoveryDelay(f *service.UpstreamFailoverError, now time.Time) time.Duration {
	delay := 500 * time.Millisecond
	if f.SameAccountRetryDelay > delay {
		delay = f.SameAccountRetryDelay
	}
	raw := strings.TrimSpace(f.ResponseHeaders.Get("Retry-After"))
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds > 0 {
		if seconds > 86400 {
			seconds = 86400
		}
		if d := time.Duration(seconds) * time.Second; d > delay {
			delay = d
		}
	} else if at, err := http.ParseTime(raw); err == nil && at.Sub(now) > delay {
		delay = at.Sub(now)
	}
	return delay + time.Duration(rand.IntN(251))*time.Millisecond
}

// Resource exhaustion after bounded recovery is distinct from client rate limits.
// A request that has never sent upstream still uses normal admission errors.
func (h *OpenAIGatewayHandler) finishBorrowSelectionFailure(c *gin.Context, err error, last *service.UpstreamFailoverError, stream, anthropic bool) bool {
	r := service.BorrowBusinessRecoveryFromContext(c.Request.Context())
	if last == nil || (!r.Active() && !isOpenAIModelFailover(last)) ||
		(!errors.Is(err, service.ErrNoAvailableAccounts) && !service.IsBorrowRouteUnavailable(err)) || failoverClientGone(c) {
		return false
	}
	r.SelectionStopped(c.Request.Context())
	service.RefreshBorrowRecoveryError(c)
	if h.finishBorrowCapacityFailure(c, last, stream, anthropic) {
		return true
	}
	if anthropic {
		h.handleAnthropicFailoverExhausted(c, last, stream)
	} else {
		h.handleFailoverExhausted(c, last, stream)
	}
	return true
}

// The recovery wait context is internal. Restore client cancellation semantics
// before outer timing/error middleware inspect the completed logical request.
func (h *OpenAIGatewayHandler) beginBorrowBusinessRequest(c *gin.Context) func() {
	ctx, r := service.WithBorrowBusinessRecovery(c.Request.Context(), h.cfg)
	c.Request = c.Request.WithContext(ctx)
	return func() {
		finalContext := service.BorrowBusinessClientContext(c.Request.Context())
		r.Close()
		service.RefreshBorrowRecoveryError(c)
		c.Request = c.Request.WithContext(finalContext)
	}
}

func (h *OpenAIGatewayHandler) finishBorrowCapacityFailure(c *gin.Context, failure *service.UpstreamFailoverError, stream, anthropic bool) bool {
	if failure == nil || failure.StatusCode != http.StatusTooManyRequests || service.IsResponseCommitted(c) {
		return false
	}
	const message = "No upstream capacity is currently available. Please retry shortly."
	if anthropic {
		h.anthropicStreamingAwareError(c, http.StatusServiceUnavailable, "overloaded_error", message, stream || c.Writer.Written())
	} else {
		h.handleStreamingAwareErrorWithCode(c, http.StatusServiceUnavailable, "api_error", "upstream_capacity_unavailable", message, stream || c.Writer.Written(), false)
	}
	return true
}
