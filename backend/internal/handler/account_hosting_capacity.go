package handler

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type hostingConcurrencyReader interface {
	GetAccountConcurrencyBatch(context.Context, []int64) (map[int64]int, error)
}

// Read only the authorized page/owner's account IDs. Cache failures remain
// unknown instead of reporting a misleading zero current load.
func (h *HostingHandler) hostedCapacity(ctx context.Context, accounts []*service.Account) map[int64]hostedCapacityDTO {
	ids := make([]int64, 0, len(accounts))
	for _, a := range accounts {
		ids = append(ids, a.ID)
	}
	result := make(map[int64]hostedCapacityDTO, len(ids))
	if len(ids) == 0 {
		return result
	}
	if h.concurrency != nil {
		if counts, err := h.concurrency.GetAccountConcurrencyBatch(ctx, ids); err == nil {
			for _, id := range ids {
				value := counts[id]
				item := result[id]
				item.Concurrency = &value
				result[id] = item
			}
		}
	}
	if h.rpm != nil {
		if counts, err := h.rpm.GetRPMBatch(ctx, ids); err == nil {
			for _, id := range ids {
				value := counts[id]
				item := result[id]
				item.RPM = &value
				result[id] = item
			}
		}
	}
	return result
}

type hostedCapacityDTO struct {
	Concurrency *int `json:"current_concurrency"`
	RPM         *int `json:"current_rpm"`
}
