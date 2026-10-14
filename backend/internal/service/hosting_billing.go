package service

import (
	"context"
	"time"
)

type HostingBillingEntry struct {
	RequestID           string  `json:"request_id"`
	InputTokens         int     `json:"input_tokens"`
	OutputTokens        int     `json:"output_tokens"`
	CacheReadTokens     int     `json:"cache_read_tokens"`
	CacheCreationTokens int     `json:"cache_creation_tokens"`
	InputCost           float64 `json:"input_cost"`
	OutputCost          float64 `json:"output_cost"`
	CacheReadCost       float64 `json:"cache_read_cost"`
	CacheCreationCost   float64 `json:"cache_creation_cost"`

	ID           int64     `json:"id"`
	CreatedAt    time.Time `json:"created_at"`
	AccountName  string    `json:"account_name"`
	KeyName      string    `json:"key_name"`
	Model        string    `json:"model"`
	Route        string    `json:"route"`
	GroupName    string    `json:"group_name"`
	Reason       string    `json:"reason"`
	Tokens       int64     `json:"tokens"`
	StandardCost float64   `json:"standard_cost"`
	Rate         float64   `json:"rate"`
	ActualCost   float64   `json:"actual_cost"`
}
type HostingBilling struct {
	ActualCost   float64               `json:"actual_cost"`
	Requests     int64                 `json:"requests"`
	Tokens       int64                 `json:"tokens"`
	StandardCost float64               `json:"standard_cost"`
	HostingCost  float64               `json:"hosting_cost"`
	FallbackCost float64               `json:"fallback_cost"`
	Items        []HostingBillingEntry `json:"items"`
	Page         int                   `json:"page"`
	PageSize     int                   `json:"page_size"`
}

func (s *HostingService) Billing(ctx context.Context, userID, accountID int64, route string, page, size int) (HostingBilling, error) {
	return s.repo.Billing(ctx, userID, accountID, route, page, size)
}
