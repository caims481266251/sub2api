//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/vmkernel"
	"github.com/Wei-Shaw/sub2api/internal/vmruntime"
	"github.com/stretchr/testify/require"
)

func TestVMKernelQuotaUsesOrdinaryAccountState(t *testing.T) {
	reset := time.Now().Add(4 * time.Hour).Truncate(time.Second)
	provider := fmt.Sprintf(`{"error":{"type":"usage_limit_reached","resets_at":%d,"resets_in_seconds":14400}}`, reset.Unix())
	wrapper, err := json.Marshal(map[string]any{"error": map[string]any{"code": "usage_limit_reached", "type": "api_error", "message": "upstream status 429: " + provider}})
	require.NoError(t, err)
	for _, tc := range []struct {
		name, body string
		headers    http.Header
		limited    bool
	}{
		{"wrapped quota", string(wrapper), nil, true},
		{"relative quota", `{"error":{"code":"usage_limit_reached","type":"usage_limit_reached","resets_in_seconds":"14400"}}`, nil, true},
		{"header quota", `{"error":{"code":"usage_limit_reached","message":"Rate limit exceeded"}}`, http.Header{
			"X-Codex-Primary-Used-Percent": {"100"}, "X-Codex-Primary-Reset-After-Seconds": {"14400"}, "X-Codex-Primary-Window-Minutes": {"300"},
		}, true},
		{"transient 429", `{"error":{"code":"usage_limit_reached","message":"Rate limit exceeded"}}`, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socket := vmForwardUDS(t, func(w http.ResponseWriter, r *http.Request) {
				for k, values := range tc.headers {
					for _, v := range values {
						w.Header().Add(k, v)
					}
				}
				w.WriteHeader(429)
				fmt.Fprint(w, tc.body)
			})
			result, err := vmkernel.NewClient().Execute(context.Background(), vmkernel.Binding{Socket: socket, Token: "test", Generation: 1}, vmkernel.Request{Protocol: "responses", Body: json.RawMessage(`{}`)}, nil)
			require.Error(t, err)
			require.NotNil(t, result)
			require.True(t, result.ProviderHTTPFailure)
			repo := &oauth429RateLimitRepo{}
			limits := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
			svc := &OpenAIGatewayService{rateLimitService: limits}
			limits.SetAccountRuntimeBlocker(svc)
			account := &Account{ID: 995511, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, ExecutionMode: AccountExecutionModeVMKernel, Credentials: map[string]any{"access_token": "fixture-token"}}
			state := vmRuntimeStatus{state: vmruntime.State{Ready: true, Healthy: true, Phase: "kernel_ready"}, credentials: vmCredentialRevision(account), proxy: vmProxyRevision(account), checked: time.Now()}
			vmKernelReadiness.Store(account.ID, state)
			vmKernelHealth.Store(account.ID, state)
			t.Cleanup(func() { vmKernelReadiness.Delete(account.ID); vmKernelHealth.Delete(account.ID) })
			before := VMKernelRuntimeSummary(account)
			svc.observeVMKernelOpenAIResult(context.Background(), nil, account, "gpt-6-luna", result)
			require.Equal(t, tc.limited, svc.isOpenAIAccountRuntimeBlocked(account))
			require.Equal(t, before, VMKernelRuntimeSummary(account), "business quota must not change Docker health")
			require.True(t, VMKernelRuntimeReady(account))
			require.True(t, account.Schedulable, "manual scheduling setting must survive a timed quota block")
			require.Equal(t, StatusActive, account.Status)

			ordinaryRepo := &oauth429RateLimitRepo{}
			ordinaryLimits := NewRateLimitService(ordinaryRepo, nil, &config.Config{}, nil, nil)
			ordinarySvc := &OpenAIGatewayService{rateLimitService: ordinaryLimits}
			ordinaryLimits.SetAccountRuntimeBlocker(ordinarySvc)
			ordinary := *account
			ordinary.ID++
			ordinary.ExecutionMode = AccountExecutionModeNormal
			ordinaryBody := []byte(tc.body)
			if tc.name == "wrapped quota" {
				ordinaryBody = []byte(provider)
			}
			ordinarySvc.handleOpenAIAccountUpstreamError(context.Background(), &ordinary, 429, tc.headers, ordinaryBody, "gpt-6-luna")
			require.Equal(t, ordinaryRepo.setRateLimitedCalls, repo.setRateLimitedCalls)
			require.Equal(t, ordinarySvc.isOpenAIAccountRuntimeBlocked(&ordinary), svc.isOpenAIAccountRuntimeBlocked(account))
			if tc.limited {
				require.Equal(t, 1, repo.setRateLimitedCalls)
				require.WithinDuration(t, reset, repo.lastRateLimitedUntil, 2*time.Second)
				require.WithinDuration(t, ordinaryRepo.lastRateLimitedUntil, repo.lastRateLimitedUntil, time.Second)
				// Model a subsequent repository/cache snapshot with the shared field.
				account.RateLimitResetAt = &repo.lastRateLimitedUntil
				require.False(t, account.IsSchedulable())
				expired := time.Now().Add(-time.Second)
				account.RateLimitResetAt = &expired
				require.True(t, account.IsSchedulable(), "ordinary deadline expiry restores eligibility")
				account.Schedulable = false
				require.False(t, account.IsSchedulable(), "expiry must not override a manual pause")
			} else {
				require.Zero(t, repo.setRateLimitedCalls)
			}
		})
	}
}

func TestVMKernelLocalFailuresDoNotChangeAccountQuota(t *testing.T) {
	for _, code := range []string{"no_capacity", "no_credential", "internal_auth_failed", "upstream_transport"} {
		t.Run(code, func(t *testing.T) {
			status := 503
			if code == "internal_auth_failed" {
				status = 401
			}
			socket := vmForwardUDS(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				fmt.Fprintf(w, `{"error":{"code":%s,"message":"local failure"}}`, strconv.Quote(code))
			})
			result, err := vmkernel.NewClient().Execute(context.Background(), vmkernel.Binding{Socket: socket, Token: "test", Generation: 1}, vmkernel.Request{Protocol: "responses", Body: json.RawMessage(`{}`)}, nil)
			require.Error(t, err)
			require.NotNil(t, result)
			repo := &oauth429RateLimitRepo{}
			limits := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
			svc := &OpenAIGatewayService{rateLimitService: limits}
			limits.SetAccountRuntimeBlocker(svc)
			account := &Account{ID: 995512, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, ExecutionMode: AccountExecutionModeVMKernel, Credentials: map[string]any{"access_token": "fixture-token"}}
			svc.observeVMKernelOpenAIResult(context.Background(), nil, account, "gpt-6-luna", result)
			require.Zero(t, repo.setRateLimitedCalls)
			require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
			require.Equal(t, StatusActive, account.Status)
		})
	}
}

func TestOpenAIStreamQuotaResetUsesOrdinaryAccountState(t *testing.T) {
	reset := time.Now().Add(4 * time.Hour).Truncate(time.Second)
	for _, tc := range []struct {
		name, payload string
		limited       bool
	}{
		{"top-level quota type", fmt.Sprintf(`{"type":"error","error":{"type":"usage_limit_reached","resets_at":%d}}`, reset.Unix()), true},
		{"nested quota type", fmt.Sprintf(`{"type":"response.failed","response":{"error":{"type":"usage_limit_reached","resets_at":%d}}}`, reset.Unix()), true},
		{"top-level quota code", fmt.Sprintf(`{"type":"error","error":{"type":"api_error","code":"usage_limit_reached","resets_at":"%d"}}`, reset.Unix()), true},
		{"nested quota code", `{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded","resets_in_seconds":"14400"}}}`, true},
		{"transient rate limit", `{"type":"error","error":{"code":"rate_limit_exceeded","message":"Please retry later"}}`, false},
		{"local capacity", `{"type":"error","error":{"code":"no_capacity","resets_in_seconds":14400}}`, false},
	} {
		for _, mode := range []string{AccountExecutionModeNormal, AccountExecutionModeDockerKernel} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				repo := &oauth429RateLimitRepo{}
				limits := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
				svc := &OpenAIGatewayService{rateLimitService: limits}
				limits.SetAccountRuntimeBlocker(svc)
				account := &Account{ID: 995513, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
					Status: StatusActive, Schedulable: true, ExecutionMode: mode}
				// A successful stream's quota snapshot is not an account rejection.
				// Only the error's explicit reset may establish a long quota block.
				headers := http.Header{
					"X-Codex-Secondary-Used-Percent":        {"100"},
					"X-Codex-Secondary-Reset-After-Seconds": {"14400"},
					"X-Codex-Secondary-Window-Minutes":      {"10080"},
				}
				if mode == AccountExecutionModeDockerKernel {
					socket := vmForwardUDS(t, func(w http.ResponseWriter, r *http.Request) {
						for key, values := range headers {
							w.Header()[key] = values
						}
						w.Header().Set("Content-Type", "text/event-stream")
						// The kernel wraps upstream WebSocket events in generic SSE.
						fmt.Fprintf(w, "event: response\ndata: %s\n\n", tc.payload)
					})
					client := vmkernel.NewClient()
					t.Cleanup(client.Close)
					result, err := client.Execute(context.Background(), vmkernel.Binding{Socket: socket, Token: "test", Generation: 1}, vmkernel.Request{Protocol: "responses", Body: json.RawMessage(`{}`)}, nil)
					require.Error(t, err)
					require.NotNil(t, result)
					require.True(t, result.Settled)
					require.False(t, result.ProviderHTTPFailure, "exercise the stream account-state path")
					require.JSONEq(t, tc.payload, string(result.ErrorEvent))
					svc.observeVMKernelOpenAIResult(context.Background(), nil, account, "gpt-6-luna", result)
				} else {
					svc.handleOpenAIWSFailureAccountSideEffects(context.Background(), account, "gpt-6-luna", headers, []byte(tc.payload))
				}
				require.Equal(t, tc.limited, svc.isOpenAIAccountRuntimeBlocked(account))
				require.Equal(t, StatusActive, account.Status, "a timed quota block must preserve account enablement")
				require.True(t, account.Schedulable, "a timed quota block must preserve manual scheduling")
				if tc.limited {
					require.Equal(t, 1, repo.setRateLimitedCalls)
					require.WithinDuration(t, reset, repo.lastRateLimitedUntil, 2*time.Second)
				} else {
					require.Zero(t, repo.setRateLimitedCalls, "transient and local failures must not inherit the quota snapshot")
				}
			})
		}
	}
}
