package service

import (
	"context"
	"errors"
	"sort"
	"strings"
)

func (s *TicketBorrowService) Selection(ctx context.Context) (BorrowSelection, error) {
	return s.repo.Selection(ctx)
}
func (s *TicketBorrowService) SourceOptions(ctx context.Context) (BorrowSourceOptions, error) {
	out, err := s.repo.SourceOptions(ctx)
	if err != nil {
		return out, err
	}
	selection, err := s.repo.Selection(ctx)
	if err != nil {
		return out, err
	}
	for i := range out.Accounts {
		option := &out.Accounts[i]
		model := selection.DefaultModel
		if override := selection.ModelOverrides[option.ID]; override != "" {
			model = override
		}
		_, healthErr := s.normalBorrowAccount(ctx, option.ID, model)
		option.RequestEligible = !option.IsTarget && healthErr == nil
		if healthErr != nil {
			option.UnavailableReason = borrowHealthReason(healthErr)
		}
	}
	return out, nil
}
func (s *TicketBorrowService) RestoreSources(ctx context.Context) ([]int64, error) {
	return s.repo.RestoreSources(ctx)
}
func normalizeBorrowSelection(v *BorrowSelection) error {
	// Source model requests always use a persisted borrowing exit.
	v.UseProxyPool, v.RotateIdentity = true, false
	if v.Revision <= 0 || len(v.GroupIDs) > 1000 || len(v.AccountIDs) > 1000 || len(v.ModelOverrides) > 1000 {
		return errors.New("borrow_selection_invalid")
	}
	validModel := func(model string) bool {
		return model != "" && len(model) <= 256 && !strings.ContainsAny(model, " \t\r\n*")
	}
	if !validModel(v.DefaultModel) {
		return errors.New("borrow_source_model_invalid")
	}
	dedupe := func(ids []int64) ([]int64, error) {
		out := []int64{}
		seen := map[int64]bool{}
		for _, id := range ids {
			if id <= 0 {
				return nil, errors.New("borrow_selection_invalid")
			}
			if !seen[id] {
				out = append(out, id)
				seen[id] = true
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out, nil
	}
	var err error
	if v.GroupIDs, err = dedupe(v.GroupIDs); err != nil {
		return err
	}
	if v.AccountIDs, err = dedupe(v.AccountIDs); err != nil {
		return err
	}
	if v.ModelOverrides == nil {
		v.ModelOverrides = map[int64]string{}
	}
	for id, model := range v.ModelOverrides {
		if id <= 0 || !validModel(model) {
			return errors.New("borrow_source_model_invalid")
		}
	}
	return nil
}
func (s *TicketBorrowService) SaveSelection(ctx context.Context, v BorrowSelection) error {
	if err := normalizeBorrowSelection(&v); err != nil {
		return err
	}
	return s.repo.SaveSelection(ctx, v)
}

func (s *TicketBorrowService) normalBorrowAccount(ctx context.Context, id int64, model string) (*Account, error) {
	if s.owner == nil || s.owner.accountRepo == nil || s.owner.openaiGatewayService == nil || id <= 0 {
		return nil, ErrBorrowUnavailable
	}
	a, err := s.owner.accountRepo.GetByID(ctx, id)
	if err != nil || a == nil || a.ID != id || a.ParentAccountID != nil || !a.IsOpenAIOAuthLike() {
		return nil, ErrBorrowUnavailable
	}
	gateway := s.owner.openaiGatewayService
	if reason := openAICompatibleAccountEligibilityFailureReasonBeforeProfit(ctx, a, nil, PlatformOpenAI, model, false, OpenAIEndpointCapabilityResponses); reason != "" {
		return nil, errors.New("source_" + reason)
	}
	if gateway.isOpenAIAccountHealthRuntimeBlocked(a, model, false, true) {
		return nil, errors.New("source_runtime_blocked")
	}
	if openAICodexStateProbeUnsupportedReason(a, model, false) != "" || gateway.isOpenAIAccountBlockedBySchedulingThreshold(ctx, a) {
		return nil, errors.New("source_account_threshold")
	}
	return a, nil
}

func (s *TicketBorrowService) normalSourceAccount(ctx context.Context, source BorrowSource) (*Account, error) {
	a, err := s.normalBorrowAccount(ctx, source.AccountID, source.Model)
	if err != nil {
		return nil, err
	}
	t, err := s.repo.Target(ctx, a.ID)
	if err != nil || t.Enabled {
		return nil, ErrBorrowUnavailable
	}
	return a, nil
}

func borrowHealthReason(err error) string {
	if err == nil {
		return ""
	}
	switch err.Error() {
	case "source_model_rate_limited", "source_model_not_supported", "source_runtime_blocked", "source_account_threshold":
		return err.Error()
	default:
		if strings.HasPrefix(err.Error(), "source_quota_auto_pause") {
			return "source_account_threshold"
		}
		return "source_account_unavailable"
	}
}
