package service

import (
	"context"
	"errors"
	"time"
)

var ErrBorrowProbePolicyInvalid = errors.New("borrow_probe_policy_invalid")
var ErrBorrowProbeBusy = errors.New("borrow_probe_concurrency_full")

type BorrowProbePolicy struct {
	UseGlobal                bool  `json:"use_global"`
	GlobalConcurrency        int   `json:"global_concurrency,omitempty"`
	GlobalRenewBeforeSeconds int   `json:"global_renew_before_seconds"`
	Revision                 int64 `json:"revision"`
	Concurrency              int   `json:"concurrency"`
	RenewBeforeSeconds       int   `json:"renew_before_seconds"`
}

func DefaultBorrowProbePolicy() BorrowProbePolicy {
	return BorrowProbePolicy{Concurrency: 2, RenewBeforeSeconds: 15}
}

func (p BorrowProbePolicy) effective() BorrowProbePolicy {
	if p.Concurrency == 0 {
		return DefaultBorrowProbePolicy()
	}
	return p
}

type BorrowProbePolicyRepository interface {
	SaveProbePolicy(context.Context, int64, BorrowProbePolicy) error
}

func (s *TicketBorrowService) SaveProbePolicy(ctx context.Context, id int64, p BorrowProbePolicy) error {
	if id < 1 || p.Revision < 1 || (!p.UseGlobal && (p.Concurrency < 1 || p.Concurrency > 32)) {
		return ErrBorrowProbePolicyInvalid
	}
	r, ok := s.repo.(BorrowProbePolicyRepository)
	if !ok {
		return ErrBorrowUnavailable
	}
	return r.SaveProbePolicy(ctx, id, p)
}

type BorrowProbeBatchAccount struct {
	AccountID int64  `json:"account_id"`
	Revision  *int64 `json:"revision"`
}
type BorrowProbeBatchInput struct {
	UseGlobal   bool                      `json:"use_global"`
	Accounts    []BorrowProbeBatchAccount `json:"accounts"`
	Concurrency *int                      `json:"concurrency"`
}
type BorrowProbeBatchResult struct {
	AccountID int64  `json:"account_id"`
	Status    string `json:"status"`
	Reason    string `json:"reason,omitempty"`
}

func (s *TicketBorrowService) SaveProbePolicies(ctx context.Context, in BorrowProbeBatchInput) ([]BorrowProbeBatchResult, error) {
	if len(in.Accounts) == 0 || len(in.Accounts) > 1000 || (!in.UseGlobal && in.Concurrency == nil) ||
		(in.UseGlobal && in.Concurrency != nil) ||
		(in.Concurrency != nil && (*in.Concurrency < 1 || *in.Concurrency > 32)) {
		return nil, ErrBorrowProbePolicyInvalid
	}
	seen := make(map[int64]bool, len(in.Accounts))
	for _, a := range in.Accounts {
		if a.AccountID < 1 || a.Revision == nil || *a.Revision < 1 || seen[a.AccountID] {
			return nil, ErrBorrowProbePolicyInvalid
		}
		seen[a.AccountID] = true
	}
	// Read saved policies directly; computing live routing status is unnecessary
	// for editing configuration and can fail independently of a policy save.
	targets, err := s.repo.Targets(ctx)
	if r, ok := s.repo.(BorrowAdminTargetRepository); ok {
		targets, err = r.AdminTargets(ctx)
	}
	if err != nil {
		return nil, err
	}
	policies := make(map[int64]BorrowProbePolicy, len(targets))
	for _, target := range targets {
		policies[target.AccountID] = target.ProbePolicy
	}
	results := make([]BorrowProbeBatchResult, 0, len(in.Accounts))
	for _, a := range in.Accounts {
		p, exists := policies[a.AccountID]
		result := BorrowProbeBatchResult{AccountID: a.AccountID, Status: "failed"}
		if !exists || p.Revision != *a.Revision {
			result.Reason = "borrow_configuration_conflict"
			results = append(results, result)
			continue
		}
		if in.Concurrency != nil {
			p.Concurrency = *in.Concurrency
		}
		// Batch edits are explicit account overrides, including accounts that
		// previously inherited their effective values from global settings.
		p.UseGlobal = in.UseGlobal
		err := s.SaveProbePolicy(ctx, a.AccountID, p)
		switch {
		case err == nil:
			result.Status = "saved"
		case errors.Is(err, ErrBorrowConflict):
			result.Reason = "borrow_configuration_conflict"
		case errors.Is(err, ErrBorrowProbePolicyInvalid):
			result.Reason = "borrow_probe_policy_invalid"
		default:
			result.Reason = "borrow_operation_failed"
		}
		results = append(results, result)
	}
	return results, nil
}

func borrowTargetNeedsProbe(t BorrowTarget) bool {
	if t.BorrowInventory != nil {
		return t.BlockedReason == "" && !t.NextAt.After(t.BorrowInventory.ServerNow)
	}
	return !t.Ready || t.ExpiresAt == nil || t.ExpiresAt.Sub(t.ServerNow) <= time.Duration(t.ProbePolicy.effective().RenewBeforeSeconds)*time.Second
}

func borrowProbeUpstreamWait(result *OpenAICodexStateProbeResult) (string, time.Duration) {
	switch result.Failure {
	case OpenAICodexStateFailureRateLimited:
		return "target_rate_limited", codexProbeMaxWait(time.Minute, result.RetryAfter)
	case OpenAICodexStateFailureAccountError:
		return "target_credentials_unavailable", codexProbeMaxWait(5*time.Minute, result.RetryAfter)
	default:
		return "", 0
	}
}
