package service

import (
	"context"
	"errors"
	"time"
)

type BorrowStatistics struct {
	TargetID      int64              `json:"target_id"`
	Since         time.Time          `json:"since"`
	Until         time.Time          `json:"until"`
	Passed        int                `json:"passed"`
	Published     int                `json:"published"`
	TicketChanged int                `json:"ticket_changed"`
	Timeout       int                `json:"timeout"`
	Cancelled     int                `json:"cancelled"`
	Other         int                `json:"other"`
	Skipped       int                `json:"skipped"`
	Inflight      int                `json:"inflight"`
	Routes        []BorrowRouteStats `json:"routes"`
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
	if target < 1 || (minutes != 15 && minutes != 60) {
		return nil, errors.New("borrow_event_query_invalid")
	}
	r, ok := s.repo.(BorrowStatisticsRepository)
	if !ok {
		return nil, ErrBorrowUnavailable
	}
	return r.BorrowStatistics(ctx, target, minutes)
}
