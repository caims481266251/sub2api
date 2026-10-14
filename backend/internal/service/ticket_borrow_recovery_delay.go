package service

import (
	"context"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
)

// SetRecoveryDelay retains the failed target's Retry-After even when ordinary
// selection later falls back to that target. Unknown/shared limits stay scoped
// to the provider; changing credentials alone is not proof of independence.
func (r *BorrowBusinessRecovery) SetRecoveryDelay(account *Account, failure *UpstreamFailoverError, delay time.Duration) {
	if r == nil || account == nil || failure == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.recovering || !r.retryNotBefore.IsZero() {
		return
	}
	now := time.Now()
	r.retryAccountID = account.ID
	r.retryIdentity = borrowRecoveryIdentity(account)
	// A local route cooldown is not an OpenAI-wide ban. An explicit or
	// request/provider-scoped wait retains the conservative shared behavior.
	r.retryLocal = failure.StatusCode == 429 && failure.ShouldRetryNextAccount() && strings.TrimSpace(failure.ResponseHeaders.Get("Retry-After")) == "" && failure.Scope != GatewayFailureScopeProvider && failure.Scope != GatewayFailureScopeRequest && !failure.RequestScopedTransient
	r.retryProvider = borrowRecoveryProvider(account)
	r.retryIndependentProvider = failure.ShouldRetryNextAccount() && !failure.RequestScopedTransient && failure.Scope != GatewayFailureScopeProvider && failure.Scope != GatewayFailureScopeRequest
	r.retryNotBefore = now.Add(delay)
	r.independentNotBefore = now.Add(min(delay, 750*time.Millisecond))
}

// WaitForAccount runs before ticket pinning or any upstream send. It also covers
// same-target fallback after the scheduler fails to find another account.
func (r *BorrowBusinessRecovery) WaitForAccount(ctx context.Context, account *Account, ordinary bool) error {
	if r == nil || account == nil {
		return nil
	}
	if err := r.CheckTarget(account.ID); err != nil {
		return err
	}
	r.mu.Lock()
	until := r.retryNotBefore
	if r.retryLocal && r.retryIdentity != "" && borrowRecoveryIdentity(account) != "" && account.ID != r.retryAccountID && borrowRecoveryIdentity(account) != r.retryIdentity {
		until = r.independentNotBefore
	}
	if ordinary && account.Type == AccountTypeAPIKey && account.ID != r.retryAccountID && r.retryIndependentProvider {
		provider := borrowRecoveryProvider(account)
		if provider != "" && r.retryProvider != "" && provider != r.retryProvider {
			until = r.independentNotBefore
		}
	}
	r.mu.Unlock()
	if until.IsZero() {
		return nil
	}
	ctx = r.Context(ctx)
	observe := requesttiming.Observe(ctx, "borrow_retry_wait")
	defer observe()
	timer := time.NewTimer(time.Until(until))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		if err := ctx.Err(); err != nil {
			return context.Cause(ctx)
		}
		return r.CheckTarget(account.ID)
	}
}

func borrowRecoveryProvider(account *Account) string {
	if account == nil || account.Platform != PlatformOpenAI {
		return ""
	}
	if account.IsOpenAIOAuthLike() {
		return "openai.com"
	}
	if account.Type != AccountTypeAPIKey {
		return ""
	}
	base := strings.TrimSpace(account.GetCredential("base_url"))
	if base == "" {
		base = "https://api.openai.com"
	}
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if domain, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
		host = domain
	}
	if host == "chatgpt.com" {
		host = "openai.com"
	}
	return host
}

func borrowRecoveryIdentity(a *Account) string {
	if a == nil || !a.IsOpenAIOAuthLike() || a.GetCredential("chatgpt_account_id") == "" {
		return ""
	}
	return borrowSubjectHash(a)
}
