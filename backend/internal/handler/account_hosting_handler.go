package handler

import (
	"encoding/json"
	"github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type HostingHandler struct {
	maintenance   hostingMaintenance
	tempState     hostingTempStateReader
	testGate      hostingTestGate
	hosting       *service.HostingService
	keys          *service.APIKeyService
	accounts      service.AccountRepository
	usage         hostingAccountUsageService
	stats         hostingAccountStatsService
	quota         hostingQuotaService
	quotaRecovery hostingQuotaRecoveryService
	concurrency   hostingConcurrencyReader
	rpm           service.RPMCache
}

func NewHostingHandler(hosting *service.HostingService, keys *service.APIKeyService, gateway *service.OpenAIGatewayService, twoFA *service.AccountTokenGuardService, accounts service.AccountRepository, usage *service.AccountUsageService, quota *service.OpenAIQuotaService, recovery *service.RateLimitService, concurrency *service.ConcurrencyService, rpm service.RPMCache, accountHandler *admin.AccountHandler) *HostingHandler {
	if twoFA != nil {
		hosting.SetTwoFALoginService(twoFA)
	}
	keys.SetHostingService(hosting)
	gateway.SetHostingService(hosting)
	return &HostingHandler{maintenance: accountHandler, tempState: recovery, hosting: hosting, keys: keys, accounts: accounts, usage: usage, stats: usage, quota: quota, quotaRecovery: recovery, concurrency: concurrency, rpm: rpm}
}

type hostedAccountDTO struct {
	hostedRuntimeDTO
	hostedCapacityDTO
	Schedulable bool                           `json:"schedulable"`
	CreatedAt   time.Time                      `json:"created_at"`
	PlanType    string                         `json:"plan_type"`
	ID          int64                          `json:"id"`
	Name        string                         `json:"name"`
	Email       string                         `json:"email"`
	State       string                         `json:"state"`
	ResetAt     *time.Time                     `json:"reset_at"`
	LastUsedAt  *time.Time                     `json:"last_used_at"`
	Usage5h     any                            `json:"usage_5h_percent"`
	Usage7d     any                            `json:"usage_7d_percent"`
	Settings    service.HostingAccountSettings `json:"settings"`
	Usage       *service.UsageInfo             `json:"usage"`
	QuotaExtra  hostedQuotaExtra               `json:"quota_extra"`
}

func hostedAccountView(a *service.Account) hostedAccountDTO {
	state, reset := service.HostedAccountAvailability(a, time.Now())
	return hostedAccountDTO{hostedRuntimeDTO: hostedRuntime(a), Schedulable: a.Schedulable, CreatedAt: a.CreatedAt, PlanType: a.GetCredential("plan_type"), ID: a.ID, Name: a.Name, Email: a.GetCredential("email"), State: state, ResetAt: reset, LastUsedAt: a.LastUsedAt, Usage5h: a.Extra["codex_5h_used_percent"], Usage7d: a.Extra["codex_7d_used_percent"], Settings: service.HostedAccountSettings(a), Usage: service.HostedAccountUsage(a), QuotaExtra: hostedQuotaExtraView(a)}
}

func hostingUser(c *gin.Context) (int64, bool) {
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return 0, false
	}
	return subject.UserID, true
}

// Keep read-only account management available after closure, but do not let
// diagnostics/quota endpoints bypass the administrator's hosting switch.
func (h *HostingHandler) RequireEnabled(c *gin.Context) {
	userID, ok := hostingUser(c)
	if !ok {
		c.Abort()
		return
	}
	cfg, _, err := h.hosting.Policy(c.Request.Context(), userID)
	if err == nil && !cfg.Enabled {
		err = service.ErrHostingDisabled
	}
	if err != nil {
		response.ErrorFrom(c, err)
		c.Abort()
		return
	}
	c.Next()
}
func (h *HostingHandler) Status(c *gin.Context) {
	id, ok := hostingUser(c)
	if !ok {
		return
	}
	cfg, _, err := h.hosting.Policy(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	pools, err := h.hosting.Pools(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"enabled": cfg.Enabled, "rate_multiplier": cfg.RateMultiplier, "pools": pools})
}
func (h *HostingHandler) Overview(c *gin.Context) {
	id, ok := hostingUser(c)
	if !ok {
		return
	}
	cfg, policy, err := h.hosting.Policy(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	accounts, err := h.hosting.Accounts(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	summary, err := h.hosting.Summary(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	pools, err := h.hosting.Pools(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	capacityAccounts := make([]*service.Account, 0, len(accounts))
	for i := range accounts {
		capacityAccounts = append(capacityAccounts, &accounts[i])
	}
	capacity := h.hostedCapacity(c.Request.Context(), capacityAccounts)
	views := make([]hostedAccountDTO, 0, len(accounts))
	for i := range accounts {
		view := hostedAccountView(&accounts[i])
		view.hostedCapacityDTO = capacity[accounts[i].ID]
		views = append(views, view)
	}
	twoFAAvailable, twoFAHost := h.hosting.TwoFAAvailability(c.Request.Context())
	response.Success(c, gin.H{"two_fa_login_available": twoFAAvailable, "two_fa_login_host": twoFAHost, "enabled": cfg.Enabled, "seat_limit": cfg.DefaultSeats, "used_seats": len(accounts), "rate_multiplier": cfg.RateMultiplier, "policy": policy, "accounts": views, "summary": summary, "pools": pools})
}
func (h *HostingHandler) Import(c *gin.Context) {
	id, ok := hostingUser(c)
	if !ok {
		return
	}
	var req struct {
		Name      string                          `json:"name"`
		Content   string                          `json:"content"`
		ReplaceID int64                           `json:"replace_id"`
		PoolID    *int64                          `json:"hosting_pool_id"`
		Settings  *service.HostingAccountSettings `json:"settings"`
	}
	if !decodeHostingRequest(c, &req, 2*1024*1024, "Invalid request") {
		return
	}
	a, err := h.hosting.ImportWithSettings(c.Request.Context(), id, req.ReplaceID, req.Name, req.Content, req.Settings, req.PoolID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, hostedAccountView(a))
}
func (h *HostingHandler) OAuthStart(c *gin.Context) {
	id, ok := hostingUser(c)
	if !ok {
		return
	}
	var req struct {
		ReplaceID int64  `json:"replace_id"`
		PoolID    *int64 `json:"hosting_pool_id"`
	}
	if !decodeHostingRequest(c, &req, 16*1024, "Invalid request") {
		return
	}
	r, err := h.hosting.StartOAuth(c.Request.Context(), id, req.ReplaceID, req.PoolID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, r)
}

func (h *HostingHandler) SavePool(c *gin.Context) {
	userID, ok := hostingUser(c)
	if !ok {
		return
	}
	id := int64(0)
	if c.Request.Method != http.MethodPost {
		var err error
		id, err = strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil || id <= 0 {
			response.BadRequest(c, "Invalid pool ID")
			return
		}
	}
	var input struct {
		Name string `json:"name" binding:"required,max=100"`
	}
	if !decodeHostingRequest(c, &input, 16*1024, "Invalid pool name") {
		return
	}
	pool, err := h.hosting.SavePool(c.Request.Context(), userID, id, input.Name)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, pool)
}

func (h *HostingHandler) DeletePool(c *gin.Context) {
	userID, ok := hostingUser(c)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid pool ID")
		return
	}
	if err := h.hosting.DeletePool(c.Request.Context(), userID, id); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"success": true})
}
func (h *HostingHandler) OAuthComplete(c *gin.Context) {
	id, ok := hostingUser(c)
	if !ok {
		return
	}
	var req struct {
		SessionID string                          `json:"session_id"`
		Code      string                          `json:"code"`
		State     string                          `json:"state"`
		Name      string                          `json:"name"`
		Settings  *service.HostingAccountSettings `json:"settings"`
	}
	if !decodeHostingRequest(c, &req, 128*1024, "Invalid request") {
		return
	}
	a, err := h.hosting.CompleteOAuth(c.Request.Context(), id, req.SessionID, req.Code, req.State, req.Name, req.Settings)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, hostedAccountView(a))
}
func (h *HostingHandler) Change(c *gin.Context) {
	userID, ok := hostingUser(c)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	action := c.Param("action")
	if c.Request.Method == http.MethodDelete {
		action = "delete"
	}
	if err = h.hosting.ChangeAccount(c.Request.Context(), userID, id, action); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"success": true})
}

func (h *HostingHandler) UpdateAccountSettings(c *gin.Context) {
	userID, ok := hostingUser(c)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	var input service.HostingAccountSettings
	if !decodeHostingRequest(c, &input, 128*1024, "Invalid account settings") {
		return
	}
	a, err := h.hosting.UpdateAccountSettings(c.Request.Context(), userID, id, input)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, hostedAccountView(a))
}

func decodeHostingRequest(c *gin.Context, input any, maxBytes int64, message string) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(input); err != nil {
		response.BadRequest(c, message)
		return false
	}
	if decoder.Decode(new(any)) != io.EOF {
		response.BadRequest(c, message)
		return false
	}
	return true
}
func (h *HostingHandler) AdminSettings(c *gin.Context) {
	if c.Request.Method == http.MethodGet {
		cfg, err := h.hosting.Settings(c.Request.Context())
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		response.Success(c, cfg)
		return
	}
	var cfg service.HostingSettings
	if c.ShouldBindJSON(&cfg) != nil {
		response.BadRequest(c, "Invalid request")
		return
	}
	if err := h.hosting.UpdateSettings(c.Request.Context(), cfg); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, cfg)
}
func (h *HostingHandler) AdminPolicy(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid user ID")
		return
	}
	if c.Request.Method == http.MethodGet {
		cfg, p, err := h.hosting.Policy(c.Request.Context(), id)
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		response.Success(c, gin.H{"policy": p, "seat_limit": cfg.DefaultSeats, "rate_multiplier": cfg.RateMultiplier})
		return
	}
	var p service.HostingUserPolicy
	if c.ShouldBindJSON(&p) != nil {
		response.BadRequest(c, "Invalid request")
		return
	}
	if err = h.hosting.SetUserPolicy(c.Request.Context(), id, p); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	h.keys.InvalidateAuthCacheByUserID(c.Request.Context(), id)
	response.Success(c, p)
}
