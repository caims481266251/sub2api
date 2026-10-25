package service

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/imroc/req/v3"
	"github.com/stretchr/testify/require"
)

func TestOpenAIOAuthReauthDirectPrivacyResult(t *testing.T) {
	for _, container := range []bool{false, true} {
		mode := "ordinary"
		if container {
			mode = "container"
		}
		for _, tc := range []struct {
			name, oldMode, workerMode, wantMode string
			status                              int
			wantCalls                           int
			blocked, noFactory, proxyFails      bool
			proxyRemoved                        bool
		}{
			{name: "retry old failure", oldMode: PrivacyModeFailed, status: http.StatusOK, wantMode: PrivacyModeTrainingOff, wantCalls: 1},
			{name: "retry old challenge", oldMode: PrivacyModeCFBlocked, status: http.StatusOK, wantMode: PrivacyModeTrainingOff, wantCalls: 1},
			{name: "missing setting", status: http.StatusOK, wantMode: PrivacyModeTrainingOff, wantCalls: 1},
			{name: "unknown setting", oldMode: "unknown", status: http.StatusOK, wantMode: PrivacyModeTrainingOff, wantCalls: 1},
			{name: "setting still fails without failing login", oldMode: PrivacyModeFailed, status: http.StatusUnauthorized, wantMode: PrivacyModeFailed, wantCalls: 1},
			{name: "setting blocked without failing login", oldMode: PrivacyModeFailed, status: http.StatusForbidden, blocked: true, wantMode: PrivacyModeCFBlocked, wantCalls: 1},
			{name: "keep confirmed setting", oldMode: PrivacyModeTrainingOff},
			{name: "use fresh worker success", oldMode: PrivacyModeFailed, workerMode: PrivacyModeTrainingOff, wantMode: PrivacyModeTrainingOff},
			{name: "use fresh worker failure", oldMode: PrivacyModeTrainingOff, workerMode: PrivacyModeFailed, wantMode: PrivacyModeFailed},
			{name: "use fresh worker challenge", oldMode: PrivacyModeFailed, workerMode: PrivacyModeCFBlocked, wantMode: PrivacyModeCFBlocked},
			{name: "no client never claims success", oldMode: PrivacyModeFailed, noFactory: true},
			{name: "unavailable proxy never falls back", oldMode: PrivacyModeFailed, proxyFails: true},
			{name: "removed container proxy never falls back", oldMode: PrivacyModeFailed, proxyRemoved: true},
		} {
			if tc.proxyRemoved && !container {
				continue
			}
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				svc, reader, _, updater, _, blocker := newReauthTestService("acct-1")
				reader.account.Extra = map[string]any{"preserved": true}
				if tc.oldMode != "" {
					reader.account.Extra["privacy_mode"] = tc.oldMode
				}
				proxyID := int64(7)
				reader.account.ProxyID = &proxyID
				reader.proxy = &Proxy{ID: proxyID, Protocol: "socks5h", Host: "192.0.2.7", Port: 1080, Status: StatusActive}
				if container {
					reader.account.ExecutionMode = AccountExecutionModeDockerKernel
				}
				var responseMu sync.Mutex
				var calls int
				var receivedMethod, receivedToken, receivedFeature, receivedValue string
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					responseMu.Lock()
					defer responseMu.Unlock()
					calls++
					receivedMethod, receivedToken = r.Method, r.Header.Get("Authorization")
					receivedFeature, receivedValue = r.URL.Query().Get("feature"), r.URL.Query().Get("value")
					if tc.blocked {
						w.Header().Set("cf-mitigated", "challenge")
					}
					status := tc.status
					if status == 0 {
						status = http.StatusOK
					}
					w.WriteHeader(status)
				}))
				t.Cleanup(server.Close)
				var factoryCalls int
				var usedProxy string
				if !tc.noFactory {
					factory := newQuotaRedirectingFactory(server)
					svc.oauth.SetPrivacyClientFactory(func(proxyURL string) (*req.Client, error) {
						factoryCalls++
						usedProxy = proxyURL
						return factory(proxyURL)
					})
				}
				savePasswordReauthConfig(t, svc)
				_, err := svc.CreateTask(t.Context(), 42)
				require.NoError(t, err)
				claim, err := svc.ClaimTask(t.Context(), "worker")
				require.NoError(t, err)
				require.NotNil(t, claim)
				if tc.proxyFails {
					reader.proxyErr = errors.New("proxy unavailable after login")
				}
				if tc.proxyRemoved {
					reader.account.ProxyID = nil
				}
				var workerExtra map[string]any
				if tc.workerMode != "" {
					workerExtra = map[string]any{"privacy_mode": tc.workerMode}
				}
				result, err := svc.SubmitCredentials(t.Context(), claim.TaskID, "worker", directReauthCredentials("acct-1", "user-1", "user@example.com"), workerExtra)
				require.NoError(t, err)
				require.Equal(t, OpenAIOAuthReauthStatusSucceeded, result.Status)
				require.True(t, updater.applied)
				require.Equal(t, "new-access", updater.credentials["access_token"])
				responseMu.Lock()
				defer responseMu.Unlock()
				require.Equal(t, tc.wantCalls, calls)
				require.Equal(t, tc.wantCalls, factoryCalls)
				if tc.wantMode == "" {
					require.NotContains(t, updater.extra, "privacy_mode", "unchecked state must leave the persisted result intact")
				} else {
					require.Equal(t, tc.wantMode, updater.extra["privacy_mode"])
				}
				if tc.wantCalls > 0 {
					require.Equal(t, reader.proxy.URL(), usedProxy)
					require.Equal(t, http.MethodPatch, receivedMethod)
					require.Equal(t, "Bearer new-access", receivedToken)
					require.Equal(t, "training_allowed", receivedFeature)
					require.Equal(t, "false", receivedValue)
				}
				require.Equal(t, tc.oldMode, reauthMapString(reader.account.Extra, "privacy_mode"), "do not mutate the pre-CAS snapshot")
				require.Equal(t, true, reader.account.Extra["preserved"])
				if container {
					require.Zero(t, blocker.clearedAccountID)
				}
			})
		}
	}
}

func TestOpenAIOAuthReauthPrivacyWaitsForIdentityValidation(t *testing.T) {
	svc, reader, _, updater, _, _ := newReauthTestService("acct-1")
	reader.account.Extra = map[string]any{"privacy_mode": PrivacyModeFailed}
	calls := 0
	svc.oauth.SetPrivacyClientFactory(func(string) (*req.Client, error) {
		calls++
		return nil, errors.New("unexpected privacy request")
	})
	savePasswordReauthConfig(t, svc)
	_, err := svc.CreateTask(t.Context(), 42)
	require.NoError(t, err)
	claim, err := svc.ClaimTask(t.Context(), "worker")
	require.NoError(t, err)
	_, err = svc.SubmitCredentials(t.Context(), claim.TaskID, "worker", directReauthCredentials("different-account", "user-1", "user@example.com"), nil)
	require.Error(t, err)
	require.Zero(t, calls)
	require.False(t, updater.applied)
}

func TestOpenAIOAuthReauthCallbackPersistsPrivacyResultOnce(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusUnauthorized} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			svc, reader, _, updater, _, _ := newReauthTestService("acct-1")
			reader.account.Extra = map[string]any{"privacy_mode": PrivacyModeFailed}
			var privacyCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPatch && r.URL.Path == "/backend-api/settings/account_user_setting" {
					privacyCalls.Add(1)
					w.WriteHeader(status)
					return
				}
				w.WriteHeader(http.StatusNotFound)
			}))
			t.Cleanup(server.Close)
			svc.oauth.SetPrivacyClientFactory(newQuotaRedirectingFactory(server))
			_, err := svc.SaveConfig(t.Context(), 42, "user@example.com", "https://mail.example.com/code")
			require.NoError(t, err)
			_, err = svc.CreateTask(t.Context(), 42)
			require.NoError(t, err)
			claim, err := svc.ClaimTask(t.Context(), "worker")
			require.NoError(t, err)
			result, err := svc.SubmitCallback(t.Context(), claim.TaskID, "worker", "http://localhost:1455/auth/callback?code=test&state="+url.QueryEscape(reauthStateFromURL(t, claim.AuthURL)))
			require.NoError(t, err)
			require.Equal(t, OpenAIOAuthReauthStatusSucceeded, result.Status)
			require.EqualValues(t, 1, privacyCalls.Load())
			wantMode := PrivacyModeFailed
			if status == http.StatusOK {
				wantMode = PrivacyModeTrainingOff
			}
			require.Equal(t, wantMode, updater.extra["privacy_mode"])
		})
	}
}
