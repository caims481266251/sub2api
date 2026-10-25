package service

import (
	"context"
	"errors"
	"time"
)

const DefaultBorrowRoutePoolSize = 128
const DefaultBorrowRouteCooldownSeconds = 120

func ResolveBorrowPoolSettings(s *BorrowSettings, previous BorrowSettings) error {
	if s.RouteQuality == nil {
		s.RouteQuality = previous.RouteQuality
	}
	if s.RouteQuality == nil {
		s.RouteQuality = DefaultBorrowRouteQualityConfig()
	}
	if err := s.RouteQuality.Validate(); err != nil {
		return err
	}
	if s.RoutePoolSize == nil {
		s.RoutePoolSize = previous.RoutePoolSize
	}
	if s.RouteCooldownSeconds == nil {
		s.RouteCooldownSeconds = previous.RouteCooldownSeconds
	}
	if s.RoutePoolSize == nil {
		s.RoutePoolSize = new(DefaultBorrowRoutePoolSize)
	}
	if s.RouteCooldownSeconds == nil {
		s.RouteCooldownSeconds = new(DefaultBorrowRouteCooldownSeconds)
	}
	if *s.RoutePoolSize < 1 || *s.RoutePoolSize > 256 || *s.RouteCooldownSeconds < 1 || *s.RouteCooldownSeconds > 86400 {
		return errors.New("borrow_route_pool_settings_invalid")
	}
	return nil
}

// Outcomes are fenced by the reservation's generation and recorded once. A
// completed old stream remains deliverable but cannot revive a cooled route.
type BorrowRouteOutcome struct {
	QualityRetry bool
	Route        *BorrowRoute
	Lease        string
	Success      bool
	Reason       string     // empty means an input error, cancellation or local rejection
	RetryAt      *time.Time // only an explicit upstream wait, never the default cooldown
	// A completed quality check authorizes recovery of an already pooled route.
	// Ordinary business success cannot restore it.
	Quality *BorrowRouteQualityResult
}

type BorrowRouteHealthJob struct {
	QualityConfig    *BorrowRouteQualityConfig
	Route            *BorrowRoute
	Token            string
	AccountSignature string
}

type BorrowRoutePoolRepository interface {
	ObserveRouteOutcome(context.Context, BorrowRouteOutcome) error
	CheckRouteReservation(context.Context, *BorrowRoute, string) error
	AcquireRouteHealth(context.Context, string) (*BorrowRouteHealthJob, error)
	CheckRouteHealth(context.Context, *BorrowRouteHealthJob, string) error
	RenewRouteHealth(context.Context, *BorrowRouteHealthJob) error
	FinishRouteHealth(context.Context, *BorrowRouteHealthJob, BorrowRouteOutcome) error
	SweepRoutePool(context.Context) error
}
