package admin

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// GetDockerSystemPool returns catalog metadata and database counts only.
func (h *AccountHandler) GetDockerSystemPool(c *gin.Context) {
	provider, ok := h.adminService.(interface {
		GetDockerSystemPool(context.Context) (*service.DockerSystemPoolSnapshot, error)
	})
	if !ok {
		response.Error(c, 503, "Docker system pool is unavailable")
		return
	}
	snapshot, err := provider.GetDockerSystemPool(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	response.Success(c, snapshot)
}
