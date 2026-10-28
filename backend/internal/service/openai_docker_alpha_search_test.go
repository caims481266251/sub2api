package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/vmkernel"
	"github.com/Wei-Shaw/sub2api/internal/vmruntime"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type dockerSearchUpstream struct {
	HTTPUpstream
	send func(*http.Request, string, int64, int) (*http.Response, error)
}

func (u dockerSearchUpstream) Do(r *http.Request, proxy string, id int64, concurrency int) (*http.Response, error) {
	return u.send(r, proxy, id, concurrency)
}

func dockerSearchAccount(t *testing.T) *Account {
	t.Helper()
	id := int64(441177)
	a := &Account{ID: id, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		ExecutionMode: AccountExecutionModeDockerKernel, Status: StatusActive, Schedulable: true, Concurrency: 15,
		Credentials: map[string]any{"access_token": "account-token", "chatgpt_account_id": "upstream-account"},
		ProxyID:     &id, Proxy: &Proxy{ID: id, Protocol: "socks5h", Host: "proxy.invalid", Port: 1080, Status: StatusActive},
	}
	dockerSearchPublishReady(a)
	t.Cleanup(func() { vmKernelReadiness.Delete(a.ID) })
	return a
}

func dockerSearchPublishReady(a *Account) {
	vmKernelReadiness.Store(a.ID, vmRuntimeStatus{state: vmruntime.State{Ready: true},
		credentials: vmCredentialRevision(a), proxy: vmProxyRevision(a), checked: time.Now()})
}

func dockerSearchContext(ctx context.Context, body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/alpha/search?feature=standalone", bytes.NewReader(body)).WithContext(ctx)
	return c, rec
}

func TestDockerAlphaSearchSchedulerEligibility(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	a := dockerSearchAccount(t)
	group := int64(441178)
	s := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{*a}},
		cache: &schedulerTestGatewayCache{}, cfg: &config.Config{}, concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{})}
	selection, _, err := s.SelectAccountWithSchedulerForCapability(t.Context(), &group, "", "", "gpt-5.6-sol", nil,
		OpenAIUpstreamTransportHTTPSSE, OpenAIEndpointCapabilityAlphaSearch, false, false, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.Equal(t, a.ID, selection.Account.ID)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
	vmKernelReadiness.Delete(a.ID)
	selection, _, err = s.SelectAccountWithSchedulerForCapability(t.Context(), &group, "", "", "gpt-5.6-sol", nil,
		OpenAIUpstreamTransportHTTPSSE, OpenAIEndpointCapabilityAlphaSearch, false, false, false)
	require.Error(t, err)
	require.Nil(t, selection)
	for _, mutate := range []func(*Account){
		func(a *Account) { a.Platform = PlatformAnthropic },
		func(a *Account) { a.Type = AccountTypeAPIKey },
		func(a *Account) { a.ExecutionMode = "vm_kernel" },
		func(a *Account) { a.Credentials["auth_mode"] = OpenAIAuthModePersonalAccessToken },
	} {
		copy := *a
		copy.Credentials = maps.Clone(a.Credentials)
		mutate(&copy)
		require.False(t, copy.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityAlphaSearch))
	}
}

func TestDockerAlphaSearchPreservesSearchWireAndBoundIdentity(t *testing.T) {
	a := dockerSearchAccount(t)
	a.Credentials["model_mapping"] = map[string]any{"client-model": "gpt-5.6-sol"}
	a.Credentials["user_agent"] = "caller-override/9"
	metadata := `{"session_id":"original-session","turn_id":"original-turn","future":{"keep":true}}`
	body := []byte(`{"id":"original-session","model":"client-model","commands":{"search_query":[{"q":"news"}]},"future":{"keep":true},"store":false,"prompt_cache_key":"drop","prompt_cache_retention":"24h"}`)
	response := "{\n  \"encrypted_output\": \"opaque==\", \"output\": \"found\", \"results\": [], \"future\": 123\n}"
	c, rec := dockerSearchContext(t.Context(), body)
	for k, v := range map[string]string{"Authorization": "Bearer client-secret", "User-Agent": "curl/9", "Version": "9.0.0",
		"Originator": "foreign-client", "ChatGPT-Account-ID": "foreign-account", "Session_ID": "foreign-session",
		"Conversation_ID": "foreign-conversation", "X-Codex-Turn-State": "foreign-state", "OpenAI-Beta": "responses=experimental",
		"X-Codex-Turn-Metadata": metadata, responsesLiteHeaderKey: "true"} {
		c.Request.Header.Set(k, v)
	}
	calls := 0
	s := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: dockerSearchUpstream{send: func(r *http.Request, proxy string, id int64, concurrency int) (*http.Response, error) {
		calls++
		require.Equal(t, chatgptCodexAlphaSearchURL+"?feature=standalone", r.URL.String())
		require.Equal(t, a.Proxy.URL(), proxy)
		require.Equal(t, a.ID, id)
		require.Equal(t, 15, concurrency)
		require.Equal(t, "Bearer account-token", r.Header.Get("Authorization"))
		require.Equal(t, "upstream-account", r.Header.Get("ChatGPT-Account-ID"))
		require.Equal(t, vmkernel.DefaultCodexUserAgent, r.Header.Get("User-Agent"))
		require.Equal(t, "0.160.0", r.Header.Get("Version"))
		require.Equal(t, "codex_cli_rs", r.Header.Get("Originator"))
		require.Equal(t, metadata, r.Header.Get("X-Codex-Turn-Metadata"))
		require.Equal(t, "application/json", r.Header.Get("Accept"))
		require.True(t, HTTPUpstreamRedirectsDisabled(r.Context()))
		for _, key := range []string{"Session_ID", "Conversation_ID", "X-Codex-Turn-State", "OpenAI-Beta", responsesLiteHeaderKey} {
			require.Empty(t, r.Header.Get(key), key)
		}
		got, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.JSONEq(t, `{"id":"original-session","model":"gpt-5.6-sol","commands":{"search_query":[{"q":"news"}]},"future":{"keep":true}}`, string(got))
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}, "X-Request-Id": {"search-request"}}, Body: io.NopCloser(strings.NewReader(response))}, nil
	}}}
	result, err := s.ForwardAlphaSearch(t.Context(), c, a, body)
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	require.Equal(t, response, rec.Body.String(), "encrypted search output must be byte-preserved")
	require.Equal(t, "client-model", result.Model)
	require.Equal(t, "gpt-5.6-sol", result.UpstreamModel)
	require.Equal(t, "search-request", result.RequestID)
	require.Equal(t, 1, result.WebSearchCalls)
	require.True(t, VMKernelRuntimeReady(a))
}

func TestDockerAlphaSearchErrorsDoNotChargeOrDamageRuntime(t *testing.T) {
	for _, status := range []int{200, 302, 400, 401, 403, 404, 429, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			a := dockerSearchAccount(t)
			body := []byte(`{"model":"gpt-5.6-sol","commands":{}}`)
			c, rec := dockerSearchContext(t.Context(), body)
			payload := `{"error":{"message":"provider rejection"}}`
			if status == 200 {
				payload = "<html>proxy error</html>"
			}
			calls := 0
			s := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: dockerSearchUpstream{send: func(*http.Request, string, int64, int) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(payload))}, nil
			}}}
			result, err := s.ForwardAlphaSearch(t.Context(), c, a, body)
			require.Nil(t, result)
			require.Equal(t, 1, calls)
			if status == 302 || status == 400 || status == 404 {
				require.NoError(t, err)
				require.Equal(t, status, rec.Code)
				require.Equal(t, payload, rec.Body.String())
			} else {
				require.Error(t, err)
				require.False(t, c.Writer.Written())
				if status != 200 {
					var failover *UpstreamFailoverError
					require.ErrorAs(t, err, &failover)
					require.Equal(t, status, failover.StatusCode)
				}
			}
			require.True(t, VMKernelRuntimeReady(a), "provider errors must not change container health")
			require.Equal(t, StatusActive, a.Status)
		})
	}
}

func TestDockerAlphaSearchPinsProxyAndSkipsPlugin(t *testing.T) {
	for _, fallback := range []string{FallbackModeDirect, FallbackModeProxy} {
		t.Run(fallback, func(t *testing.T) {
			a := dockerSearchAccount(t)
			a.Proxy.FallbackMode = fallback
			backup := int64(2)
			a.Proxy.BackupProxyID = &backup
			plugin := &PluginManager{}
			plugin.route.Store(&pluginRoute{rolloutPercent: 100, unavailable: "must not be called"})
			calls := 0
			s := &OpenAIGatewayService{pluginManager: plugin, httpUpstream: dockerSearchUpstream{send: func(r *http.Request, proxy string, _ int64, _ int) (*http.Response, error) {
				calls++
				require.Equal(t, a.Proxy.URL(), proxy)
				if trace := httptrace.ContextClientTrace(r.Context()); trace != nil && trace.GetConn != nil {
					trace.GetConn("chatgpt.com:443")
				}
				return nil, errors.New("connection refused")
			}}}
			req, err := s.buildOpenAIAlphaSearchRequest(t.Context(), nil, a, []byte(`{}`), "account-token")
			require.NoError(t, err)
			_, err = s.doOpenAIUpstream(req, a.Proxy.URL(), a)
			require.ErrorContains(t, err, "connection refused")
			require.Equal(t, 1, calls)
			for _, proxy := range []string{"", "http://other.invalid:8080"} {
				_, err = s.doOpenAIUpstream(req, proxy, a)
				require.True(t, IsOpenAITurnAdmissionError(err))
			}
			req.URL.Path = "/backend-api/codex/responses"
			_, err = s.doOpenAIUpstream(req, a.Proxy.URL(), a)
			require.True(t, IsOpenAITurnAdmissionError(err), "inference must remain in the resident kernel")
			require.Equal(t, 1, calls)
		})
	}
}

func TestDockerAlphaSearchAdmissionAndCancellation(t *testing.T) {
	for _, reason := range []string{"not_ready", "disabled", "rate_limited", "proxy_missing", "proxy_changed", "cancelled"} {
		t.Run(reason, func(t *testing.T) {
			a := dockerSearchAccount(t)
			selected := *a
			s := &OpenAIGatewayService{cfg: &config.Config{}, accountRepo: &turnAdmissionRepo{account: a}, httpUpstream: dockerSearchUpstream{send: func(*http.Request, string, int64, int) (*http.Response, error) {
				t.Fatal("ineligible request reached upstream")
				return nil, nil
			}}}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch reason {
			case "not_ready":
				vmKernelReadiness.Delete(a.ID)
			case "disabled":
				a.Schedulable = false
			case "rate_limited":
				until := time.Now().Add(time.Hour)
				a.RateLimitResetAt = &until
			case "proxy_missing":
				a.Proxy = nil
			case "proxy_changed":
				proxy := *a.Proxy
				proxy.Host = "changed.invalid"
				a.Proxy = &proxy
				dockerSearchPublishReady(a)
			case "cancelled":
				cancel()
			}
			body := []byte(`{"model":"gpt-5.6-sol","commands":{}}`)
			c, _ := dockerSearchContext(ctx, body)
			result, err := s.ForwardAlphaSearch(ctx, c, &selected, body)
			require.Error(t, err)
			require.Nil(t, result)
			require.False(t, c.Writer.Written())
		})
	}
	// Cancellation must reach a dispatched host request, not just the scheduler.
	a := dockerSearchAccount(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	body := []byte(`{"model":"gpt-5.6-sol","commands":{}}`)
	c, _ := dockerSearchContext(ctx, body)
	s := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: dockerSearchUpstream{send: func(r *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
		cancel()
		<-r.Context().Done()
		return nil, r.Context().Err()
	}}}
	result, err := s.ForwardAlphaSearch(ctx, c, a, body)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, result)
	require.False(t, c.Writer.Written())
	require.True(t, VMKernelRuntimeReady(a))
}
