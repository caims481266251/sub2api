package admin

import (
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *AccountHandler) BorrowCandidatePool(c *gin.Context) {
	s := h.borrowService(c)
	if s == nil {
		return
	}
	q := service.BorrowCandidatePoolQuery{State: c.Query("state"), Search: c.Query("search")}
	for key, dest := range map[string]*int64{"source_account_id": &q.SourceAccountID, "target_id": &q.TargetID} {
		if raw := c.Query(key); raw != "" {
			value, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				borrowError(c, service.ErrBorrowPoolQuery)
				return
			}
			*dest = value
		}
	}
	for key, dest := range map[string]*int{"page": &q.Page, "page_size": &q.PageSize} {
		if raw := c.Query(key); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value < 1 {
				borrowError(c, service.ErrBorrowPoolQuery)
				return
			}
			*dest = value
		}
	}
	v, err := s.CandidatePoolPage(c.Request.Context(), q)
	if err != nil {
		borrowError(c, err)
		return
	}
	response.Success(c, v)
}

func (h *AccountHandler) BorrowCandidatePoolDetail(c *gin.Context) {
	s := h.borrowService(c)
	if s == nil {
		return
	}
	v, err := s.CandidatePoolDetail(c.Request.Context(), c.Param("candidateID"))
	if err != nil {
		borrowError(c, err)
		return
	}
	response.Success(c, v)
}

func (h *AccountHandler) BorrowCandidatePoolConfig(c *gin.Context) {
	s := h.borrowService(c)
	if s == nil {
		return
	}
	var input service.BorrowCandidatePoolConfig
	if c.ShouldBindJSON(&input) != nil {
		borrowError(c, service.ErrBorrowPoolCapacity)
		return
	}
	v, err := s.SaveCandidatePoolConfig(c.Request.Context(), input)
	if err != nil {
		borrowError(c, err)
		return
	}
	response.Success(c, v)
}
