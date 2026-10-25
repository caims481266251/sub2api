package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type hostingAccountUsageService interface {
	GetUsageForAccount(context.Context, *service.Account, ...bool) (*service.UsageInfo, error)
}

type hostingQuotaService interface {
	QueryUsage(context.Context, int64) (*service.OpenAIQuotaUsage, error)
	ResetCredit(context.Context, int64) (*service.OpenAIQuotaResetResult, error)
	CacheCreditsSnapshot(context.Context, int64, *service.OpenAIQuotaUsage) error
	CacheResetCreditsSnapshot(context.Context, int64, *service.OpenAIRateLimitResetCredits) error
	CachePostResetSnapshot(context.Context, int64, *service.OpenAIQuotaUsage) error
}

type hostingQuotaRecoveryService interface {
	RecoverAccountState(context.Context, int64, service.AccountRecoveryOptions) (*service.SuccessfulTestRecoveryResult, error)
}

// Only data needed by the shared quota panel is exposed. In particular, neither
// account credentials nor platform routing/network configuration is returned.
type hostedQuotaExtra struct {
	Credits          *hostedCreditsSnapshot               `json:"codex_credits_snapshot,omitempty"`
	ResetCredits     *service.OpenAIRateLimitResetCredits `json:"codex_reset_credit_snapshot,omitempty"`
	AutoResetEnabled bool                                 `json:"auto_reset_credit_enabled"`
	AutoResetState   *hostedAutoResetState                `json:"codex_auto_reset_credit_state,omitempty"`
}

type hostedCreditsSnapshot struct {
	Credits   *service.OpenAICredits `json:"credits"`
	FetchedAt int64                  `json:"fetched_at"`
}

type hostedAutoResetState struct {
	Status        string `json:"status"`
	TriggerWindow string `json:"trigger_window,omitempty"`
	CheckedAt     string `json:"checked_at,omitempty"`
	ErrorCode     string `json:"error_code,omitempty"`
}

func hostedSnapshot[T any](value any) *T {
	if value == nil {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var result T
	if json.Unmarshal(raw, &result) != nil {
		return nil
	}
	return &result
}

func hostedQuotaExtraView(a *service.Account) hostedQuotaExtra {
	enabled, _ := a.Extra["auto_reset_credit_enabled"].(bool)
	return hostedQuotaExtra{
		Credits:          hostedSnapshot[hostedCreditsSnapshot](a.Extra["codex_credits_snapshot"]),
		ResetCredits:     hostedSnapshot[service.OpenAIRateLimitResetCredits](a.Extra["codex_reset_credit_snapshot"]),
		AutoResetEnabled: enabled,
		AutoResetState:   hostedSnapshot[hostedAutoResetState](a.Extra["codex_auto_reset_credit_state"]),
	}
}

func (h *HostingHandler) quotaAccount(c *gin.Context, admin bool) (*service.Account, bool) {
	userID, ok := hostingUser(c)
	if !ok {
		return nil, false
	}
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || accountID <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return nil, false
	}
	account, err := h.accounts.GetByID(c.Request.Context(), accountID)
	if err != nil || account == nil || account.OwnerUserID == nil || (!admin && *account.OwnerUserID != userID) || !account.IsOpenAIOAuth() || account.ParentAccountID != nil {
		response.ErrorFrom(c, service.ErrHostingAccountNotFound)
		return nil, false
	}
	c.Header("Cache-Control", "no-store")
	return account, true
}

func (h *HostingHandler) AccountUsage(c *gin.Context)      { h.accountUsage(c, false) }
func (h *HostingHandler) AdminAccountUsage(c *gin.Context) { h.accountUsage(c, true) }
func (h *HostingHandler) RefreshQuota(c *gin.Context)      { h.refreshQuota(c, false) }
func (h *HostingHandler) AdminRefreshQuota(c *gin.Context) { h.refreshQuota(c, true) }
func (h *HostingHandler) ResetQuota(c *gin.Context)        { h.resetQuota(c, false) }
func (h *HostingHandler) AdminResetQuota(c *gin.Context)   { h.resetQuota(c, true) }

func (h *HostingHandler) accountUsage(c *gin.Context, admin bool) {
	account, ok := h.quotaAccount(c, admin)
	if !ok {
		return
	}
	usage, err := h.usage.GetUsageForAccount(c.Request.Context(), account, c.Query("force") == "true")
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if usage != nil {
		// The shared usage service may carry upstream diagnostic text. Copy
		// before redaction so a cached result is not modified for other callers.
		safe := *usage
		safe.Error = hostedSafeError(account, safe.Error)
		safe.ForbiddenReason = hostedSafeError(account, safe.ForbiddenReason)
		usage = &safe
	}
	response.Success(c, usage)
}

func (h *HostingHandler) refreshQuota(c *gin.Context, admin bool) {
	account, ok := h.quotaAccount(c, admin)
	if !ok {
		return
	}
	usage, err := h.quota.QueryUsage(c.Request.Context(), account.ID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if usage == nil {
		response.Error(c, http.StatusBadGateway, "openai quota query returned an empty result")
		return
	}
	service.NotifyOpenAIAutoResetCredit(account.ID)
	creditsCached := h.quota.CacheCreditsSnapshot(c.Request.Context(), account.ID, usage) == nil
	resetCached := h.quota.CacheResetCreditsSnapshot(c.Request.Context(), account.ID, usage.RateLimitResetCredits) == nil
	response.Success(c, struct {
		*service.OpenAIQuotaUsage
		CachePersisted        bool `json:"cache_persisted"`
		CreditsCachePersisted bool `json:"credits_cache_persisted"`
	}{usage, resetCached, creditsCached})
}

func (h *HostingHandler) resetQuota(c *gin.Context, admin bool) {
	account, ok := h.quotaAccount(c, admin)
	if !ok {
		return
	}
	result, err := h.quota.ResetCredit(c.Request.Context(), account.ID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if result == nil {
		response.Error(c, http.StatusBadGateway, "openai quota reset returned an empty result")
		return
	}
	// Consumption succeeded. Finish the existing recovery workflow even when the
	// browser disconnects, and report partial failures without suggesting a retry.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 8*time.Second)
	defer cancel()
	post := service.RunOpenAIQuotaResetPostProcess(ctx, account.ID, h.quota, h.quotaRecovery, h.accounts.GetByID)
	var view *hostedAccountDTO
	if post.Account != nil {
		safe := hostedAccountView(post.Account)
		view = &safe
	}
	response.Success(c, struct {
		*service.OpenAIQuotaResetResult
		Quota                 *service.OpenAIQuotaUsage `json:"quota,omitempty"`
		Account               *hostedAccountDTO         `json:"account,omitempty"`
		CacheRefreshed        bool                      `json:"cache_refreshed"`
		AccountStateRecovered bool                      `json:"account_state_recovered"`
		WarningCode           string                    `json:"warning_code,omitempty"`
	}{result, post.Quota, view, post.CacheRefreshed, post.AccountStateRecovered, post.WarningCode})
}
