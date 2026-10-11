package service

import (
	"context"
	"errors"
	"maps"
)

type borrowSchedulingTierKey struct{}
type borrowSchedulingTier struct {
	ids       map[int64]struct{}
	preferred bool
}

func borrowPreferredPass(ctx context.Context) bool {
	tier, ok := ctx.Value(borrowSchedulingTierKey{}).(borrowSchedulingTier)
	return ok && tier.preferred
}

// The tier is a request-local preference, never a replacement for admission.
// Checks of identity, model, route deadline and concurrency still run normally.
func borrowSchedulingAllowsAccount(ctx context.Context, id int64) bool {
	tier, restricted := ctx.Value(borrowSchedulingTierKey{}).(borrowSchedulingTier)
	if !restricted {
		return true
	}
	_, borrowed := tier.ids[id]
	return borrowed == tier.preferred
}

type openAISelectionCall func(context.Context) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error)

func borrowSchedulingCapacityError(err error) bool {
	return errors.Is(err, ErrNoAvailableAccounts) || errors.Is(err, ErrNoAvailableCompactAccounts) || errors.Is(err, ErrOpenAIRPMExhausted) || IsBorrowRouteUnavailable(err)
}

func selectBorrowTier(ctx context.Context, ids map[int64]struct{}, selectAccount openAISelectionCall) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	remaining := maps.Clone(ids)
	var waiting *AccountSelectionResult
	var waitDecision OpenAIAccountScheduleDecision
	for attempts := 0; len(remaining) > 0 && attempts < openAIAccountSelectionProbeLimit; attempts++ {
		selection, decision, err := selectAccount(context.WithValue(ctx, borrowSchedulingTierKey{}, borrowSchedulingTier{ids: remaining, preferred: true}))
		if err != nil {
			if waiting != nil && borrowSchedulingCapacityError(err) {
				return waiting, waitDecision, nil
			}
			return selection, decision, err
		}
		if selection == nil || selection.Acquired || selection.Account == nil || selection.WaitPlan == nil {
			return selection, decision, nil
		}
		if waiting == nil {
			waiting, waitDecision = selection, decision
		}
		if _, included := remaining[selection.Account.ID]; !included {
			break
		}
		// A sticky/full account must not hide free slots on another borrowed
		// account. Do not mutate a map retained by a prior selection context.
		remaining = maps.Clone(remaining)
		delete(remaining, selection.Account.ID)
	}
	return waiting, waitDecision, nil
}

// Avoid loading dashboard counters and scan histories on every business request.
type BorrowSchedulingRepository interface {
	EnabledTargetIDs(context.Context) ([]int64, error)
}

func (s *TicketBorrowService) schedulingTargetIDs(ctx context.Context) ([]int64, error) {
	if repo, ok := s.repo.(BorrowSchedulingRepository); ok {
		return repo.EnabledTargetIDs(ctx)
	}
	targets, err := s.repo.Targets(ctx)
	ids := make([]int64, 0, len(targets))
	for _, target := range targets {
		if target.Enabled {
			ids = append(ids, target.AccountID)
		}
	}
	return ids, err
}

func (s *OpenAIGatewayService) selectWithBorrowPreference(ctx context.Context, selectAccount openAISelectionCall) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	if s.borrowRoutes == nil || borrowInternal(ctx) {
		return selectAccount(ctx)
	}
	targets, err := s.borrowRoutes.schedulingTargetIDs(ctx)
	if err != nil {
		// Normal selection still checks borrowing admission and cannot send a
		// borrowed account directly when its authority is unavailable.
		return selectAccount(ctx)
	}
	ids := make(map[int64]struct{}, len(targets))
	for _, id := range targets {
		if borrowHTTPContinuationAllowsAccount(ctx, id) {
			ids[id] = struct{}{}
		}
	}
	if len(ids) == 0 {
		return selectAccount(ctx)
	}
	preferred, decision, err := selectBorrowTier(ctx, ids, selectAccount)
	if err == nil && preferred != nil && preferred.Acquired {
		decision.BorrowPreferred = true
		return preferred, decision, nil
	}
	if err != nil && !borrowSchedulingCapacityError(err) {
		return nil, decision, err
	}
	// A preferred account's wait plan must not hide immediately available normal
	// capacity. If everything is busy, retain the original scheduler's wait plan.
	selection, fallbackDecision, fallbackErr := selectAccount(context.WithValue(ctx, borrowSchedulingTierKey{}, borrowSchedulingTier{ids: ids}))
	if preferred != nil && preferred.WaitPlan != nil && (fallbackErr != nil || selection == nil || !selection.Acquired) && ctx.Err() == nil &&
		(fallbackErr == nil || borrowSchedulingCapacityError(fallbackErr)) {
		return preferred, decision, nil
	}
	return selection, fallbackDecision, fallbackErr
}
