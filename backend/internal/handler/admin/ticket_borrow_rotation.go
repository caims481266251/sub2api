package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *AccountHandler) BorrowRotations(c *gin.Context) {
	if _, observer := service.ObserverGroupIDs(c.Request.Context()); observer {
		response.Forbidden(c, "Administrator access required")
		return
	}
	s := h.borrowService(c)
	if s == nil {
		return
	}
	if c.Request.Method == "POST" {
		var in service.BorrowRotationInput
		if c.ShouldBindJSON(&in) != nil {
			borrowError(c, service.ErrBorrowRotationInvalid)
			return
		}
		job, err := s.Rotate(c.Request.Context(), in)
		if err != nil {
			borrowError(c, err)
			return
		}
		response.Success(c, job)
		return
	}
	state, err := s.RotationStates(c.Request.Context())
	if err != nil {
		borrowError(c, err)
		return
	}
	response.Success(c, state)
}
func (h *AccountHandler) BorrowRotationPolicy(c *gin.Context) {
	if _, observer := service.ObserverGroupIDs(c.Request.Context()); observer {
		response.Forbidden(c, "Administrator access required")
		return
	}
	s := h.borrowService(c)
	if s == nil {
		return
	}
	id, ok := borrowID(c, "id")
	if !ok {
		return
	}
	var in service.BorrowRotationPolicy
	if c.ShouldBindJSON(&in) != nil {
		borrowError(c, service.ErrBorrowRotationInvalid)
		return
	}
	if err := s.SaveRotationPolicy(c.Request.Context(), id, in); err != nil {
		borrowError(c, err)
		return
	}
	state, err := s.RotationStates(c.Request.Context())
	if err != nil {
		borrowError(c, err)
		return
	}
	response.Success(c, state)
}

func (h *AccountHandler) BorrowRotationPolicies(c *gin.Context) {
	if _, observer := service.ObserverGroupIDs(c.Request.Context()); observer {
		response.Forbidden(c, "Administrator access required")
		return
	}
	s := h.borrowService(c)
	if s == nil {
		return
	}
	var in service.BorrowRotationBatchInput
	if c.ShouldBindJSON(&in) != nil {
		borrowError(c, service.ErrBorrowRotationInvalid)
		return
	}
	results, err := s.SaveRotationPolicies(c.Request.Context(), in)
	if err != nil {
		borrowError(c, err)
		return
	}
	response.Success(c, results)
}
