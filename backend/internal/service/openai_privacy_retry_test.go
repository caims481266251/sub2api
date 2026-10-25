//go:build unit

package service

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/imroc/req/v3"
	"github.com/stretchr/testify/require"
)

type privacyEnsurePersistenceRepo struct {
	AccountRepository
	err     error
	updates map[string]any
}

func (r *privacyEnsurePersistenceRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	r.updates = maps.Clone(updates)
	return r.err
}

func TestAdminService_EnsureOpenAIPrivacy_SynchronizesOnlyPersistedResult(t *testing.T) {
	for _, tc := range []struct {
		name       string
		emptyExtra bool
		persistErr bool
	}{
		{name: "stale failure replaced"},
		{name: "missing extra initialized", emptyExtra: true},
		{name: "failed persistence retains old state", persistErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPatch || r.Header.Get("Authorization") != "Bearer new-access" {
					t.Error("unexpected privacy request")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{}`))
			}))
			t.Cleanup(server.Close)
			target, err := url.Parse(server.URL)
			require.NoError(t, err)
			repo := &privacyEnsurePersistenceRepo{}
			if tc.persistErr {
				repo.err = errors.New("private database failure details")
			}
			svc := &adminServiceImpl{accountRepo: repo, privacyClientFactory: func(string) (*req.Client, error) {
				return req.C().WrapRoundTripFunc(func(rt req.RoundTripper) req.RoundTripFunc {
					return func(r *req.Request) (*req.Response, error) {
						r.URL.Scheme, r.URL.Host = target.Scheme, target.Host
						return rt.RoundTrip(r)
					}
				}), nil
			}}
			account := &Account{ID: 101, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
				Credentials: map[string]any{"access_token": "new-access"},
				Extra:       map[string]any{"privacy_mode": PrivacyModeFailed, "custom_setting": "keep"}}
			if tc.emptyExtra {
				account.Extra = nil
			}
			mode := svc.EnsureOpenAIPrivacy(context.Background(), account)
			require.Equal(t, map[string]any{"privacy_mode": PrivacyModeTrainingOff}, repo.updates)
			if tc.persistErr {
				require.Empty(t, mode, "unpersisted success must not be reported")
				require.Equal(t, PrivacyModeFailed, account.Extra["privacy_mode"])
			} else {
				require.Equal(t, PrivacyModeTrainingOff, mode)
				require.Equal(t, PrivacyModeTrainingOff, account.Extra["privacy_mode"])
			}
			if !tc.emptyExtra {
				require.Equal(t, "keep", account.Extra["custom_setting"])
			}
		})
	}
}

func TestAdminService_EnsureOpenAIPrivacy_RetriesNonSuccessModes(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{PrivacyModeFailed, PrivacyModeCFBlocked} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			privacyCalls := 0
			svc := &adminServiceImpl{
				accountRepo: &mockAccountRepoForGemini{},
				privacyClientFactory: func(proxyURL string) (*req.Client, error) {
					privacyCalls++
					return nil, errors.New("factory failed")
				},
			}

			account := &Account{
				ID:       101,
				Platform: PlatformOpenAI,
				Type:     AccountTypeOAuth,
				Credentials: map[string]any{
					"access_token": "token-1",
				},
				Extra: map[string]any{
					"privacy_mode": mode,
				},
			}

			got := svc.EnsureOpenAIPrivacy(context.Background(), account)

			require.Equal(t, PrivacyModeFailed, got)
			require.Equal(t, 1, privacyCalls)
		})
	}
}

func TestTokenRefreshService_ensureOpenAIPrivacy_RetriesNonSuccessModes(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		TokenRefresh: config.TokenRefreshConfig{
			MaxRetries:          1,
			RetryBackoffSeconds: 0,
		},
	}

	for _, mode := range []string{PrivacyModeFailed, PrivacyModeCFBlocked} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			service := NewTokenRefreshService(&tokenRefreshAccountRepo{}, nil, nil, nil, nil, nil, nil, cfg, nil)
			privacyCalls := 0
			service.SetPrivacyDeps(func(proxyURL string) (*req.Client, error) {
				privacyCalls++
				return nil, errors.New("factory failed")
			}, nil)

			account := &Account{
				ID:       202,
				Platform: PlatformOpenAI,
				Type:     AccountTypeOAuth,
				Credentials: map[string]any{
					"access_token": "token-2",
				},
				Extra: map[string]any{
					"privacy_mode": mode,
				},
			}

			service.ensureOpenAIPrivacy(context.Background(), account)

			require.Equal(t, 1, privacyCalls)
		})
	}
}
