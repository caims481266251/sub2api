package admin

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func ProvideProxyHandler(adminService service.AdminService, repo service.ProxyPoolRepository, encryptor service.OpenAICredentialEncryptor) *ProxyHandler {
	h := NewProxyHandler(adminService)
	h.pool = service.NewProxyPoolService(repo, encryptor)
	return h
}
func (h *ProxyHandler) poolService(c *gin.Context) *service.ProxyPoolService {
	if _, observer := service.ObserverGroupIDs(c.Request.Context()); observer {
		response.Forbidden(c, "Administrator access required")
		return nil
	}
	if h.pool == nil {
		proxyPoolError(c, service.ErrProxyPoolUnavailable)
		return nil
	}
	return h.pool
}
func proxyPoolError(c *gin.Context, err error) {
	status, reason := http.StatusServiceUnavailable, "proxy_pool_unavailable"
	switch {
	case errors.Is(err, service.ErrProxyPoolConflict):
		status, reason = http.StatusConflict, "proxy_pool_conflict"
	case errors.Is(err, service.ErrProxyPoolInvalid):
		status, reason = http.StatusBadRequest, "proxy_pool_invalid"
	case errors.Is(err, service.ErrProxyPoolEncryption):
		reason = "proxy_pool_encryption_required"
	}
	response.ErrorWithDetails(c, status, reason, reason, nil)
}
func (h *ProxyHandler) PoolState(c *gin.Context) {
	s := h.poolService(c)
	if s == nil {
		return
	}
	state, err := s.State(c.Request.Context())
	if err != nil {
		proxyPoolError(c, err)
		return
	}
	response.Success(c, state)
}
func (h *ProxyHandler) PoolEncryption(c *gin.Context) {
	s := h.poolService(c)
	if s == nil {
		return
	}
	state, err := s.InitializeEncryption(c.Request.Context())
	if err != nil {
		proxyPoolError(c, err)
		return
	}
	response.Success(c, state)
}
func (h *ProxyHandler) PoolImport(c *gin.Context) {
	s := h.poolService(c)
	if s == nil {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 2*service.ProxyPoolMaxBytes)
	var in service.ProxyPoolImport
	if c.ShouldBindJSON(&in) != nil {
		proxyPoolError(c, service.ErrProxyPoolInvalid)
		return
	}
	if c.Request.Method == http.MethodPost {
		preview, err := s.Preview(c.Request.Context(), in)
		if err != nil {
			proxyPoolError(c, err)
			return
		}
		response.Success(c, preview)
		return
	}
	state, err := s.Import(c.Request.Context(), in)
	if err != nil {
		proxyPoolError(c, err)
		return
	}
	response.Success(c, state)
}
func (h *ProxyHandler) PoolSetEnabled(c *gin.Context) {
	s := h.poolService(c)
	if s == nil {
		return
	}
	id, err := strconv.ParseInt(c.Param("entryID"), 10, 64)
	var in struct {
		Revision int64 `json:"revision"`
		Enabled  *bool `json:"enabled"`
	}
	if err != nil || id < 1 || c.ShouldBindJSON(&in) != nil || in.Enabled == nil {
		proxyPoolError(c, service.ErrProxyPoolInvalid)
		return
	}
	state, err := s.SetEnabled(c.Request.Context(), in.Revision, id, *in.Enabled)
	if err != nil {
		proxyPoolError(c, err)
		return
	}
	response.Success(c, state)
}
