package handler

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/gin-gonic/gin"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type hostedAdminAccountDTO struct {
	hostedAccountDTO
	UserID    int64  `json:"user_id"`
	UserEmail string `json:"user_email"`
	Username  string `json:"username"`
	PoolName  string `json:"pool_name"`
}

func (h *HostingHandler) AdminAccounts(c *gin.Context) {
	page, pageErr := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, sizeErr := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	search := strings.TrimSpace(c.Query("search"))
	if pageErr != nil || sizeErr != nil || page < 1 || page > 1000000 || pageSize < 1 || pageSize > 100 || len([]rune(search)) > 200 {
		response.BadRequest(c, "分页或搜索参数无效")
		return
	}
	accounts, total, err := h.hosting.AdminAccounts(c.Request.Context(), search, page, pageSize)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	capacityAccounts := make([]*service.Account, 0, len(accounts))
	for i := range accounts {
		capacityAccounts = append(capacityAccounts, &accounts[i].Account)
	}
	capacity := h.hostedCapacity(c.Request.Context(), capacityAccounts)
	items := make([]hostedAdminAccountDTO, 0, len(accounts))
	for _, item := range accounts {
		view := hostedAccountView(&item.Account)
		view.hostedCapacityDTO = capacity[item.Account.ID]
		items = append(items, hostedAdminAccountDTO{hostedAccountDTO: view, UserID: item.UserID, UserEmail: item.UserEmail, Username: item.Username, PoolName: item.PoolName})
	}
	c.Header("Cache-Control", "no-store")
	response.Success(c, gin.H{"items": items, "total": total, "page": page, "page_size": pageSize})
}

// Ownership is taken from the stored hosted account, never from the request body.
func (h *HostingHandler) AdminAccountSettings(c *gin.Context) {
	account, ok := h.quotaAccount(c, true)
	if !ok {
		return
	}
	var input service.HostingAccountSettings
	if !decodeHostingRequest(c, &input, 128*1024, "Invalid account settings") {
		return
	}
	updated, err := h.hosting.UpdateAccountSettings(c.Request.Context(), *account.OwnerUserID, account.ID, input)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, hostedAccountView(updated))
}

func (h *HostingHandler) AdminAccountPools(c *gin.Context) {
	account, ok := h.quotaAccount(c, true)
	if !ok {
		return
	}
	pools, err := h.hosting.Pools(c.Request.Context(), *account.OwnerUserID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, pools)
}

func (h *HostingHandler) AdminChangeAccount(c *gin.Context) {
	account, ok := h.quotaAccount(c, true)
	if !ok {
		return
	}
	action := c.Param("action")
	if c.Request.Method == http.MethodDelete {
		action = "delete"
	}
	if err := h.hosting.ChangeAccount(c.Request.Context(), *account.OwnerUserID, account.ID, action); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"success": true})
}
