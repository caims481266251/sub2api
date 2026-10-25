package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/google/uuid"
)

// This is a scheduling preference, never authority to replay a response or use
// another account's encrypted state. Response/turn ownership remains separate.
type OpenAISessionAffinityBinding struct {
	AccountID int64  `json:"account_id"`
	Channel   string `json:"channel"`
	Version   string `json:"version"`
	Pending   bool   `json:"pending"`
}

// Owner and provisional claims live in a separate versioned namespace from
// legacy sticky keys. All mutations must be atomic across gateway instances.
type OpenAISessionAffinityCache interface {
	GetOpenAISessionAffinity(context.Context, string) (*OpenAISessionAffinityBinding, error)
	ClaimOpenAISessionAffinity(context.Context, string, OpenAISessionAffinityBinding, time.Duration) (*OpenAISessionAffinityBinding, error)
	CompleteOpenAISessionAffinity(context.Context, string, OpenAISessionAffinityBinding, OpenAISessionAffinityBinding, time.Duration) (bool, error)
}

type OpenAISessionAffinityRequest struct {
	UserID, APIKeyID   int64
	GroupID            *int64
	SessionHash        string
	Model              string
	Platform           string
	PreviousResponseID string
	Disabled           bool
}

type openAISessionAffinityKey struct{}
type openAISessionAffinityOnlyKey struct{}
type openAISessionAffinitySkipKey struct{}

func openAISessionAffinityAllowsAccount(ctx context.Context, id int64) bool {
	if owner, _ := ctx.Value(openAISessionAffinityOnlyKey{}).(int64); owner > 0 && owner != id {
		return false
	}
	if skipped, _ := ctx.Value(openAISessionAffinitySkipKey{}).(int64); skipped > 0 && skipped == id {
		return false
	}
	return true
}

type openAISessionAffinity struct {
	mu                            sync.Mutex
	key, sessionHash, model       string
	parentKey                     string
	groupID                       int64
	cache                         OpenAISessionAffinityCache
	base                          *OpenAISessionAffinityBinding
	loaded, cacheFailed, finished bool
	mode, reason                  string
	selectedID                    int64
	transientFailures             map[int64]string
}

func openAISessionAffinityFromContext(ctx context.Context) *openAISessionAffinity {
	a, _ := ctx.Value(openAISessionAffinityKey{}).(*openAISessionAffinity)
	return a
}

func (a *openAISessionAffinity) snapshot() *OpenAISessionAffinityBinding {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.base == nil {
		return nil
	}
	b := *a.base
	return &b
}

func managedOpenAISessionAffinity(ctx context.Context, groupID *int64, sessionHash string) *openAISessionAffinity {
	a := openAISessionAffinityFromContext(ctx)
	if a != nil && a.groupID == derefGroupID(groupID) && a.sessionHash == sessionHash {
		return a
	}
	return nil
}

func (s *OpenAIGatewayService) BeginOpenAISessionAffinity(ctx context.Context, req OpenAISessionAffinityRequest) context.Context {
	if s == nil || s.cfg == nil || !s.cfg.Gateway.OpenAISessionAffinityEnabled || req.Disabled ||
		req.UserID <= 0 || req.APIKeyID <= 0 || strings.TrimSpace(req.SessionHash) == "" || strings.TrimSpace(req.Model) == "" ||
		NormalizeOpenAICompatiblePlatform(req.Platform) != PlatformOpenAI || req.PreviousResponseID != "" || borrowInternal(ctx) {
		return ctx
	}
	if _, guardian := openAIGuardianParentAffinityFromContext(ctx); guardian || borrowHTTPContinuationFromContext(ctx) != nil {
		return ctx
	}
	cache, ok := s.cache.(OpenAISessionAffinityCache)
	if !ok {
		return ctx
	}
	groups := s.cfg.Gateway.OpenAISessionAffinityGroupIDs
	if len(groups) > 0 {
		allowed := false
		for _, id := range groups {
			if id == derefGroupID(req.GroupID) {
				allowed = true
				break
			}
		}
		if !allowed {
			return ctx
		}
	}
	// Do not promote legacy group-only sticky records into tenant/model owners.
	// The scheduling hash never replaces the existing outbound cache identity.
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%d\x00%d\x00%s\x00%s", req.UserID, req.APIKeyID,
		derefGroupID(req.GroupID), strings.TrimSpace(req.Model), req.SessionHash)))
	a := &openAISessionAffinity{key: fmt.Sprintf("%x", sum[:]), sessionHash: req.SessionHash,
		model: req.Model, groupID: derefGroupID(req.GroupID), cache: cache,
		parentKey: openAIScopedParentAffinityKey(req.APIKeyID, req.SessionHash)}
	return context.WithValue(ctx, openAISessionAffinityKey{}, a)
}

func (a *openAISessionAffinity) load(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.loaded {
		return
	}
	a.loaded = true
	readCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	var err error
	a.base, err = a.cache.GetOpenAISessionAffinity(readCtx, a.key)
	// A failed read must not create a replacement owner or fall back to an
	// unscoped legacy binding. Availability still uses the normal scheduler.
	if err != nil {
		a.cacheFailed = true
		a.base = nil
	}
}

// Record the selected execution separately from ownership. This also runs at
// the handler's post-slot admission point for selections that had to queue.
func (s *OpenAIGatewayService) admitOpenAISessionAffinity(ctx context.Context, a *openAISessionAffinity, accountID int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.selectedID = accountID
	if a.base != nil || a.cacheFailed || accountID <= 0 {
		return
	}
	channel := "ordinary"
	if s.borrowRoutes != nil {
		if enabled, err := s.borrowRoutes.Enabled(ctx, accountID); err == nil && enabled {
			channel = "borrowed"
		}
	}
	proposal := OpenAISessionAffinityBinding{AccountID: accountID, Channel: channel, Version: uuid.NewString(), Pending: true}
	claimCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	b, err := a.cache.ClaimOpenAISessionAffinity(claimCtx, a.key, proposal, min(s.openAIWSSessionStickyTTL(), 5*time.Minute))
	if err != nil {
		a.cacheFailed = true
		return
	}
	a.base = b
	if b != nil && b.AccountID != accountID {
		// A queued first request can lose the claim after it has acquired its
		// slot. It may execute, but must not overwrite the concurrent winner.
		a.mode, a.reason = "temporary_spillover", "concurrent_owner"
	}
}

// A dedicated, exact-account pass reuses all existing eligibility, group,
// budget and concurrency gates. It does not turn sticky affinity into a bypass.
func (s *OpenAIGatewayService) selectWithOpenAISessionAffinity(ctx context.Context, selectOnce, selectDefault openAISelectionCall, model string, excluded map[int64]struct{}) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	a := openAISessionAffinityFromContext(ctx)
	if a == nil {
		return selectDefault(ctx)
	}
	a.load(ctx)
	for attempt := 0; attempt < 2; attempt++ {
		base := a.snapshot()
		mode, reason := "new_session", "unbound"
		var waiting *AccountSelectionResult
		var waitDecision OpenAIAccountScheduleDecision
		if base != nil {
			ownerCtx := context.WithValue(ctx, openAISessionAffinityOnlyKey{}, base.AccountID)
			ownerCtx, ownerError := withBorrowSelectionFailure(ownerCtx)
			selection, decision, err := selectOnce(ownerCtx)
			err = ownerError(err)
			if err == nil && selection != nil && selection.Acquired && selection.Account != nil {
				a.setDecision("owner_hit", "owner_available", selection.Account.ID)
				return selection, decision, nil
			}
			if ctx.Err() != nil {
				return selection, decision, ctx.Err()
			}
			if err != nil && !borrowSchedulingCapacityError(err) {
				return selection, decision, err
			}
			waiting, waitDecision = selection, decision
			mode, reason = "takeover", "owner_unavailable"
			if selection != nil && selection.WaitPlan != nil {
				mode, reason = "temporary_spillover", "account_busy"
			} else if temporary, why := s.openAISessionOwnerTemporarilyBusy(ctx, base.AccountID, model, err, excluded); temporary {
				mode, reason = "temporary_spillover", why
			}
		}
		fallbackCtx := ctx
		if base != nil {
			fallbackCtx = context.WithValue(ctx, openAISessionAffinitySkipKey{}, base.AccountID)
		}
		selection, decision, err := selectDefault(fallbackCtx)
		if waiting != nil && waiting.WaitPlan != nil && (selection == nil || !selection.Acquired) &&
			(err == nil || borrowSchedulingCapacityError(err)) {
			selection, decision, err = waiting, waitDecision, nil
		}
		if err != nil || selection == nil || selection.Account == nil {
			return selection, decision, err
		}
		a.setDecision(mode, reason, selection.Account.ID)
		if selection.Acquired {
			s.admitOpenAISessionAffinity(ctx, a, selection.Account.ID)
			claimed := a.snapshot()
			// A simultaneous first request may have won another account. Try its
			// owner once, releasing our slot first; never duplicate an upstream send.
			if base == nil && claimed != nil && claimed.AccountID != selection.Account.ID && attempt == 0 {
				if selection.ReleaseFunc != nil {
					selection.ReleaseFunc()
				}
				continue
			}
		}
		if base != nil && selection.Account.ID == base.AccountID {
			a.setDecision("owner_hit", "owner_available", selection.Account.ID)
		}
		return selection, decision, nil
	}
	return nil, OpenAIAccountScheduleDecision{}, ErrNoAvailableAccounts
}

func (a *openAISessionAffinity) setDecision(mode, reason string, accountID int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.mode, a.reason, a.selectedID = mode, reason, accountID
}

func (s *OpenAIGatewayService) openAISessionOwnerTemporarilyBusy(ctx context.Context, id int64, model string, selectionErr error, excluded map[int64]struct{}) (bool, string) {
	if a := openAISessionAffinityFromContext(ctx); a != nil {
		a.mu.Lock()
		reason := a.transientFailures[id]
		a.mu.Unlock()
		if reason != "" {
			return true, reason
		}
	}
	if IsOpenAITurnAdmissionError(selectionErr) && !IsBorrowRouteUnavailable(selectionErr) {
		return true, "owner_state_unavailable"
	}
	var admission *BorrowRouteAdmissionError
	if errors.As(selectionErr, &admission) && (admission.Reason == "route_busy" || admission.Reason == "account_busy") {
		return true, admission.Reason
	}
	if errors.Is(selectionErr, ErrOpenAIRPMExhausted) {
		return true, "rpm_busy"
	}
	if s.accountRepo == nil {
		return true, "owner_lookup_failed"
	}
	account, err := s.accountRepo.GetByID(ctx, id)
	if errors.Is(err, ErrAccountNotFound) {
		return false, ""
	}
	if err != nil {
		return true, "owner_lookup_failed"
	}
	if account == nil || !account.IsSchedulable() {
		return false, ""
	}
	// Profit admission remains a transient gate, as in the original sticky logic.
	if vetoed, _ := openAIProfitControlVetoReason(ctx, account); vetoed {
		return true, "profit_gate"
	}
	if _, failed := excluded[id]; failed {
		return false, ""
	}
	if account.IsOpenAIOAuth() {
		allowed, _, err := s.OpenAIRPMSchedulable(ctx, account, true)
		if err != nil {
			return true, "rpm_lookup_failed"
		}
		if !allowed {
			return true, "rpm_busy"
		}
	}
	return false, ""
}

// Admission can race after account selection. Keep the actual local capacity
// reason across the handler's exclusion/reselection loop; an excluded owner is
// not necessarily an upstream failure or permission to migrate the session.
func ObserveOpenAISessionAffinityFailure(ctx context.Context, accountID int64, err error) {
	a := openAISessionAffinityFromContext(ctx)
	if a == nil || err == nil {
		return
	}
	reason := ""
	var admission *BorrowRouteAdmissionError
	if errors.As(err, &admission) && (admission.Reason == "account_busy" || admission.Reason == "route_busy") {
		reason = admission.Reason
	}
	if errors.Is(err, ErrOpenAIRPMExhausted) {
		reason = "rpm_busy"
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.transientFailures == nil {
		a.transientFailures = make(map[int64]string)
	}
	a.transientFailures[accountID] = reason
}

// Only call after a successful forwarding return; nil/partial/cancelled results
// never establish an owner. The deferred abort call records unsuccessful exits.
func (s *OpenAIGatewayService) FinishOpenAISessionAffinity(ctx context.Context, account *Account, result *OpenAIForwardResult) {
	a := openAISessionAffinityFromContext(ctx)
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.finished {
		return
	}
	a.finished = true
	success := ctx.Err() == nil && account != nil && result != nil && !result.ClientDisconnect && result.SucceededForScheduling()
	committed := false
	var commitErr error
	if success && a.base != nil && !a.cacheFailed && a.selectedID == account.ID &&
		(a.mode != "temporary_spillover" || a.base.AccountID == account.ID) {
		next := *a.base
		next.Pending = false
		if next.AccountID != account.ID {
			next.AccountID, next.Version, next.Channel = account.ID, uuid.NewString(), "ordinary"
			if s.borrowRoutes != nil {
				if enabled, err := s.borrowRoutes.Enabled(ctx, account.ID); err == nil && enabled {
					next.Channel = "borrowed"
				}
			}
		}
		commitCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
		committed, commitErr = a.cache.CompleteOpenAISessionAffinity(commitCtx, a.key, *a.base, next, s.openAIWSSessionStickyTTL())
		if committed {
			// Guardian/review subtasks already have a separate parent preference.
			// Keep that hint available without writing tenant-ambiguous legacy keys.
			_ = s.cache.SetSessionAccountID(commitCtx, a.groupID, a.parentKey, account.ID, s.openAIWSSessionStickyTTL())
		}
		cancel()
	}
	ownerID := int64(0)
	if a.base != nil {
		ownerID = a.base.AccountID
	}
	requesttiming.SessionAffinity(ctx, requesttiming.SessionAffinitySnapshot{
		Decision: a.mode, Reason: a.reason, OwnerAccountID: ownerID, ExecutionAccountID: a.selectedID,
		OwnerPending: a.base != nil && a.base.Pending, Success: success, OwnerCommitted: committed, CacheUnavailable: a.cacheFailed || commitErr != nil,
	})
	fields := []any{"trace_id", requesttiming.TraceID(ctx), "session_key_hash", a.key, "group_id", a.groupID, "model", a.model, "decision", a.mode,
		"reason", a.reason, "owner_account_id", ownerID, "execution_account_id", a.selectedID,
		"owner_pending", a.base != nil && a.base.Pending,
		"success", success, "owner_committed", committed, "cache_unavailable", a.cacheFailed || commitErr != nil}
	if result != nil {
		fields = append(fields, "cache_read_tokens", result.Usage.CacheReadInputTokens, "total_input_tokens", result.Usage.InputTokens,
			"cache_creation_tokens", result.Usage.CacheCreationInputTokens)
		if result.FirstTokenMs != nil {
			fields = append(fields, "first_token_ms", *result.FirstTokenMs)
		}
	}
	if commitErr != nil {
		fields = append(fields, "error", commitErr)
	}
	slog.InfoContext(ctx, "openai.session_affinity", fields...)
}

// A parent review can use a different model; this is a tenant-scoped soft hint,
// not the model-specific session owner and never response ownership authority.
func openAIScopedParentAffinityKey(apiKeyID int64, sessionHash string) string {
	return fmt.Sprintf("openai:parent-affinity:v1:%d:%s", apiKeyID, sessionHash)
}
