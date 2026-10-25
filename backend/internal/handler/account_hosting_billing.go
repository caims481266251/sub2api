package handler

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/gin-gonic/gin"
	"strconv"
)

func (h *HostingHandler) Billing(c *gin.Context)             { h.hostingBilling(c, false) }
func (h *HostingHandler) AdminAccountBilling(c *gin.Context) { h.hostingBilling(c, true) }
func (h *HostingHandler) hostingBilling(c *gin.Context, admin bool) {
	userID, ok := hostingUser(c)
	if !ok {
		return
	}
	accountID := int64(0)
	if c.Param("id") != "" {
		account, allowed := h.quotaAccount(c, admin)
		if !allowed {
			return
		}
		accountID = account.ID
		userID = *account.OwnerUserID
	}
	page, e1 := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, e2 := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	route := c.Query("route")
	if e1 != nil || e2 != nil || page < 1 || page > 1000000 || size < 1 || size > 100 || (route != "" && route != "hosted" && route != "fallback") {
		response.BadRequest(c, "Invalid billing filters")
		return
	}
	result, err := h.hosting.Billing(c.Request.Context(), userID, accountID, route, page, size)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}
