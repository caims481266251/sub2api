package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

type astraSeamlessMaintainer interface {
	MaintainAstraGatewayTarget(context.Context, *http.Request, string, int64, int)
	BlockAstraGatewayTarget(context.Context, int64, string)
	StopAstraSeamless()
}

// Fresh credentials/egress are resolved for every maintenance cycle. No business
// body, source credential or probe-issued turn-state is retained by the worker.
func (s *AccountTestService) astraTargetTemplate(ctx context.Context, id int64) (*http.Request, string, int, error) {
	if s.accountRepo == nil || s.openaiGatewayService == nil {
		return nil, "", 0, errors.New("runtime_unavailable")
	}
	account, err := s.accountRepo.GetByID(ctx, id)
	if err != nil || account == nil || !account.IsOpenAIOAuthLike() || account.Status != StatusActive || (account.ExpiresAt != nil && !time.Now().Before(*account.ExpiresAt)) {
		return nil, "", 0, errors.New("account_unavailable")
	}
	if openAICodexStateProbeUnsupportedReason(account, "gpt-6-astra", false) != "" {
		return nil, "", 0, errors.New("target_probe_unsupported")
	}
	token, _, err := s.openaiGatewayService.GetAccessToken(ctx, account)
	if err != nil || strings.TrimSpace(token) == "" {
		return nil, "", 0, errors.New("account_unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatgptCodexURL, nil)
	if err != nil {
		return nil, "", 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if err = resolveAndSetOpenAIChatGPTAccountHeaders(ctx, s.accountRepo, req.Header, account); err != nil {
		return nil, "", 0, errors.New("account_unavailable")
	}
	applyOpenAICodexTicketHarvestIdentity(req.Header, "gpt-6-astra")
	return req, openAIAccountProxyURL(account), account.Concurrency, nil
}

func (s *AccountTestService) startAstraSeamlessMaintenance() func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	provider, ok := s.httpUpstream.(astraSeamlessMaintainer)
	if !ok {
		close(done)
		return cancel
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		cursor := 0
		for {
			settings := s.cfg.AstraRouting(ctx)
			if settings.CookiePool.Enabled && settings.CookiePool.Seamless {
				ids := settings.CookiePool.TargetAccountIDs
				for offset := range len(ids) {
					id := ids[(cursor+offset)%len(ids)]
					cycle, stop := context.WithTimeout(ctx, 5*time.Second)
					req, proxy, n, err := s.astraTargetTemplate(cycle, id)
					if err == nil {
						provider.MaintainAstraGatewayTarget(cycle, req, proxy, id, n)
					} else if ctx.Err() == nil {
						provider.BlockAstraGatewayTarget(cycle, id, err.Error())
					}
					stop()
					if ctx.Err() != nil {
						return
					}
				}
				cursor++
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() { cancel(); provider.StopAstraSeamless(); <-done }
}
