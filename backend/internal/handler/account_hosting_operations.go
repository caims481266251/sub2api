package handler

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/util/logredact"
	"github.com/gin-gonic/gin"
)

type hostingMaintenance interface {
	RefreshHostedCredentials(context.Context, *service.Account) error
	SetHostedPrivacy(context.Context, *service.Account) error
	GetAvailableModels(*gin.Context)
	SyncUpstreamModels(*gin.Context)
	Test(*gin.Context)
}
type hostingTempStateReader interface {
	GetTempUnschedStatus(context.Context, int64) (*service.TempUnschedState, error)
}

type hostedRuntimeDTO struct {
	Status                 string         `json:"status"`
	ErrorMessage           string         `json:"error_message,omitempty"`
	RateLimitResetAt       *time.Time     `json:"rate_limit_reset_at"`
	OverloadUntil          *time.Time     `json:"overload_until"`
	TempUnschedulableUntil *time.Time     `json:"temp_unschedulable_until"`
	RuntimeExtra           map[string]any `json:"runtime_extra"`
}

func hostedRuntime(a *service.Account) hostedRuntimeDTO {
	extra := map[string]any{}
	for _, key := range []string{"privacy_mode", "openai_compact_supported", "openai_compact_checked_at"} {
		if value, ok := a.Extra[key]; ok {
			extra[key] = value
		}
	}
	// Nested rate-limit entries are allowlisted as well: never expose arbitrary Extra.
	var limits map[string]struct {
		Reset string `json:"rate_limit_reset_at"`
	}
	if raw, err := json.Marshal(a.Extra["model_rate_limits"]); err == nil && json.Unmarshal(raw, &limits) == nil && limits != nil {
		extra["model_rate_limits"] = limits
	}
	return hostedRuntimeDTO{Status: a.Status, ErrorMessage: hostedSafeError(a, a.ErrorMessage), RateLimitResetAt: a.RateLimitResetAt, OverloadUntil: a.OverloadUntil, TempUnschedulableUntil: a.TempUnschedulableUntil, RuntimeExtra: extra}
}

func hostedSafeError(a *service.Account, message string) string {
	for _, value := range a.Credentials {
		if s, ok := value.(string); ok && len(s) > 5 {
			message = strings.ReplaceAll(message, s, "[redacted]")
		}
	}
	message = logredact.RedactText(message)
	runes := []rune(message)
	if len(runes) > 1000 {
		message = string(runes[:1000])
	}
	return message
}

func (h *HostingHandler) AccountOperation(c *gin.Context)      { h.accountOperation(c, false) }
func (h *HostingHandler) AdminAccountOperation(c *gin.Context) { h.accountOperation(c, true) }

func (h *HostingHandler) accountOperation(c *gin.Context, admin bool) {
	a, ok := h.quotaAccount(c, admin)
	if !ok {
		return
	}
	operation := c.Param("operation")
	readOnly := operation == "models" || operation == "temp-unschedulable"
	if (readOnly && c.Request.Method != "GET") || (!readOnly && c.Request.Method != "POST") {
		response.Error(c, 405, "Method not allowed")
		return
	}
	if operation == "models" {
		h.maintenance.GetAvailableModels(c)
		return
	}
	if operation == "temp-unschedulable" {
		state, err := h.tempState.GetTempUnschedStatus(c.Request.Context(), a.ID)
		if err != nil {
			response.InternalError(c, "无法读取账号状态")
			return
		}
		if state == nil || state.UntilUnix <= time.Now().Unix() {
			response.Success(c, gin.H{"active": false})
			return
		}
		copy := *state
		copy.ErrorMessage = hostedSafeError(a, copy.ErrorMessage)
		response.Success(c, gin.H{"active": true, "state": copy})
		return
	}
	if operation == "sync-models" {
		h.maintenance.SyncUpstreamModels(c)
		return
	}
	if operation == "test" {
		// Diagnostics must not provide an unmetered arbitrary-prompt gateway.
		var req struct {
			Model string `json:"model_id"`
			// Accepted for compatibility with the shared dialog, never forwarded.
			Prompt string `json:"prompt"`
			Mode   string `json:"mode"`
		}
		if !decodeHostingRequest(c, &req, 16*1024, "Invalid test request") {
			return
		}
		if len(req.Model) > 256 {
			response.BadRequest(c, "Invalid model")
			return
		}
		if !h.testGate.allow(a.ID) {
			response.Error(c, 429, "请稍后再测试，每个账号每分钟仅允许一次连接测试")
			return
		}
		raw, _ := json.Marshal(gin.H{"model_id": req.Model, "prompt": "Reply with OK only.", "mode": "default"})
		c.Request.Body = io.NopCloser(strings.NewReader(string(raw)))
		c.Request.ContentLength = int64(len(raw))
		h.maintenance.Test(c)
		return
	}
	var err error
	switch operation {
	case "refresh":
		err = h.maintenance.RefreshHostedCredentials(c.Request.Context(), a)
	case "set-privacy":
		err = h.maintenance.SetHostedPrivacy(c.Request.Context(), a)
	case "recover-state":
		_, err = h.quotaRecovery.RecoverAccountState(c.Request.Context(), a.ID, service.AccountRecoveryOptions{InvalidateToken: true})
	default:
		response.BadRequest(c, "Invalid operation")
		return
	}
	if err != nil {
		response.Error(c, 502, "操作失败，请检查账号登录状态后重试")
		return
	}
	updated, err := h.accounts.GetByID(c.Request.Context(), a.ID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	view := hostedAccountView(updated)
	view.hostedCapacityDTO = h.hostedCapacity(c.Request.Context(), []*service.Account{updated})[a.ID]
	response.Success(c, view)
}

type hostingTestGate struct {
	mu   sync.Mutex
	next map[int64]time.Time
}

func (g *hostingTestGate) allow(id int64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	if now.Before(g.next[id]) {
		return false
	}
	if g.next == nil {
		g.next = map[int64]time.Time{}
	}
	for key, until := range g.next {
		if now.After(until) {
			delete(g.next, key)
		}
	}
	g.next[id] = now.Add(time.Minute)
	return true
}
