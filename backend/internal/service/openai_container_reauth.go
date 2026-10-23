package service

import (
	"context"
	"log/slog"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// Authentication is an account concern, independent of container health and
// the administrator's scheduling switch. These markers are owned by this flow.
const OpenAIContainerAuthRequired = "OpenAI authentication expired (401); 2FA reauthorization required"
const OpenAIContainerTwoFAEnrollmentPending = "2FA login information has not been saved; retry account enrollment"

type OpenAIContainerReauthRepository interface {
	QueueContainerReauth(context.Context, *Account, string, bool) (bool, error)
	CompleteContainerTwoFAEnrollment(context.Context, int64) error
}

func validateContainerTwoFAIdentity(account *Account, email string) error {
	if account.GetCredential("chatgpt_account_id") == "" && account.GetCredential("chatgpt_user_id") == "" {
		return infraerrors.BadRequest("OPENAI_REAUTH_IDENTITY_MISSING", "A stable OpenAI account identity is required")
	}
	if expected := strings.TrimSpace(account.GetCredential("email")); expected == "" || !strings.EqualFold(expected, strings.TrimSpace(email)) {
		return infraerrors.Conflict("OPENAI_REAUTH_EMAIL_MISMATCH", "Login email does not match the account")
	}
	return nil
}

func (s *OpenAIOAuthReauthService) completeContainerTwoFAEnrollment(ctx context.Context, id int64) error {
	account, err := s.accountFor(ctx, id)
	if err != nil {
		return err
	}
	if !account.IsVMKernel() || account.ErrorMessage != OpenAIContainerTwoFAEnrollmentPending {
		return nil
	}
	repo, ok := s.repo.(OpenAIContainerReauthRepository)
	if !ok {
		return infraerrors.ServiceUnavailable("OPENAI_REAUTH_UNAVAILABLE", "Account enrollment is unavailable")
	}
	return repo.CompleteContainerTwoFAEnrollment(ctx, id)
}

// Called only after the kernel has classified a provider response, never for
// internal kernel HTTP authentication or incoming client/API-key failures.
// Queueing is durable and short; the existing worker does the actual login.
func (s *OpenAIOAuthReauthService) handleContainerUnauthorized(ctx context.Context, account *Account, dispatchedHash string) bool {
	if s == nil || account == nil || !account.IsVMKernel() || account.Platform != PlatformOpenAI || account.Type != AccountTypeOAuth || account.IsCredentialShadow() || account.IsSIWCEnabled() {
		return false
	}
	repo, ok := s.repo.(OpenAIContainerReauthRepository)
	if !ok {
		return false
	}
	handled, err := repo.QueueContainerReauth(ctx, account, dispatchedHash, false)
	if err != nil {
		// No credential material or database/provider error text enters logs.
		slog.Warn("openai_container_reauth_queue_failed", "account_id", account.ID)
		if s.runtimeBlocker != nil {
			s.runtimeBlocker.BlockAccountScheduling(account, time.Now().Add(time.Minute), "oauth_401")
		}
		return true
	}
	if handled {
		s.EnsureWorker()
	}
	return handled
}
