package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func installDockerKernelTestRuntime(t *testing.T, runtime *VMKernelService) {
	t.Helper()
	previous, loaded := vmKernelServices.Swap(PlatformOpenAI, runtime)
	t.Cleanup(func() {
		if loaded {
			vmKernelServices.Store(PlatformOpenAI, previous)
		} else {
			vmKernelServices.Delete(PlatformOpenAI)
		}
	})
}

func TestDockerKernelUsageProbeAndCredentialGuardUseBoundKernel(t *testing.T) {
	var calls atomic.Int32
	runtime, account := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/internal/v1/codex/responses", r.URL.Path)
		require.Equal(t, "fixture-internal", r.Header.Get("x-kin-internal-token"))
		var envelope struct {
			Body json.RawMessage `json:"body"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&envelope))
		require.Equal(t, "gpt-6.1-sol", gjson.GetBytes(envelope.Body, "model").String())
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("x-codex-primary-used-percent", "25")
		w.Header().Set("x-codex-primary-window-minutes", "300")
		for _, event := range vmForwardEvents("responses") {
			fmt.Fprintf(w, "data: %s\n\n", event)
		}
	}, func(a *Account) {
		a.Schedulable = false
		a.Credentials["model_mapping"] = map[string]any{"gpt-6.1-sol": "gpt-6.1-sol"}
	})
	installDockerKernelTestRuntime(t, runtime)
	usage := &AccountUsageService{}
	updates, err := usage.probeOpenAICodexSnapshot(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, float64(25), updates["codex_5h_used_percent"])
	// No model-list prober or host HTTP client is configured. Success therefore
	// requires an actual second kernel execution, never a cached model catalog.
	guard := &AccountTokenGuardV2Service{}
	state, _ := guard.probeAccount(context.Background(), account)
	require.Equal(t, AccountTokenGuardV2ProbeOK, state)
	legacy := (&AccountTokenGuardService{}).probe(context.Background(), AccountTokenGuardConfig{ProbeModel: "gpt-6.1-sol", ProbeTimeoutSeconds: 10}, account)
	require.Equal(t, AccountTokenGuardProbeOK, legacy.State)
	require.Equal(t, int32(3), calls.Load())
	require.False(t, account.Schedulable)
}

func TestDockerKernelUsageProbeRecoveryRequiresCompletedStream(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(fmt.Sprintf("completed_%v", completed), func(t *testing.T) {
			runtime, account := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/cancel") {
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("x-codex-primary-used-percent", "0")
				w.Header().Set("x-codex-primary-window-minutes", "300")
				w.Header().Set("x-codex-secondary-used-percent", "0")
				w.Header().Set("x-codex-secondary-window-minutes", "10080")
				events := vmForwardEvents("responses")
				if !completed {
					events = events[:len(events)-1]
				}
				for _, event := range events {
					fmt.Fprintf(w, "data: %s\n\n", event)
				}
			}, func(a *Account) {
				limited, reset := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
				a.RateLimitedAt, a.RateLimitResetAt = &limited, &reset
			})
			installDockerKernelTestRuntime(t, runtime)
			repo := &quotaRecoveryTestRepo{account: *account}
			svc := &AccountUsageService{accountRepo: repo}
			updates, err := svc.probeOpenAICodexSnapshot(context.Background(), account)
			require.NoError(t, err, "error streams may still update the display snapshot")
			require.Equal(t, float64(0), updates["codex_5h_used_percent"])
			require.Eventually(t, func() bool {
				repo.mu.Lock()
				defer repo.mu.Unlock()
				return repo.writes > 0 && (!completed || repo.clears > 0)
			}, time.Second, time.Millisecond)
			repo.mu.Lock()
			defer repo.mu.Unlock()
			require.Equal(t, completed, repo.account.RateLimitResetAt == nil)
		})
	}
}

func TestDockerKernelManagementNeverFallsBackWithoutRuntime(t *testing.T) {
	previous, loaded := vmKernelServices.LoadAndDelete(PlatformOpenAI)
	t.Cleanup(func() {
		if loaded {
			vmKernelServices.Store(PlatformOpenAI, previous)
		}
	})
	account := &Account{ID: 123, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ExecutionMode: AccountExecutionModeDockerKernel}
	_, err := (&AccountUsageService{}).probeOpenAICodexSnapshot(context.Background(), account)
	require.ErrorContains(t, err, "kernel runtime is unavailable")
	state, _ := (&AccountTokenGuardV2Service{}).probeAccount(context.Background(), account)
	require.Equal(t, AccountTokenGuardV2ProbeTransient, state)
	legacy := (&AccountTokenGuardService{}).probe(context.Background(), AccountTokenGuardConfig{ProbeTimeoutSeconds: 10}, account)
	require.Equal(t, AccountTokenGuardProbeTransient, legacy.State)
}

func TestDockerKernelCredentialProbesOnlyTreatProvider401AsAuthFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		event  string
		auth   bool
	}{
		{"provider unauthorized", 200, `{"type":"error","error":{"status":401,"message":"Unauthorized"}}`, true},
		{"token revoked", 200, `{"type":"response.failed","response":{"error":{"code":"token_revoked","message":"revoked"}}}`, true},
		{"model permission", 200, `{"type":"error","error":{"status":403,"code":"permission_denied"}}`, false},
		{"quota exhausted", 200, `{"type":"error","error":{"status":429,"code":"usage_limit_reached"}}`, false},
		{"transport failure", 200, `{"type":"error","error":{"status":502,"code":"upstream_transport"}}`, false},
		{"kernel socket unauthorized", 401, `{"error":"unauthorized"}`, false},
		{"incomplete stream", 200, `{"type":"response.created","response":{"id":"resp_fixture","status":"in_progress"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime, account := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/cancel") {
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(tc.status)
				if tc.status != http.StatusOK {
					fmt.Fprint(w, tc.event)
					return
				}
				fmt.Fprintf(w, "data: %s\n\n", tc.event)
			})
			installDockerKernelTestRuntime(t, runtime)
			want := AccountTokenGuardProbeTransient
			if tc.auth {
				want = AccountTokenGuardProbeAuth
			}
			legacy := (&AccountTokenGuardService{}).probe(context.Background(), AccountTokenGuardConfig{ProbeTimeoutSeconds: 10}, account)
			require.Equal(t, want, legacy.State)
			state, _ := (&AccountTokenGuardV2Service{}).probeAccount(context.Background(), account)
			require.Equal(t, want, state)
		})
	}
}

func TestDockerKernelLegacyGuardPreservesConfirmedCredentialFailure(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, ExecutionMode: AccountExecutionModeDockerKernel,
		Status: StatusError, ErrorMessage: "Token revoked (401): token has been revoked"}
	result := (&AccountTokenGuardService{}).probe(context.Background(), AccountTokenGuardConfig{}, account)
	require.Equal(t, AccountTokenGuardProbeAuth, result.State)
	require.Equal(t, "account_auth_rejected", result.Diagnostic.Code)
}

func TestDockerKernelUsageSnapshotTTLDoesNotDependOnHostWebSocketMode(t *testing.T) {
	now := time.Now()
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, ExecutionMode: AccountExecutionModeDockerKernel,
		Extra: map[string]any{"codex_usage_updated_at": now.Add(-2 * openAIProbeCacheTTL).UTC().Format(time.RFC3339)}}
	require.False(t, account.IsOpenAIResponsesWebSocketV2Enabled())
	require.True(t, isOpenAICodexSnapshotStale(account, now))
	account.ExecutionMode = AccountExecutionModeNormal
	require.False(t, isOpenAICodexSnapshotStale(account, now), "ordinary account refresh policy stays unchanged")
}
