package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *AccountHandler) BorrowSaveProbePolicies(c *gin.Context) {
	if _, observer := service.ObserverGroupIDs(c.Request.Context()); observer {
		response.Forbidden(c, "Administrator access required")
		return
	}
	s := h.borrowService(c)
	if s == nil {
		return
	}
	var input service.BorrowProbeBatchInput
	if c.ShouldBindJSON(&input) != nil {
		borrowError(c, service.ErrBorrowProbePolicyInvalid)
		return
	}
	results, err := s.SaveProbePolicies(c.Request.Context(), input)
	if err != nil {
		borrowError(c, err)
		return
	}
	response.Success(c, results)
}
