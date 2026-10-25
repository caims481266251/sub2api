//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/vmkernel"
	"github.com/stretchr/testify/require"
)

// Exact production rejection shape: no reset metadata or quota/retry headers.
const openAIUsageLimitWithoutReset = `{"error":{"code":"usage_limit_reached","type":"api_error","message":"The usage limit has been reached"}}`

func usageLimitSnapshotAccount(now time.Time) (*Account, time.Time) {
	now = now.UTC()
	reset := now.Add(6 * 24 * time.Hour).Truncate(time.Second)
	return &Account{ID: 995514, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
		Extra: map[string]any{
			"codex_usage_updated_at": now.Add(-time.Minute).Format(time.RFC3339),
			"codex_5h_used_percent":  0.0, "codex_5h_reset_at": now.Format(time.RFC3339),
			"codex_7d_used_percent": 100.0, "codex_7d_window_minutes": 10080,
			"codex_7d_reset_at": reset.Format(time.RFC3339),
		}}, reset
}

func TestOpenAIUsageLimitSnapshotRequiresRecentExplicitExhaustion(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		name   string
		body   string
		change func(*Account)
		want   bool
	}{
		{name: "production rejection", want: true},
		{name: "setup token", change: func(a *Account) { a.Type = AccountTypeSetupToken }, want: true},
		{name: "nested explicit type", body: `{"type":"response.failed","response":{"error":{"type":"usage_limit_reached"}}}`, want: true},
		{name: "rate limit is not quota", body: `{"error":{"code":"rate_limit_exceeded","type":"rate_limit_error"}}`},
		{name: "message is not quota", body: `{"error":{"type":"api_error","message":"The usage limit has been reached"}}`},
		{name: "local capacity", body: `{"error":{"code":"no_capacity"}}`},
		{name: "no rejection", body: `{}`},
		{name: "truncated JSON", body: `{"error":{"code":"usage_limit_reached"}`},
		{name: "API key relay", change: func(a *Account) { a.Type = AccountTypeAPIKey }},
		{name: "shadow", change: func(a *Account) { parent := int64(4); a.ParentAccountID = &parent }},
		{name: "SIWC", change: func(a *Account) { a.Extra[OpenAISIWCEnabledKey] = true }},
		{name: "TTL expired", change: func(a *Account) {
			a.Extra["codex_usage_updated_at"] = now.Add(-openAIUsageLimitSnapshotMaxAge).Format(time.RFC3339)
		}},
		{name: "future sample", change: func(a *Account) { a.Extra["codex_usage_updated_at"] = now.Add(time.Second).Format(time.RFC3339) }},
		{name: "missing sample time", change: func(a *Account) { delete(a.Extra, "codex_usage_updated_at") }},
		{name: "invalid sample time", change: func(a *Account) { a.Extra["codex_usage_updated_at"] = "invalid" }},
		{name: "not exhausted", change: func(a *Account) { a.Extra["codex_7d_used_percent"] = 99.9 }},
		{name: "invalid usage", change: func(a *Account) { a.Extra["codex_7d_used_percent"] = math.Inf(1) }},
		{name: "expired reset", change: func(a *Account) { a.Extra["codex_7d_reset_at"] = now.Format(time.RFC3339) }},
		{name: "invalid reset", change: func(a *Account) { a.Extra["codex_7d_reset_at"] = "invalid" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account, expected := usageLimitSnapshotAccount(now)
			if tc.change != nil {
				tc.change(account)
			}
			body := tc.body
			if body == "" {
				body = openAIUsageLimitWithoutReset
			}
			reset := openAIUsageLimitSnapshotResetAt(account, []byte(body), now)
			if !tc.want {
				require.Nil(t, reset)
				return
			}
			require.NotNil(t, reset)
			require.True(t, reset.Equal(expected))
		})
	}
	account, _ := usageLimitSnapshotAccount(now)
	delete(account.Extra, "codex_7d_reset_at")
	account.Extra["codex_7d_reset_after_seconds"] = 3600
	reset := openAIUsageLimitSnapshotResetAt(account, []byte(openAIUsageLimitWithoutReset), now)
	require.NotNil(t, reset)
	require.Equal(t, now.Add(59*time.Minute), *reset, "relative reset stays anchored to the one-minute-old sample")
}

func TestOpenAIUsageLimitWithoutResetPersistsSameDeadline(t *testing.T) {
	for _, mode := range []string{AccountExecutionModeNormal, AccountExecutionModeDockerKernel} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", mode, stream), func(t *testing.T) {
				account, reset := usageLimitSnapshotAccount(time.Now())
				account.ExecutionMode = mode
				repo := &oauth429RateLimitRepo{}
				limits := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
				svc := &OpenAIGatewayService{rateLimitService: limits}
				limits.SetAccountRuntimeBlocker(svc)
				body := []byte(openAIUsageLimitWithoutReset)
				disposition, classifiedReset := classifyOpenAIOAuth429(account, nil, body)
				require.Equal(t, openAIOAuth429QuotaReset, disposition)
				require.NotNil(t, classifiedReset)
				require.Equal(t, reset, *classifiedReset)
				require.False(t, svc.ShouldRetryOpenAIOAuth429(account, nil, body))
				require.False(t, svc.shouldRetryOpenAIOAuth429OnSameAccountWithResponse(account, 429, false, nil, body))
				if stream {
					body = []byte(`{"type":"response.failed","response":` + openAIUsageLimitWithoutReset + `}`)
				}
				if mode == AccountExecutionModeDockerKernel {
					socket := vmForwardUDS(t, func(w http.ResponseWriter, _ *http.Request) {
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							fmt.Fprintf(w, "event: response\ndata: %s\n\n", body)
						} else {
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(http.StatusTooManyRequests)
							_, _ = w.Write(body)
						}
					})
					client := vmkernel.NewClient()
					t.Cleanup(client.Close)
					result, err := client.Execute(context.Background(), vmkernel.Binding{Socket: socket, Token: "test", Generation: 1}, vmkernel.Request{Protocol: "responses", Body: json.RawMessage(`{}`)}, nil)
					require.Error(t, err)
					require.NotNil(t, result)
					require.Equal(t, !stream, result.ProviderHTTPFailure)
					svc.observeVMKernelOpenAIResult(context.Background(), nil, account, "gpt-6-luna", result)
				} else if stream {
					svc.handleOpenAIWSFailureAccountSideEffects(context.Background(), account, "gpt-6-luna", nil, body)
				} else {
					svc.handleOpenAIAccountUpstreamError(context.Background(), account, 429, nil, body, "gpt-6-luna")
				}
				require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
				require.Equal(t, 1, repo.setRateLimitedCalls)
				require.Equal(t, reset, repo.lastRateLimitedUntil)
				require.Equal(t, StatusActive, account.Status)
				require.True(t, account.Schedulable)
			})
		}
	}
}

func TestOpenAIUsageLimitSnapshotDoesNotChangeOther429Policies(t *testing.T) {
	for _, scenario := range []string{"rate limit", "API key relay", "Spark model", "successful snapshot"} {
		t.Run(scenario, func(t *testing.T) {
			account, _ := usageLimitSnapshotAccount(time.Now())
			repo := &oauth429RateLimitRepo{}
			limits := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
			svc := &OpenAIGatewayService{rateLimitService: limits}
			limits.SetAccountRuntimeBlocker(svc)
			body := []byte(openAIUsageLimitWithoutReset)
			model := "gpt-6-luna"
			switch scenario {
			case "rate limit":
				body = []byte(`{"error":{"code":"rate_limit_exceeded","message":"Too many requests per minute"}}`)
			case "API key relay":
				account.Type = AccountTypeAPIKey
			case "Spark model":
				model = "gpt-5.3-codex-spark"
			case "successful snapshot":
				svc.observeVMKernelOpenAIResult(context.Background(), nil, account, model, &vmkernel.Result{Completed: true})
			}
			if scenario != "successful snapshot" {
				svc.handleOpenAIAccountUpstreamError(context.Background(), account, 429, nil, body, model)
			}
			if scenario == "API key relay" {
				require.Equal(t, 1, repo.setRateLimitedCalls)
				require.WithinDuration(t, time.Now().Add(defaultRateLimit429CooldownSeconds*time.Second), repo.lastRateLimitedUntil, time.Second)
			} else {
				require.Zero(t, repo.setRateLimitedCalls)
				require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
			}
			if scenario == "Spark model" {
				require.Equal(t, 1, repo.setModelRateLimitCalls)
				require.WithinDuration(t, time.Now().Add(defaultRateLimit429CooldownSeconds*time.Second), repo.lastModelRateLimitedUntil, time.Second)
			}
		})
	}
}

func TestOpenAIUsageLimitSnapshotManagementAndExplicitReset(t *testing.T) {
	account, reset := usageLimitSnapshotAccount(time.Now())
	repo := &openAIAccountTestRepo{}
	tests := &AccountTestService{accountRepo: repo}
	tests.reconcileOpenAI429State(context.Background(), account, nil, []byte(openAIUsageLimitWithoutReset))
	require.NotNil(t, repo.rateLimitedAt)
	require.Equal(t, reset, *repo.rateLimitedAt)
	require.Equal(t, reset, *account.RateLimitResetAt)
	require.Equal(t, StatusActive, account.Status)
	require.True(t, account.Schedulable)

	explicit := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	body := []byte(fmt.Sprintf(`{"error":{"type":"usage_limit_reached","resets_at":%d}}`, explicit.Unix()))
	disposition, actual := classifyOpenAIOAuth429(account, nil, body)
	require.Equal(t, openAIOAuth429QuotaReset, disposition)
	require.NotNil(t, actual)
	require.True(t, explicit.Equal(*actual), "the rejection's own reset has priority over the cached window")
}
