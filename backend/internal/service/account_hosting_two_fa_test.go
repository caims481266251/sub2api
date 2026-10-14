package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

type hostingRefreshClient struct {
	OpenAIOAuthClient
	err   error
	calls int
}

func (c *hostingRefreshClient) RefreshTokenWithClientID(_ context.Context, rt, _, _ string) (*openai.TokenResponse, error) {
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	if rt != "login-refresh" {
		return nil, errors.New("unexpected refresh token")
	}
	claims := `{"email":"verified@example.com","https://api.openai.com/auth":{"chatgpt_account_id":"verified-account","chatgpt_user_id":"verified-user","chatgpt_plan_type":"plus"}}`
	return &openai.TokenResponse{AccessToken: "verified-access", RefreshToken: "verified-refresh", IDToken: "e30." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".signature", ExpiresIn: 3600}, nil
}

func TestHostingTwoFAOwnershipVerificationAndSaveRetry(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"credential":{"refresh_token":"login-refresh","access_token":"unverified-access","id_token":"unverified-id","plan_type":"pro","password":"echoed-password","mfa_secret":"echoed-mfa"}}`)
	}))
	defer server.Close()
	guard := newTwoFATestService(t, server)
	s, repo, _ := hostingTestService()
	s.SetTwoFALoginService(guard)
	refresh := &hostingRefreshClient{}
	s.oauth = NewOpenAIOAuthService(nil, refresh)
	pool := int64(31)
	repo.pools = map[int64]int64{pool: 7}
	job, err := s.StartTwoFALogin(ctx, 7, 0, &pool, AccountTokenGuardReloginAccount{Email: "login@example.com", Password: "test-password", MFASecret: "test-secret"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.DeleteTwoFALogin(7, job.ID) })
	_, err = s.TwoFALogin(ctx, 8, job.ID)
	require.Error(t, err)
	_, err = s.CompleteTwoFALogin(ctx, 8, job.ID, nil)
	require.Error(t, err)
	require.Error(t, s.DeleteTwoFALogin(8, job.ID))
	_, err = s.StartTwoFALogin(ctx, 7, 0, nil, AccountTokenGuardReloginAccount{Email: "another@example.com", Password: "p", MFASecret: "m"})
	require.Error(t, err)
	require.Eventually(t, func() bool {
		status, err := s.TwoFALogin(ctx, 7, job.ID)
		return err == nil && status.Status == "succeeded"
	}, time.Second, 10*time.Millisecond)
	status, err := s.TwoFALogin(ctx, 7, job.ID)
	require.NoError(t, err)
	raw, err := json.Marshal(status)
	require.NoError(t, err)
	for _, secret := range []string{"refresh", "access", "credential", "password", "mfa"} {
		require.NotContains(t, string(raw), secret)
	}
	_, err = s.CompleteTwoFALogin(ctx, 7, job.ID, &HostingAccountSettings{Extra: map[string]any{"openai_prism_browser": true}})
	require.Error(t, err)
	require.Zero(t, refresh.calls)
	_, err = s.CompleteTwoFALogin(ctx, 7, job.ID, &HostingAccountSettings{HostingPoolID: func() *int64 { v := int64(0); return &v }()})
	require.Error(t, err)
	require.Zero(t, refresh.calls)
	refresh.err = errors.New("private-upstream-error")
	_, err = s.CompleteTwoFALogin(ctx, 7, job.ID, nil)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "private-upstream-error")
	_, err = s.TwoFALogin(ctx, 7, job.ID)
	require.NoError(t, err, "failed save keeps the verified login job for retry")
	refresh.err = nil
	var input HostingAccountSettings
	require.NoError(t, json.Unmarshal([]byte(`{"hosting_pool_id":31,"name":"Configured 2FA","concurrency":8,"extra":{"openai_compact_mode":"force_on"}}`), &input))
	a, err := s.CompleteTwoFALogin(ctx, 7, job.ID, &input)
	require.NoError(t, err)
	require.Equal(t, "Configured 2FA", a.Name)
	require.Equal(t, "verified-account", a.GetChatGPTAccountID())
	require.Equal(t, "verified-refresh", a.GetCredential("refresh_token"))
	require.Equal(t, "plus", a.GetCredential("plan_type"))
	require.NotContains(t, a.Credentials, "password")
	require.NotContains(t, a.Credentials, "mfa_secret")
	require.Same(t, &input, repo.savedSettings)
	require.Equal(t, pool, *a.HostingPoolID)
	_, err = s.CompleteTwoFALogin(ctx, 7, job.ID, &input)
	require.Error(t, err)
	_, err = s.TwoFALogin(ctx, 7, job.ID)
	require.Error(t, err)
	settings := guard.settings.(*twoFALoginSettings)
	settings.mu.Lock()
	defer settings.mu.Unlock()
	require.Zero(t, settings.writes, "hosting must not enroll user passwords in platform credential operations")
}

func TestHostingTwoFARejectsFullSeatsForeignAccountsAndPools(t *testing.T) {
	ctx := context.Background()
	s, repo, settings := hostingTestService()
	entry := AccountTokenGuardReloginAccount{Email: "user@example.com", Password: "password", MFASecret: "secret"}
	owner := int64(7)
	repo.accounts = []Account{{ID: 17, OwnerUserID: &owner}}
	_, err := s.StartTwoFALogin(ctx, 7, 0, nil, entry)
	require.ErrorIs(t, err, ErrHostingSeatsFull)
	_, err = s.StartTwoFALogin(ctx, 8, 17, nil, entry)
	require.ErrorIs(t, err, ErrHostingAccountNotFound)
	_, err = s.StartTwoFALogin(ctx, 7, 17, nil, entry)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrHostingSeatsFull, "reauthorization does not need another seat")
	repo.accounts = nil
	pool := int64(31)
	repo.pools = map[int64]int64{31: 8}
	_, err = s.StartTwoFALogin(ctx, 7, 0, &pool, entry)
	require.ErrorIs(t, err, ErrHostingPoolNotFound)
	settings.value = `{"enabled":false}`
	_, err = s.StartTwoFALogin(ctx, 7, 0, nil, entry)
	require.ErrorIs(t, err, ErrHostingDisabled)
}
