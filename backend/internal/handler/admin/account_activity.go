package admin

import (
	"context"
	"net/http"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type accountQualityMonitor interface {
	Get(context.Context, int64, bool) (*service.AccountQualityMonitorView, error)
	BatchForAccounts(context.Context, []*service.Account) (map[int64]*service.AccountQualityMonitorView, error)
	Save(context.Context, int64, service.AccountQualityMonitorConfig) (*service.AccountQualityMonitorView, error)
	Start(context.Context, int64, string) (*service.AccountQualityMonitorView, error)
}

func (h *AccountHandler) SetAccountActivity(monitor *service.AccountQualityMonitorService, recent service.AccountRecentRequestsCache) {
	h.qualityMonitor = monitor
	h.recentRequests = recent
}

type accountActivityItem struct {
	AccountID      int64                              `json:"account_id"`
	Quality        *service.AccountQualityMonitorView `json:"quality"`
	RecentRequests []service.AccountRecentRequest     `json:"recent_requests"`
}

// GetAccountActivity reads the visible page in batches, independently of the
// account list's ETag and without reading request history or full answer bodies.
func (h *AccountHandler) GetAccountActivity(c *gin.Context) {
	var req struct {
		AccountIDs []int64 `json:"account_ids"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
	if c.ShouldBindJSON(&req) != nil || len(req.AccountIDs) > 200 {
		response.BadRequest(c, "Provide at most 200 account IDs")
		return
	}
	ids := make([]int64, 0, len(req.AccountIDs))
	seen := make(map[int64]bool, len(req.AccountIDs))
	for _, id := range req.AccountIDs {
		if id <= 0 {
			response.BadRequest(c, "Invalid account ID")
			return
		}
		if !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	items := make([]accountActivityItem, 0, len(ids))
	if len(ids) == 0 {
		response.Success(c, gin.H{"items": items})
		return
	}
	if h.qualityMonitor == nil {
		response.Error(c, http.StatusServiceUnavailable, "Account activity is unavailable")
		return
	}
	ctx := c.Request.Context()
	accounts, err := h.adminService.GetAccountsByIDs(ctx, ids)
	if response.ErrorFrom(c, err) {
		return
	}
	byID := make(map[int64]*service.Account, len(accounts))
	scopes := make(map[int64]string, len(accounts))
	for _, account := range accounts {
		if account == nil {
			continue
		}
		if _, observer := service.ObserverGroupIDs(ctx); observer && !service.ObserverCanManageAccount(ctx, account) {
			response.ErrorFrom(c, service.ErrObserverScope)
			return
		}
		if account.Platform != service.PlatformOpenAI {
			response.BadRequest(c, "Account activity supports OpenAI accounts only")
			return
		}
		byID[account.ID] = account
	}
	for _, id := range ids {
		if byID[id] == nil {
			response.ErrorFrom(c, service.ErrAccountNotFound)
			return
		}
	}
	quality, err := h.qualityMonitor.BatchForAccounts(ctx, accounts)
	if response.ErrorFrom(c, err) {
		return
	}
	for _, id := range ids {
		if view := quality[id]; view != nil {
			// The monitor resolves credential shadows in a batch, so this scope
			// follows the parent identity as well as the selected local account.
			scopes[id] = view.Scope
		}
	}
	var recent map[int64][]service.AccountRecentRequest
	available := false
	if h.recentRequests != nil {
		recent, err = h.recentRequests.Batch(ctx, scopes)
		available = err == nil
	}
	for _, id := range ids {
		item := accountActivityItem{AccountID: id, Quality: quality[id]}
		if available {
			item.RecentRequests = recent[id]
			if item.RecentRequests == nil {
				item.RecentRequests = []service.AccountRecentRequest{}
			}
		}
		items = append(items, item)
	}
	response.Success(c, gin.H{"items": items})
}

func (h *AccountHandler) qualityMonitorAccountID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return 0, false
	}
	if h.qualityMonitor == nil {
		response.Error(c, http.StatusServiceUnavailable, "Account quality monitoring is unavailable")
		return 0, false
	}
	return id, true
}

func (h *AccountHandler) GetQualityMonitor(c *gin.Context) {
	id, ok := h.qualityMonitorAccountID(c)
	if !ok {
		return
	}
	view, err := h.qualityMonitor.Get(c.Request.Context(), id, true)
	if !response.ErrorFrom(c, err) {
		response.Success(c, view)
	}
}

func (h *AccountHandler) SaveQualityMonitor(c *gin.Context) {
	id, ok := h.qualityMonitorAccountID(c)
	if !ok {
		return
	}
	var config service.AccountQualityMonitorConfig
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
	if c.ShouldBindJSON(&config) != nil {
		response.BadRequest(c, "Invalid quality monitor configuration")
		return
	}
	view, err := h.qualityMonitor.Save(c.Request.Context(), id, config)
	if !response.ErrorFrom(c, err) {
		response.Success(c, view)
	}
}

func (h *AccountHandler) RunQualityMonitor(c *gin.Context) {
	id, ok := h.qualityMonitorAccountID(c)
	if !ok {
		return
	}
	var req struct {
		Scope string `json:"scope" binding:"required,max=128"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1024)
	if c.ShouldBindJSON(&req) != nil {
		response.BadRequest(c, "A current account scope is required")
		return
	}
	view, err := h.qualityMonitor.Start(c.Request.Context(), id, req.Scope)
	if !response.ErrorFrom(c, err) {
		response.Accepted(c, view)
	}
}
