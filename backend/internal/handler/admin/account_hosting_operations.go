package admin

import (
	"context"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// The hosting handler authorizes ownership and serializes a restricted DTO.
// Reuse the admin maintenance implementation without its credential-rich response.
func (h *AccountHandler) RefreshHostedCredentials(ctx context.Context, a *service.Account) error {
	_, _, err := h.refreshSingleAccount(ctx, a)
	return err
}

func (h *AccountHandler) SetHostedPrivacy(ctx context.Context, a *service.Account) error {
	if h.adminService.ForceOpenAIPrivacy(ctx, a) == "" {
		return fmt.Errorf("privacy setting failed")
	}
	return nil
}
