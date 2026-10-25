package handler

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// The forward call has already released its concurrency slot. Reselect only
// after an explicitly marked initial admission rejection, never based on the
// error text of a later send. Written heartbeats are conservatively terminal too.
func retryOpenAIInitialAdmission(c *gin.Context, err error, result *service.OpenAIForwardResult, body []byte, accountID int64, excluded map[int64]struct{}, switches *int, maxSwitches int) bool {
	if !service.IsOpenAIInitialAdmissionRejection(err) || result != nil ||
		c.Request.Context().Err() != nil || c.Writer.Written() {
		return false
	}
	// These requests may depend on upstream state held by the selected account.
	if !service.OpenAIInitialAdmissionCanReselect(c.Request.Context(), body) {
		return false
	}
	recovery := service.BorrowBusinessRecoveryFromContext(c.Request.Context())
	// Revisiting a temporarily full route consumes the existing admission wait,
	// not another account switch or upstream-send attempt.
	if recovery.HasAdmissionRetry(accountID) && recovery.RememberAdmissionRetry(accountID, err) {
		return true
	}
	if *switches >= maxSwitches {
		return false
	}
	if _, alreadyExcluded := excluded[accountID]; alreadyExcluded {
		return false
	}
	excluded[accountID] = struct{}{}
	*switches++
	recovery.RememberAdmissionRetry(accountID, err)
	return true
}

// handleTurnAdmissionError retains model-capability attribution when a final
// pre-send check rejects an account after model mapping.
func (h *OpenAIGatewayHandler) handleTurnAdmissionError(c *gin.Context, err error, streamStarted bool) {
	if service.IsOpenAIModelCapabilityRejection(err) {
		service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalModelConfiguration)
		h.handleStreamingAwareError(c, http.StatusNotFound, "model_not_found", "The requested model is not supported by the selected account", streamStarted)
		return
	}
	status, code, message := openAIAdmissionClientError(err)
	h.handleStreamingAwareError(c, status, code, message, streamStarted)
}
