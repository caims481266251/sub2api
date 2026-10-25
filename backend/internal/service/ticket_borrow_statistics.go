package service

import (
	"context"
	"errors"
	"time"
)

type BorrowPhaseStatistics struct {
	Requests          int            `json:"requests"`
	DiagnosticSamples int            `json:"diagnostic_samples"`
	Retries           int            `json:"retries"`
	AverageMS         *float64       `json:"average_ms"`
	Timeouts          int            `json:"timeouts"`
	ErrorClasses      map[string]int `json:"error_classes"`
	TimeoutStages     map[string]int `json:"timeout_stages"`
	Cancellations     map[string]int `json:"cancellations"`
}

type BorrowStatistics struct {
	Mint                 BorrowPhaseStatistics `json:"mint"`
	Continue             BorrowPhaseStatistics `json:"continue"`
	SharedTargetInflight int                   `json:"shared_target_inflight"`
	SharedTargetCount    int                   `json:"shared_target_count"`
	Candidates           int                   `json:"candidates"`
	Gateways             int                   `json:"gateways"`
	SourceFailed         int                   `json:"source_failed"`
	Inconclusive         int                   `json:"inconclusive"`
	RouteChanged         int                   `json:"route_changed"`
	Prioritized          int                   `json:"prioritized"`
	Rechecked            int                   `json:"rechecked"`
	LastSuccessAt        *time.Time            `json:"last_success_at,omitempty"`
	TargetID             int64                 `json:"target_id"`
	Since                time.Time             `json:"since"`
	Until                time.Time             `json:"until"`
	Passed               int                   `json:"passed"`
	Published            int                   `json:"published"`
	TicketChanged        int                   `json:"ticket_changed"`
	Timeout              int                   `json:"timeout"`
	Cancelled            int                   `json:"cancelled"`
	Other                int                   `json:"other"`
	Skipped              int                   `json:"skipped"`
	Inflight             int                   `json:"inflight"`
	Routes               []BorrowRouteStats    `json:"routes"`
}

type BorrowRouteStats struct {
	RouteID           string   `json:"route_id"`
	Gateway           string   `json:"gateway"`
	Requests          int      `json:"requests"`
	Completed         int      `json:"completed"`
	Interrupted       int      `json:"interrupted"`
	Unconfirmed       int      `json:"unconfirmed"`
	VisibleSamples    int      `json:"visible_samples"`
	VisibleAverageMS  *float64 `json:"visible_average_ms"`
	SemanticSamples   int      `json:"semantic_samples"`
	SemanticAverageMS *float64 `json:"semantic_average_ms"`
	AverageAgeMS      *float64 `json:"average_age_ms"`
	LatestUsageID     int64    `json:"latest_usage_id"`
}

type BorrowStatisticsRepository interface {
	BorrowStatistics(context.Context, int64, int) (*BorrowStatistics, error)
}

func (s *TicketBorrowService) Statistics(ctx context.Context, target int64, minutes int) (*BorrowStatistics, error) {
	if target < 1 || (minutes != 2 && minutes != 15 && minutes != 60) {
		return nil, errors.New("borrow_event_query_invalid")
	}
	r, ok := s.repo.(BorrowStatisticsRepository)
	if !ok {
		return nil, ErrBorrowUnavailable
	}
	return r.BorrowStatistics(ctx, target, minutes)
}
