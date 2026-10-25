package service

import (
	"context"
	"net/http"
)

// Only borrowing installs this observer. Standalone diagnostic probes remain
// read-only. State is attributed to the account whose credentials were sent.
func (s *TicketBorrowService) borrowShotObserver(account *Account, model string, sentTokens ...string) func(context.Context, openAICodexStateShot) {
	if account == nil {
		return func(context.Context, openAICodexStateShot) {}
	}
	// Capture values, not mutable credential maps retained by another worker.
	identity, access, refresh := borrowAccountIdentity(account), account.GetOpenAIAccessToken(), borrowCredential(account.Credentials, "refresh_token")
	sent := access
	if len(sentTokens) > 0 {
		sent = sentTokens[0]
	}
	return func(ctx context.Context, shot openAICodexStateShot) {
		gateway := s.owner.openaiGatewayService
		if gateway == nil || account == nil || (shot.status < 400 && len(shot.errorBody) == 0) {
			return
		}
		stateCtx, cancel := openAIAccountStateContext(ctx)
		defer cancel()
		latest, err := s.owner.accountRepo.GetByID(stateCtx, account.ID)
		if err != nil || latest == nil {
			return
		}
		// A late response must not poison a newly authorized/refreshed credential.
		if latest.GetOpenAIAccessToken() != access || sent != access ||
			borrowCredential(latest.Credentials, "refresh_token") != refresh || borrowAccountIdentity(latest) != identity {
			return
		}
		stateCtx = context.WithValue(stateCtx, openAIUpstreamFinalAttemptKey{}, true)
		if shot.status >= http.StatusBadRequest {
			gateway.handleOpenAIAccountUpstreamError(stateCtx, latest, shot.status, shot.headers, shot.errorBody, model)
		} else if len(shot.errorBody) > 0 {
			gateway.handleOpenAIStreamTerminalAccountSideEffectsContext(stateCtx, latest, shot.errorBody, extractUpstreamErrorMessage(shot.errorBody), shot.headers, model)
		}
	}
}
