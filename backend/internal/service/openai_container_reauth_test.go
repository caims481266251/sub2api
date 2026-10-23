package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/vmkernel"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type containerReauthTestRepo struct {
	*reauthTestRepo
	calls  int
	hash   string
	err    error
	manual bool
}

func (r *containerReauthTestRepo) QueueContainerReauth(_ context.Context, _ *Account, hash string, manual bool) (bool, error) {
	r.calls++
	r.manual = manual
	r.hash = hash
	return true, r.err
}
func (r *containerReauthTestRepo) CompleteContainerTwoFAEnrollment(context.Context, int64) error {
	return r.err
}
func TestContainerReauthOnlyProvider401Triggers(t *testing.T) {
	for _, tc := range []struct {
		name, event string
		want        int
	}{
		{"explicit unauthorized", "{\"type\":\"error\",\"error\":{\"status\":401,\"message\":\"Unauthorized\"}}", 1},
		{"revoked", "{\"type\":\"response.failed\",\"response\":{\"error\":{\"status_code\":401,\"code\":\"token_revoked\",\"message\":\"Token revoked\"}}}", 1},
		{"rate limit", "{\"type\":\"error\",\"error\":{\"status\":429,\"code\":\"rate_limit_exceeded\",\"message\":\"Too many requests\"}}", 0},
		{"transport", "{\"type\":\"error\",\"error\":{\"status\":502,\"message\":\"transport error\"}}", 0},
		{"internal 401 never produces provider event", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, reader, repo, _, _, _ := newReauthTestService("acct-1")
			reader.account.ExecutionMode = AccountExecutionModeDockerKernel
			store := &containerReauthTestRepo{reauthTestRepo: repo}
			svc.repo = store
			gateway := &OpenAIGatewayService{excelOAuthReauth: svc}
			gateway.observeVMKernelOpenAIResult(t.Context(), nil, reader.account, "gpt-test", &vmkernel.Result{ErrorEvent: json.RawMessage(tc.event), DispatchedCredentialsHash: "actual-dispatch"})
			require.Equal(t, tc.want, store.calls)
			if tc.want > 0 {
				require.Equal(t, "actual-dispatch", store.hash)
			}
		})
	}
}
func TestContainerReauthOrdinaryModeUnchanged(t *testing.T) {
	svc, reader, repo, _, _, _ := newReauthTestService("acct-1")
	store := &containerReauthTestRepo{reauthTestRepo: repo}
	svc.repo = store
	require.False(t, svc.handleContainerUnauthorized(t.Context(), reader.account, "hash"))
	require.Zero(t, store.calls)
	reader.account.ExecutionMode = AccountExecutionModeDockerKernel
	store.err = errors.New("private database diagnostics")
	require.True(t, svc.handleContainerUnauthorized(t.Context(), reader.account, "hash"), "failed queue must not start a second auth policy")
}
func TestContainerTwoFAIdentityAndRequiredSecret(t *testing.T) {
	for _, tc := range []struct {
		name, email, secret string
		stripIdentity       bool
		valid               bool
	}{
		{"matching", "USER@example.com", "TESTSECRET", false, true},
		{"different email", "other@example.com", "TESTSECRET", false, false},
		{"missing secret", "user@example.com", "", false, false},
		{"missing stable identity", "user@example.com", "TESTSECRET", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, reader, repo, _, _, _ := newReauthTestService("acct-1")
			reader.account.ExecutionMode = AccountExecutionModeDockerKernel
			if tc.stripIdentity {
				delete(reader.account.Credentials, "chatgpt_account_id")
				delete(reader.account.Credentials, "chatgpt_user_id")
			}
			_, err := svc.SaveCredentialConfig(t.Context(), 42, OpenAIOAuthReauthConfigInput{LoginEmail: tc.email, CredentialMode: OpenAIOAuthReauthModePasswordTOTP, Password: "test-password", TOTPSecret: tc.secret})
			if tc.valid {
				require.NoError(t, err)
				require.Equal(t, "encrypted:test-password", repo.config.PasswordCiphertext)
			} else {
				require.Error(t, err)
				require.Nil(t, repo.config)
			}
		})
	}
}
func TestContainerReauthSuccessPreservesUnrelatedRuntimeBlocks(t *testing.T) {
	svc, reader, repo, updater, _, blocker := newReauthTestService("acct-1")
	reader.account.ExecutionMode = AccountExecutionModeDockerKernel
	savePasswordReauthConfig(t, svc)
	task, err := svc.CreateTask(t.Context(), 42)
	require.NoError(t, err)
	duplicate, err := svc.CreateTask(t.Context(), 42)
	require.NoError(t, err)
	require.Equal(t, task.ID, duplicate.ID)
	require.Equal(t, 1, repo.createCalls)
	claim, err := svc.ClaimTask(t.Context(), "worker")
	require.NoError(t, err)
	result, err := svc.SubmitCredentials(t.Context(), claim.TaskID, "worker", directReauthCredentials("acct-1", "user-1", "user@example.com"), nil)
	require.NoError(t, err)
	require.Equal(t, OpenAIOAuthReauthStatusSucceeded, result.Status)
	require.True(t, updater.applied)
	require.Zero(t, blocker.clearedAccountID, "reauth must not clear business runtime blocks")
}
func TestContainerInitialLoginValidatesReturnedIdentity(t *testing.T) {
	valid := directReauthCredentials("acct-1", "user-1", "user@example.com")
	out, err := validatedContainerLoginCredential(valid, "USER@example.com")
	require.NoError(t, err)
	require.Equal(t, "user@example.com", out["email"])
	_, err = validatedContainerLoginCredential(valid, "other@example.com")
	require.Error(t, err)
	_, err = validatedContainerLoginCredential(map[string]any{"access_token": "a", "refresh_token": "r", "id_token": "invalid"}, "user@example.com")
	require.Error(t, err)
}
func TestContainerEnrollmentRemainsClosedBeforeEncryptedSave(t *testing.T) {
	proxyID := int64(7)
	input := &CreateAccountInput{ExecutionMode: AccountExecutionModeDockerKernel, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ProxyID: &proxyID, Concurrency: 15, InitialOpenAITwoFA: true}
	a, err := buildAccountForCreate(input, map[string]any{})
	require.NoError(t, err)
	require.Equal(t, StatusError, a.Status)
	require.Equal(t, OpenAIContainerTwoFAEnrollmentPending, a.ErrorMessage)
	require.False(t, a.IsSchedulable())
	require.Equal(t, 15, a.Concurrency)
	a.Proxy = &Proxy{ID: 7, Status: StatusActive}
	require.True(t, vmAccountEligible(a), "enrollment must not mark a healthy container broken")
	a.ErrorMessage = OpenAIContainerAuthRequired
	require.True(t, vmAccountEligible(a))
	require.False(t, vmAccountRequestEligible(a, false))
	a.Status = StatusDisabled
	require.False(t, vmAccountEligible(a))
}

func TestContainerReauthProbeUsesAutomaticDeduplication(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			svc, reader, repo, _, _, _ := newReauthTestService("acct-1")
			reader.account.ExecutionMode = AccountExecutionModeDockerKernel
			store := &containerReauthTestRepo{reauthTestRepo: repo}
			svc.repo = store
			guard := &accountTokenGuardV2TestRepo{record: &AccountTokenGuardV2Record{AccountID: 42, Enabled: true, AutoReloginEnabled: true}}
			prober := accountTokenGuardV2TestProber{err: &codexModelsManifestUpstreamError{err: errors.New("upstream rejection"), statusCode: status}}
			NewAccountTokenGuardV2Service(guard, nil, reader, prober, svc).runProbe(t.Context(), *guard.record)
			if status == http.StatusUnauthorized {
				require.Equal(t, 1, store.calls)
			} else {
				require.Zero(t, store.calls)
			}
			require.False(t, store.manual, "automatic probes cannot override a failed attempt")
			require.Zero(t, repo.createCalls)
		})
	}
}

func TestContainerReauthPendingProbeReconcilesWithoutManualRetry(t *testing.T) {
	svc, reader, repo, _, _, _ := newReauthTestService("acct-1")
	reader.account.ExecutionMode = AccountExecutionModeDockerKernel
	reader.account.Status = StatusError
	reader.account.ErrorMessage = OpenAIContainerAuthRequired
	store := &containerReauthTestRepo{reauthTestRepo: repo}
	svc.repo = store
	guard := &accountTokenGuardV2TestRepo{record: &AccountTokenGuardV2Record{AccountID: 42, Enabled: true}}
	NewAccountTokenGuardV2Service(guard, nil, reader, accountTokenGuardV2TestProber{}, svc).runProbe(t.Context(), *guard.record)
	require.Equal(t, 1, store.calls)
	require.False(t, store.manual)
	require.Equal(t, AccountTokenGuardV2ProbeAuth, guard.completion.ProbeState)
	require.Zero(t, repo.createCalls)
}

func TestContainerReauthPausePreservesRuntimeAndProjectsRecovery(t *testing.T) {
	svc, account, fixture := vmLifecycleSetup(t)
	svc.prepare(t.Context(), account)
	require.True(t, VMKernelRuntimeReady(account))
	generation := fixture.state.Generation
	before := len(fixture.operations())
	account.Status = StatusError
	account.ErrorMessage = OpenAIContainerAuthRequired
	svc.prepare(t.Context(), account)
	require.False(t, VMKernelRuntimeReady(account))
	require.True(t, fixture.state.Ready, "authentication must not break container health")
	require.Equal(t, generation, fixture.state.Generation)
	require.Equal(t, []string{"inspect:0"}, fixture.operations()[before:])
	account.Status = StatusActive
	account.ErrorMessage = ""
	account.Credentials["access_token"] = "reauthorized"
	svc.prepare(t.Context(), account)
	require.True(t, VMKernelRuntimeReady(account))
	require.Equal(t, generation+1, fixture.state.Generation)
	result, err := svc.Execute(t.Context(), account, "responses", []byte("{}"), nil)
	require.NoError(t, err)
	expected, err := hashReauthCredentials(account.Credentials)
	require.NoError(t, err)
	require.Equal(t, expected, result.DispatchedCredentialsHash)
}

func TestContainerInitialLoginJobValidatesIdentityAndNeverSavesPlaintext(t *testing.T) {
	for _, email := range []string{"user@example.com", "other@example.com"} {
		t.Run(email, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"credential": directReauthCredentials("acct-1", "user-1", email)}))
			}))
			defer server.Close()
			svc := newTwoFATestService(t, server)
			settings := svc.settings.(*twoFALoginSettings)
			job, err := svc.StartContainerTwoFALogin(t.Context(), AccountTokenGuardReloginAccount{Email: "user@example.com", Password: "test-password", MFASecret: "TESTSECRET"})
			require.NoError(t, err)
			result := waitTwoFALogin(t, svc, job.ID)
			if email == "user@example.com" {
				require.Equal(t, "succeeded", result.Status)
				require.Equal(t, "acct-1", result.Credential["chatgpt_account_id"])
			} else {
				require.Equal(t, "failed", result.Status)
				require.Nil(t, result.Credential)
			}
			settings.mu.Lock()
			require.Zero(t, settings.writes)
			settings.mu.Unlock()
			raw, err := json.Marshal(result)
			require.NoError(t, err)
			require.NotContains(t, string(raw), "test-password")
			require.NotContains(t, string(raw), "TESTSECRET")
		})
	}
}

func TestContainerReauthRealKernelHTTP401Contract(t *testing.T) {
	reauth, _, repo, _, _, _ := newReauthTestService("acct-1")
	store := &containerReauthTestRepo{reauthTestRepo: repo}
	reauth.repo = store
	runtime, account := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"upstream_auth","type":"api_error","message":"upstream status 401: private-provider-body"}}`))
	})
	gateway := &OpenAIGatewayService{vmKernel: runtime, excelOAuthReauth: reauth}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	_, err := gateway.forwardVMKernelResponses(t.Context(), c, account, []byte(`{"model":"gpt-test","input":"hello","stream":true}`), "responses", "")
	require.Error(t, err)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Equal(t, 1, store.calls)
	require.NotEmpty(t, store.hash)
	require.False(t, store.manual)
	require.NotContains(t, w.Body.String(), "private-provider-body")
	require.True(t, VMKernelRuntimeReady(account), "provider 401 must not damage container health")
	require.Zero(t, runtime.slot(account.ID).active)
}
