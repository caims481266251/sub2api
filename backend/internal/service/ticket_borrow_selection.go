package service

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
)

func (s *TicketBorrowService) Selection(ctx context.Context) (BorrowSelection, error) {
	return s.repo.Selection(ctx)
}
func (s *TicketBorrowService) SourceOptions(ctx context.Context) (BorrowSourceOptions, error) {
	return s.repo.SourceOptions(ctx)
}
func (s *TicketBorrowService) RestoreSources(ctx context.Context) ([]int64, error) {
	return s.repo.RestoreSources(ctx)
}
func normalizeBorrowSelection(v *BorrowSelection) error {
	if v.UseProxyPool {
		v.RotateIdentity = false
	}
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

func (s *TicketBorrowService) normalSourceAccount(ctx context.Context, source BorrowSource) (*Account, error) {
	if s.owner == nil || s.owner.accountRepo == nil || s.owner.openaiGatewayService == nil {
		return nil, ErrBorrowUnavailable
	}
	a, err := s.owner.accountRepo.GetByID(ctx, source.AccountID)
	if err != nil || a == nil || a.ParentAccountID != nil || !a.IsOpenAIOAuthLike() || !a.IsSchedulable() || !a.IsModelSupported(source.Model) || a.GetModelRateLimitRemainingTimeWithContext(ctx, source.Model) > 0 {
		return nil, ErrBorrowUnavailable
	}
	t, err := s.repo.Target(ctx, a.ID)
	if err != nil || t.Enabled {
		return nil, ErrBorrowUnavailable
	}
	if openAICodexStateProbeUnsupportedReason(a, source.Model, false) != "" || s.owner.openaiGatewayService.isOpenAIAccountBlockedBySchedulingThreshold(ctx, a) {
		return nil, ErrBorrowUnavailable
	}
	if !source.UseProxyPool && a.ProxyID != nil && (a.Proxy == nil || !a.Proxy.IsActive() || a.Proxy.IsExpired(time.Now())) {
		return nil, ErrBorrowUnavailable
	}
	return a, nil
}
