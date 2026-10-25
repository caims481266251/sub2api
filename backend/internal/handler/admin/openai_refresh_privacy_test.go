//go:build unit

package admin

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/imroc/req/v3"
	"github.com/stretchr/testify/require"
)

type privacyRefreshOAuthClient struct {
	service.OpenAIOAuthClient
}

func (*privacyRefreshOAuthClient) RefreshTokenWithClientID(context.Context, string, string, string) (*openai.TokenResponse, error) {
	return &openai.TokenResponse{AccessToken: "new-access", RefreshToken: "new-refresh", ExpiresIn: 3600}, nil
}

type privacyRefreshAdminService struct {
	*stubAdminService
	extraErr     error
	extraUpdates map[string]any
	ensureCalls  int
}

func (s *privacyRefreshAdminService) UpdateAccount(_ context.Context, _ int64, input *service.UpdateAccountInput) (*service.Account, error) {
	s.lastUpdateAccountInput = input
	updated := *s.getAccountResult
	updated.Credentials = maps.Clone(input.Credentials)
	updated.Extra = maps.Clone(updated.Extra)
	return &updated, nil
}

func (s *privacyRefreshAdminService) UpdateAccountExtra(_ context.Context, _ int64, updates map[string]any) error {
	s.updateAccountExtraCalls++
	s.extraUpdates = maps.Clone(updates)
	return s.extraErr
}

func (s *privacyRefreshAdminService) EnsureOpenAIPrivacy(context.Context, *service.Account) string {
	s.ensureCalls++
	return ""
}

func TestOpenAIRefreshPersistsFreshPrivacyResult(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, entry := range []string{"openai", "generic"} {
		for _, tc := range []struct {
			name       string
			status     int
			cfBlocked  bool
			persistErr bool
			mode       string
		}{
			{name: "success replaces stale failure", status: http.StatusOK, mode: service.PrivacyModeTrainingOff},
			{name: "fresh failure remains failure", status: http.StatusBadGateway, mode: service.PrivacyModeFailed},
			{name: "fresh Cloudflare block is retained", status: http.StatusForbidden, cfBlocked: true, mode: service.PrivacyModeCFBlocked},
			{name: "persistence failure keeps stored state", status: http.StatusOK, persistErr: true, mode: service.PrivacyModeTrainingOff},
		} {
			t.Run(entry+"/"+tc.name, func(t *testing.T) {
				var privacyCalls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if r.Method == http.MethodPatch {
						privacyCalls.Add(1)
						if r.Header.Get("Authorization") != "Bearer new-access" {
							t.Error("privacy request did not use the refreshed token")
						}
						if tc.cfBlocked {
							w.Header().Set("cf-mitigated", "challenge")
						}
						w.WriteHeader(tc.status)
					}
					_, _ = w.Write([]byte(`{}`))
				}))
				t.Cleanup(server.Close)
				target, err := url.Parse(server.URL)
				require.NoError(t, err)
				oauth := service.NewOpenAIOAuthService(nil, &privacyRefreshOAuthClient{})
				oauth.SetPrivacyClientFactory(func(string) (*req.Client, error) {
					return req.C().WrapRoundTripFunc(func(rt req.RoundTripper) req.RoundTripFunc {
						return func(r *req.Request) (*req.Response, error) {
							r.URL.Scheme, r.URL.Host = target.Scheme, target.Host
							return rt.RoundTrip(r)
						}
					}), nil
				})
				account := &service.Account{
					ID: 42, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
					Credentials: map[string]any{"access_token": "old-access", "refresh_token": "old-refresh", "model_mapping": "keep"},
					Extra:       map[string]any{"privacy_mode": service.PrivacyModeFailed, "custom_setting": "keep"},
				}
				admin := &privacyRefreshAdminService{stubAdminService: &stubAdminService{getAccountResult: account}}
				if tc.persistErr {
					admin.extraErr = errors.New("private database failure details")
				}
				var responseExtra map[string]any
				if entry == "generic" {
					handler := &AccountHandler{adminService: admin, openaiOAuthService: oauth}
					updated, warning, err := handler.refreshSingleAccount(context.Background(), account)
					require.NoError(t, err)
					require.Empty(t, warning)
					responseExtra = updated.Extra
				} else {
					handler := NewOpenAIOAuthHandler(oauth, admin, nil, nil)
					router := gin.New()
					router.POST("/admin/openai/accounts/:id/refresh", handler.RefreshAccountToken)
					recorder := httptest.NewRecorder()
					router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/admin/openai/accounts/42/refresh", nil))
					require.Equal(t, http.StatusOK, recorder.Code)
					var response struct {
						Data struct {
							Platform string         `json:"platform"`
							Extra    map[string]any `json:"extra"`
						} `json:"data"`
					}
					require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
					require.Equal(t, service.PlatformOpenAI, response.Data.Platform)
					responseExtra = response.Data.Extra
				}
				wantMode := tc.mode
				if tc.persistErr {
					wantMode = service.PrivacyModeFailed
				}
				require.Equal(t, wantMode, responseExtra["privacy_mode"])
				require.Equal(t, "keep", responseExtra["custom_setting"])
				require.Equal(t, map[string]any{"privacy_mode": tc.mode}, admin.extraUpdates)
				require.Equal(t, 1, admin.updateAccountExtraCalls)
				require.Zero(t, admin.ensureCalls, "fresh failures also must not be retried immediately")
				require.EqualValues(t, 1, privacyCalls.Load())
				require.Nil(t, admin.lastUpdateAccountInput.Extra, "credential updates must not replace unrelated extra")
				require.Equal(t, "new-access", admin.lastUpdateAccountInput.Credentials["access_token"])
				require.Equal(t, "keep", admin.lastUpdateAccountInput.Credentials["model_mapping"])
			})
		}
	}
}

func TestRefreshSingleAccountEnsuresPrivacyWhenRefreshHasNoResult(t *testing.T) {
	account := &service.Account{ID: 42, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "old-access", "refresh_token": "old-refresh"}}
	admin := &privacyRefreshAdminService{stubAdminService: &stubAdminService{getAccountResult: account}}
	handler := &AccountHandler{adminService: admin, openaiOAuthService: service.NewOpenAIOAuthService(nil, &privacyRefreshOAuthClient{})}
	_, _, err := handler.refreshSingleAccount(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, 1, admin.ensureCalls)
	require.Zero(t, admin.updateAccountExtraCalls)
}
