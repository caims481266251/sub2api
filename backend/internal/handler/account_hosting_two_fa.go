package handler

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *HostingHandler) StartTwoFALogin(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	userID, ok := hostingUser(c)
	if !ok {
		return
	}
	var input struct {
		service.AccountTokenGuardReloginAccount
		ReplaceID int64  `json:"replace_id"`
		PoolID    *int64 `json:"hosting_pool_id"`
	}
	if !decodeHostingRequest(c, &input, 16<<10, "登录凭据格式不正确") {
		return
	}
	job, err := h.hosting.StartTwoFALogin(c.Request.Context(), userID, input.ReplaceID, input.PoolID, input.AccountTokenGuardReloginAccount)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Accepted(c, job)
}

func (h *HostingHandler) TwoFALogin(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	userID, ok := hostingUser(c)
	if !ok {
		return
	}
	job, err := h.hosting.TwoFALogin(c.Request.Context(), userID, c.Param("id"))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, job)
}

func (h *HostingHandler) DeleteTwoFALogin(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	userID, ok := hostingUser(c)
	if !ok {
		return
	}
	if err := h.hosting.DeleteTwoFALogin(userID, c.Param("id")); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"deleted": true})
}

func (h *HostingHandler) CompleteTwoFALogin(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	userID, ok := hostingUser(c)
	if !ok {
		return
	}
	var input struct {
		Settings *service.HostingAccountSettings `json:"settings"`
	}
	if !decodeHostingRequest(c, &input, 128<<10, "账号配置格式不正确") {
		return
	}
	a, err := h.hosting.CompleteTwoFALogin(c.Request.Context(), userID, c.Param("id"), input.Settings)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, hostedAccountView(a))
}
