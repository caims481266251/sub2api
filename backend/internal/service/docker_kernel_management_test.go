package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	httppool "github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	openaipkg "github.com/Wei-Shaw/sub2api/internal/pkg/openai"
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

func TestDockerKernelCredentialGuardsUseBoundKernel(t *testing.T) {
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
	// No model-list prober or host HTTP client is configured. Success therefore
	// requires actual kernel execution, never a cached model catalog.
	guard := &AccountTokenGuardV2Service{}
	state, _ := guard.probeAccount(context.Background(), account)
	require.Equal(t, AccountTokenGuardV2ProbeOK, state)
	legacy := (&AccountTokenGuardService{}).probe(context.Background(), AccountTokenGuardConfig{ProbeModel: "gpt-6.1-sol", ProbeTimeoutSeconds: 10}, account)
	require.Equal(t, AccountTokenGuardProbeOK, legacy.State)
	require.Equal(t, int32(2), calls.Load())
	require.False(t, account.Schedulable)
}

func TestDockerKernelUsageProbeUsesOAuthHTTPThroughAccountProxy(t *testing.T) {
	previous, loaded := vmKernelServices.LoadAndDelete(PlatformOpenAI)
	t.Cleanup(func() {
		if loaded {
			vmKernelServices.Store(PlatformOpenAI, previous)
		}
	})
	for _, withHeaders := range []bool{true, false} {
		t.Run(fmt.Sprintf("quota_headers_%v", withHeaders), func(t *testing.T) {
			var upstreamCalls, proxyCalls atomic.Int32
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls.Add(1)
				require.Equal(t, http.MethodPost, r.Method)
				require.Equal(t, "chatgpt.com", r.Host)
				require.Equal(t, "/backend-api/codex/responses", r.URL.Path)
				require.Equal(t, "Bearer fixture-access", r.Header.Get("Authorization"))
				require.Equal(t, "fixture-account", r.Header.Get("ChatGPT-Account-ID"))
				body, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				require.Equal(t, openaipkg.CodexUsageProbeModel, gjson.GetBytes(body, "model").String())
				w.Header().Set("Content-Type", "text/event-stream")
				if withHeaders {
					w.Header().Set("x-codex-primary-used-percent", "25")
					w.Header().Set("x-codex-primary-window-minutes", "300")
					w.Header().Set("x-codex-secondary-used-percent", "40")
					w.Header().Set("x-codex-secondary-window-minutes", "10080")
				}
				fmt.Fprint(w, "data: [DONE]\n\n")
			}))
			t.Cleanup(upstream.Close)
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				proxyCalls.Add(1)
				require.Equal(t, http.MethodConnect, r.Method)
				require.Equal(t, "chatgpt.com:443", r.Host)
				remote, err := net.DialTimeout("tcp", upstream.Listener.Addr().String(), time.Second)
				require.NoError(t, err)
				defer remote.Close()
				conn, buffered, err := w.(http.Hijacker).Hijack()
				require.NoError(t, err)
				defer conn.Close()
				_, err = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
				require.NoError(t, err)
				require.NoError(t, buffered.Flush())
				done := make(chan struct{})
				go func() {
					_, _ = io.Copy(remote, buffered)
					_ = remote.Close()
					close(done)
				}()
				_, _ = io.Copy(conn, remote)
				_ = conn.Close()
				<-done
			}))
			t.Cleanup(proxy.Close)
			proxyURL, err := url.Parse(proxy.URL)
			require.NoError(t, err)
			port, err := strconv.Atoi(proxyURL.Port())
			require.NoError(t, err)
			account := quotaRecoveryAccount()
			account.ExecutionMode = AccountExecutionModeDockerKernel
			account.Credentials = map[string]any{"access_token": "fixture-access", "chatgpt_account_id": "fixture-account"}
			account.Proxy = &Proxy{ID: 1, Protocol: "http", Host: proxyURL.Hostname(), Port: port}
			account.ProxyID = &account.Proxy.ID
			// This unique proxy owns its pooled client. Trust only the local TLS
			// fixture while keeping the real CONNECT and OAuth request paths.
			client, err := httppool.GetClient(httppool.Options{ProxyURL: proxy.URL, Timeout: 15 * time.Second, ResponseHeaderTimeout: 10 * time.Second})
			require.NoError(t, err)
			transport := upstream.Client().Transport.(*http.Transport).Clone()
			transport.Proxy = http.ProxyURL(proxyURL)
			transport.TLSClientConfig.ServerName = upstream.Certificate().DNSNames[0]
			client.Transport = transport
			t.Cleanup(func() {
				transport.CloseIdleConnections()
				httppool.EvictProxyClients(proxy.URL)
			})
			repo := &quotaRecoveryTestRepo{account: account}
			updates, err := (&AccountUsageService{accountRepo: repo}).probeOpenAICodexSnapshot(context.Background(), &account)
			require.NoError(t, err)
			require.Equal(t, int32(1), upstreamCalls.Load())
			require.Equal(t, int32(1), proxyCalls.Load())
			if withHeaders {
				require.Equal(t, float64(25), updates["codex_5h_used_percent"])
				require.Equal(t, float64(40), updates["codex_7d_used_percent"])
				require.Eventually(t, func() bool {
					repo.mu.Lock()
					defer repo.mu.Unlock()
					return repo.writes == 1
				}, time.Second, time.Millisecond)
			} else {
				require.Empty(t, updates, "missing headers must not become a zero-usage snapshot")
			}
			repo.mu.Lock()
			defer repo.mu.Unlock()
			require.NotNil(t, repo.account.RateLimitResetAt)
			require.Zero(t, repo.clears)
			if withHeaders {
				require.Equal(t, updates, repo.account.Extra)
			} else {
				require.Zero(t, repo.writes)
			}
		})
	}
}

func TestDockerKernelCredentialGuardsNeverFallBackWithoutRuntime(t *testing.T) {
	previous, loaded := vmKernelServices.LoadAndDelete(PlatformOpenAI)
	t.Cleanup(func() {
		if loaded {
			vmKernelServices.Store(PlatformOpenAI, previous)
		}
	})
	account := &Account{ID: 123, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ExecutionMode: AccountExecutionModeDockerKernel}
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
