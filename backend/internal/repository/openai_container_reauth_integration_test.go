//go:build integration

package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func newContainerReauthFixture(t *testing.T) (*openAIOAuthReauthRepository, *service.Account) {
	t.Helper()
	a := mustCreateAccount(t, testEntClient(t), &service.Account{Name: "container-reauth", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Concurrency: 15, Credentials: map[string]any{"access_token": "old-test", "refresh_token": "old-refresh", "email": "user@example.com", "chatgpt_account_id": "workspace", "chatgpt_user_id": "user"}})
	a.ExecutionMode = service.AccountExecutionModeDockerKernel
	_, err := integrationDB.ExecContext(t.Context(), "UPDATE accounts SET execution_mode='docker_kernel' WHERE id=$1", a.ID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(context.Background(), "DELETE FROM scheduler_outbox WHERE account_id=$1", a.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(context.Background(), "DELETE FROM accounts WHERE id=$1", a.ID)
		require.NoError(t, err)
	})
	r := NewOpenAIOAuthReauthRepository(integrationDB).(*openAIOAuthReauthRepository)
	require.NoError(t, r.UpsertConfig(t.Context(), &service.OpenAIOAuthReauthStoredConfig{AccountID: a.ID, LoginEmail: "user@example.com", CredentialMode: service.OpenAIOAuthReauthModePasswordTOTP, ProxySource: service.OpenAIOAuthReauthProxySourceAccount, PasswordCiphertext: "encrypted-test-password", TOTPSecretCiphertext: "encrypted-test-totp"}))
	return r, a
}
func TestContainerReauthConcurrent401AndFailureRetry(t *testing.T) {
	r, a := newContainerReauthFixture(t)
	var wg sync.WaitGroup
	errs := make(chan error, 30)
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.QueueContainerReauth(context.Background(), a, "", false)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	var count, concurrency int
	var status, message string
	var schedulable bool
	require.NoError(t, integrationDB.QueryRowContext(t.Context(), "SELECT count(*) FROM openai_oauth_reauth_tasks WHERE account_id=$1", a.ID).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, integrationDB.QueryRowContext(t.Context(), "SELECT status,error_message,schedulable,concurrency FROM accounts WHERE id=$1", a.ID).Scan(&status, &message, &schedulable, &concurrency))
	require.Equal(t, service.StatusError, status)
	require.Equal(t, service.OpenAIContainerAuthRequired, message)
	require.True(t, schedulable)
	require.Equal(t, 15, concurrency)
	task, err := r.GetLatestTask(t.Context(), a.ID)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(t.Context(), "UPDATE openai_oauth_reauth_tasks SET status='failed',stage='failed' WHERE id=$1", task.ID)
	require.NoError(t, err)
	_, err = r.QueueContainerReauth(t.Context(), a, "", false)
	require.NoError(t, err)
	require.NoError(t, integrationDB.QueryRowContext(t.Context(), "SELECT count(*) FROM openai_oauth_reauth_tasks WHERE account_id=$1", a.ID).Scan(&count))
	require.Equal(t, 1, count, "automatic failure must not loop")
	_, err = r.QueueContainerReauth(t.Context(), a, "", true)
	require.NoError(t, err)
	require.NoError(t, integrationDB.QueryRowContext(t.Context(), "SELECT count(*) FROM openai_oauth_reauth_tasks WHERE account_id=$1", a.ID).Scan(&count))
	require.Equal(t, 2, count, "manual retry must remain available")
}
func TestContainerReauthLate401CannotRevokeNewCredentials(t *testing.T) {
	r, a := newContainerReauthFixture(t)
	_, err := integrationDB.ExecContext(t.Context(), "UPDATE accounts SET credentials=jsonb_set(credentials,'{access_token}',to_jsonb('new-test'::text)) WHERE id=$1", a.ID)
	require.NoError(t, err)
	handled, err := r.QueueContainerReauth(t.Context(), a, "", false)
	require.NoError(t, err)
	require.True(t, handled)
	var count int
	var status string
	require.NoError(t, integrationDB.QueryRowContext(t.Context(), "SELECT count(*) FROM openai_oauth_reauth_tasks WHERE account_id=$1", a.ID).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, integrationDB.QueryRowContext(t.Context(), "SELECT status FROM accounts WHERE id=$1", a.ID).Scan(&status))
	require.Equal(t, service.StatusActive, status)
}
func TestContainerReauthApplyPreservesManualAndBusinessState(t *testing.T) {
	for _, manualDisable := range []bool{false, true} {
		t.Run(map[bool]string{false: "active", true: "manually disabled"}[manualDisable], func(t *testing.T) {
			r, a := newContainerReauthFixture(t)
			_, err := r.QueueContainerReauth(t.Context(), a, "", false)
			require.NoError(t, err)
			task, err := r.GetLatestTask(t.Context(), a.ID)
			require.NoError(t, err)
			_, err = integrationDB.ExecContext(t.Context(), "UPDATE openai_oauth_reauth_tasks SET status='callback_processing',worker_id='test-worker' WHERE id=$1", task.ID)
			require.NoError(t, err)
			status := service.StatusError
			if manualDisable {
				status = "inactive"
			}
			until := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
			_, err = integrationDB.ExecContext(t.Context(), "UPDATE accounts SET status=$2,schedulable=$3,rate_limit_reset_at=$4,overload_until=$4,temp_unschedulable_until=$4,temp_unschedulable_reason='proxy pause' WHERE id=$1", a.ID, status, !manualDisable, until)
			require.NoError(t, err)
			updater := NewAccountRepository(testEntClient(t), integrationDB, nil).(service.OpenAIOAuthReauthCredentialUpdater)
			credentials := map[string]any{}
			for k, v := range a.Credentials {
				credentials[k] = v
			}
			credentials["access_token"] = "updated-test"
			applied, err := updater.ApplyOpenAIOAuthReauth(t.Context(), task.ID, "test-worker", a.ID, a.Credentials, credentials, nil)
			require.NoError(t, err)
			require.True(t, applied)
			var gotStatus, reason, token string
			var schedulable bool
			var quota, overload, temp time.Time
			require.NoError(t, integrationDB.QueryRowContext(t.Context(), "SELECT status,schedulable,rate_limit_reset_at,overload_until,temp_unschedulable_until,temp_unschedulable_reason,credentials->>'access_token' FROM accounts WHERE id=$1", a.ID).Scan(&gotStatus, &schedulable, &quota, &overload, &temp, &reason, &token))
			if manualDisable {
				require.Equal(t, "inactive", gotStatus)
				require.False(t, schedulable)
			} else {
				require.Equal(t, service.StatusActive, gotStatus)
				require.True(t, schedulable)
			}
			require.True(t, quota.Equal(until))
			require.True(t, overload.Equal(until))
			require.True(t, temp.Equal(until))
			require.Equal(t, "proxy pause", reason)
			require.Equal(t, "updated-test", token)
			_, err = r.QueueContainerReauth(t.Context(), a, "", false)
			require.NoError(t, err)
			var count int
			require.NoError(t, integrationDB.QueryRowContext(t.Context(), "SELECT count(*) FROM openai_oauth_reauth_tasks WHERE account_id=$1", a.ID).Scan(&count))
			require.Equal(t, 1, count)
		})
	}
}
func TestContainerTwoFAEnrollmentRequiresEncryptedSave(t *testing.T) {
	r, a := newContainerReauthFixture(t)
	_, err := integrationDB.ExecContext(t.Context(), "UPDATE accounts SET status='error',error_message=$2,schedulable=false WHERE id=$1", a.ID, service.OpenAIContainerTwoFAEnrollmentPending)
	require.NoError(t, err)
	require.Error(t, r.CompleteContainerTwoFAEnrollment(t.Context(), a.ID), "guard enrollment missing must not report success")
	guard := NewAccountTokenGuardV2Repository(integrationDB)
	require.NoError(t, guard.UpsertAccount(t.Context(), a.ID, true, true))
	require.NoError(t, r.CompleteContainerTwoFAEnrollment(t.Context(), a.ID))
	require.NoError(t, r.CompleteContainerTwoFAEnrollment(t.Context(), a.ID), "completion retry is idempotent")
	var status string
	var schedulable bool
	require.NoError(t, integrationDB.QueryRowContext(t.Context(), "SELECT status,schedulable FROM accounts WHERE id=$1", a.ID).Scan(&status, &schedulable))
	require.Equal(t, service.StatusActive, status)
	require.False(t, schedulable)
}
func TestContainerReauthUsesDispatchedSnapshot(t *testing.T) {
	r, a := newContainerReauthFixture(t)
	newer := map[string]any{}
	for k, v := range a.Credentials {
		newer[k] = v
	}
	newer["access_token"] = "dispatch-new"
	raw, err := json.Marshal(newer)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(t.Context(), "UPDATE accounts SET credentials=$2::jsonb WHERE id=$1", a.ID, string(raw))
	require.NoError(t, err)
	a.Credentials = newer
	// Obtain canonical expected hash through the same queue, then issue a duplicate
	// with an older selected snapshot and the actual dispatch hash.
	_, err = r.QueueContainerReauth(t.Context(), a, "", false)
	require.NoError(t, err)
	task, err := r.GetLatestTask(t.Context(), a.ID)
	require.NoError(t, err)
	a.Credentials = map[string]any{"access_token": "selected-older"}
	handled, err := r.QueueContainerReauth(t.Context(), a, task.ExpectedCredentialsHash, false)
	require.NoError(t, err)
	require.True(t, handled)
	var count int
	require.NoError(t, integrationDB.QueryRowContext(t.Context(), "SELECT count(*) FROM openai_oauth_reauth_tasks WHERE account_id=$1", a.ID).Scan(&count))
	require.Equal(t, 1, count)
}

func TestContainerReauthOtherTaskDefersWithoutLosingTrigger(t *testing.T) {
	r, a := newContainerReauthFixture(t)
	_, err := integrationDB.ExecContext(t.Context(), "INSERT INTO openai_oauth_reauth_tasks(account_id,status,stage,expected_credentials_hash,oauth_profile) VALUES($1,'queued','queued','other','excel')", a.ID)
	require.NoError(t, err)
	_, err = r.QueueContainerReauth(t.Context(), a, "", false)
	require.NoError(t, err)
	task, err := r.GetLatestTask(t.Context(), a.ID)
	require.NoError(t, err)
	require.Equal(t, "excel", task.OAuthProfile)
	var status, message string
	require.NoError(t, integrationDB.QueryRowContext(t.Context(), "SELECT status,error_message FROM accounts WHERE id=$1", a.ID).Scan(&status, &message))
	require.Equal(t, service.StatusError, status)
	require.Equal(t, service.OpenAIContainerAuthRequired, message)
	_, err = integrationDB.ExecContext(t.Context(), "UPDATE openai_oauth_reauth_tasks SET status='failed',stage='failed' WHERE id=$1", task.ID)
	require.NoError(t, err)
	_, err = r.QueueContainerReauth(t.Context(), a, "", false)
	require.NoError(t, err)
	task, err = r.GetLatestTask(t.Context(), a.ID)
	require.NoError(t, err)
	require.Equal(t, "codex", task.OAuthProfile)
	require.Equal(t, service.OpenAIOAuthReauthStatusQueued, task.Status)
}

func TestContainerReauthApplyPreservesUnrelatedError(t *testing.T) {
	for _, message := range []any{nil, "business failure"} {
		t.Run(fmt.Sprint(message), func(t *testing.T) {
			r, a := newContainerReauthFixture(t)
			_, err := r.QueueContainerReauth(t.Context(), a, "", false)
			require.NoError(t, err)
			task, err := r.GetLatestTask(t.Context(), a.ID)
			require.NoError(t, err)
			_, err = integrationDB.ExecContext(t.Context(), "UPDATE openai_oauth_reauth_tasks SET status='callback_processing',worker_id='test-worker' WHERE id=$1", task.ID)
			require.NoError(t, err)
			_, err = integrationDB.ExecContext(t.Context(), "UPDATE accounts SET status='error',error_message=$2 WHERE id=$1", a.ID, message)
			require.NoError(t, err)
			newer := map[string]any{}
			for k, v := range a.Credentials {
				newer[k] = v
			}
			newer["access_token"] = "new-test"
			updater := NewAccountRepository(testEntClient(t), integrationDB, nil).(service.OpenAIOAuthReauthCredentialUpdater)
			applied, err := updater.ApplyOpenAIOAuthReauth(t.Context(), task.ID, "test-worker", a.ID, a.Credentials, newer, nil)
			require.NoError(t, err)
			require.True(t, applied)
			var status string
			var gotMessage sql.NullString
			require.NoError(t, integrationDB.QueryRowContext(t.Context(), "SELECT status,error_message FROM accounts WHERE id=$1", a.ID).Scan(&status, &gotMessage))
			require.Equal(t, service.StatusError, status)
			if message == nil {
				require.False(t, gotMessage.Valid)
			} else {
				require.Equal(t, message, gotMessage.String)
			}
		})
	}
}
