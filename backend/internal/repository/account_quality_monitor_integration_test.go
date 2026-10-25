//go:build integration

package repository

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountQualityMonitorRepositoryOwnershipAndSnapshots(t *testing.T) {
	ctx := context.Background()
	var id int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type,status,schedulable,credentials) VALUES('quality-monitor','openai','apikey','active',true,'{"api_key":"monitor-test"}') RETURNING id`).Scan(&id))
	t.Cleanup(func() { _, _ = integrationDB.ExecContext(ctx, `DELETE FROM accounts WHERE id=$1`, id) })
	account := &service.Account{ID: id, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"api_key": "monitor-test"}}
	repo := NewAccountQualityMonitorRepository(integrationDB)
	missing, err := repo.Get(ctx, id, true)
	require.NoError(t, err)
	require.Nil(t, missing)
	empty, err := repo.Batch(ctx, []int64{id, -1})
	require.NoError(t, err)
	require.Empty(t, empty, "summary reads must not initialize monitor rows")
	empty, err = repo.Batch(ctx, nil)
	require.NoError(t, err)
	require.Empty(t, empty)
	config := service.AccountQualityMonitorConfig{Scope: service.AccountActivityScope(account), Model: "test-model", IntervalMinutes: 5}
	now := time.Now().Truncate(time.Microsecond)
	record, err := repo.Save(ctx, id, config, now)
	require.NoError(t, err)
	require.False(t, record.Enabled)
	var wg sync.WaitGroup
	wins := make(chan string, 2)
	failures := make(chan error, 2)
	for _, owner := range []string{"owner-one", "owner-two"} {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			ok, err := repo.Claim(ctx, record, owner, now, now.Add(time.Minute), false)
			if err != nil {
				failures <- err
			}
			if ok {
				wins <- owner
			}
		}(owner)
	}
	wg.Wait()
	close(wins)
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
	require.Len(t, wins, 1, "only one instance may own an account")
	owner := <-wins
	require.NoError(t, repo.Release(ctx, id, "another-owner"))
	running, err := repo.Get(ctx, id, true)
	require.NoError(t, err)
	require.Equal(t, owner, running.RunID)
	config.IntervalMinutes = 11
	updated, err := repo.Save(ctx, id, config, now)
	require.NoError(t, err)
	require.Equal(t, record.ResultVersion, updated.ResultVersion)
	result := &service.AccountQualityMonitorResult{State: "passed", Answer: "21", Model: record.Model, CheckedAt: now, Text: "full answer", DurationMS: 100}
	ok, err := repo.Finish(ctx, record, owner, result, now)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, repo.Release(ctx, id, owner))
	detail, err := repo.Get(ctx, id, true)
	require.NoError(t, err)
	require.Equal(t, "full answer", detail.LatestAttempt.Text)
	require.Empty(t, detail.LastValid.Text)
	require.Nil(t, detail.NextDueAt)
	summaries, err := repo.Batch(ctx, []int64{id})
	require.NoError(t, err)
	require.Empty(t, summaries[id].LatestAttempt.Text)

	config.Enabled = true
	record, err = repo.Save(ctx, id, config, now)
	require.NoError(t, err)
	ok, err = repo.Claim(ctx, record, "error-owner", now, now.Add(time.Minute), true)
	require.NoError(t, err)
	require.True(t, ok)
	config.IntervalMinutes = 9
	_, err = repo.Save(ctx, id, config, now)
	require.NoError(t, err)
	failed := &service.AccountQualityMonitorResult{State: "error", Model: record.Model, CheckedAt: now}
	ok, err = repo.Finish(ctx, record, "error-owner", failed, now)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, repo.Release(ctx, id, "error-owner"))
	detail, err = repo.Get(ctx, id, true)
	require.NoError(t, err)
	require.Equal(t, "error", detail.LatestAttempt.State)
	require.Equal(t, "passed", detail.LastValid.State)
	require.WithinDuration(t, now.Add(9*time.Minute), *detail.NextDueAt, time.Millisecond)

	record = detail
	ok, err = repo.Claim(ctx, record, "old-model", now, now.Add(time.Minute), false)
	require.NoError(t, err)
	require.True(t, ok)
	config.Model = "new-model"
	updated, err = repo.Save(ctx, id, config, now)
	require.NoError(t, err)
	require.Greater(t, updated.ResultVersion, record.ResultVersion)
	ok, err = repo.Finish(ctx, record, "old-model", result, now)
	require.NoError(t, err)
	require.False(t, ok)
	require.NoError(t, repo.Release(ctx, id, "old-model"))
	record, err = repo.Get(ctx, id, true)
	require.NoError(t, err)
	require.Empty(t, record.RunID)
	require.Nil(t, record.LatestAttempt)

	ok, err = repo.Claim(ctx, record, "old-identity", now, now.Add(time.Minute), false)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = integrationDB.ExecContext(ctx, `UPDATE accounts SET credentials='{"api_key":"replacement"}' WHERE id=$1`, id)
	require.NoError(t, err)
	ok, err = repo.Finish(ctx, record, "old-identity", result, now)
	require.NoError(t, err)
	require.False(t, ok, "actual account identity must be checked even when the monitor row is unchanged")
	require.NoError(t, repo.Release(ctx, id, "old-identity"))

	ok, err = repo.Claim(ctx, record, "expired-owner", now, now.Add(-time.Second), false)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = repo.Claim(ctx, record, "replacement-owner", now, now.Add(time.Minute), false)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, repo.Release(ctx, id, "expired-owner"))
	detail, err = repo.Get(ctx, id, true)
	require.NoError(t, err)
	require.Equal(t, "replacement-owner", detail.RunID)
	ok, err = repo.Renew(ctx, id, "expired-owner", now.Add(2*time.Minute))
	require.NoError(t, err)
	require.False(t, ok)
	ok, err = repo.Renew(ctx, id, "replacement-owner", now.Add(2*time.Minute))
	require.NoError(t, err)
	require.True(t, ok)
}

func TestAccountQualityMonitorEnsurePreservesConcurrentSettings(t *testing.T) {
	ctx := context.Background()
	var id int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type,status,schedulable,credentials) VALUES('quality-settings','openai','apikey','active',true,'{"api_key":"initial"}') RETURNING id`).Scan(&id))
	t.Cleanup(func() { _, _ = integrationDB.ExecContext(ctx, `DELETE FROM accounts WHERE id=$1`, id) })
	repo := NewAccountQualityMonitorRepository(integrationDB)
	now := time.Now()
	account := &service.Account{ID: id, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey}
	var previous service.AccountQualityMonitorConfig
	for _, key := range []string{"identity-one", "identity-two", "identity-three"} {
		_, err := integrationDB.ExecContext(ctx, `UPDATE accounts SET credentials=jsonb_build_object('api_key',$2::text) WHERE id=$1`, id, key)
		require.NoError(t, err)
		account.Credentials = map[string]any{"api_key": key}
		defaults := service.AccountQualityMonitorConfig{Scope: service.AccountActivityScope(account), Model: "gpt-6.1-sol", IntervalMinutes: 5}
		chosen := defaults
		chosen.Enabled, chosen.Model, chosen.IntervalMinutes = true, "custom-text-model", 17
		ready := make(chan struct{})
		failures := make(chan error, 2)
		var wg sync.WaitGroup
		for _, ensure := range []bool{false, true} {
			wg.Add(1)
			go func(ensure bool) {
				defer wg.Done()
				<-ready
				var err error
				if ensure {
					_, err = repo.Ensure(ctx, id, defaults, now)
				} else {
					_, err = repo.Save(ctx, id, chosen, now)
				}
				failures <- err
			}(ensure)
		}
		close(ready)
		wg.Wait()
		close(failures)
		for err := range failures {
			require.NoError(t, err)
		}
		stored, err := repo.Get(ctx, id, true)
		require.NoError(t, err)
		require.Equal(t, chosen, stored.AccountQualityMonitorConfig, "background initialization must not override current UI settings")
		ensured, err := repo.Ensure(ctx, id, defaults, now)
		require.NoError(t, err)
		require.Equal(t, chosen, ensured.AccountQualityMonitorConfig)
		if previous.Scope != "" {
			_, err = repo.Save(ctx, id, previous, now)
			require.ErrorIs(t, err, service.ErrAccountQualityScopeChanged)
			_, err = repo.Ensure(ctx, id, previous, now)
			require.ErrorIs(t, err, service.ErrAccountQualityScopeChanged)
		}
		previous = chosen
	}
	_, err := integrationDB.ExecContext(ctx, `UPDATE accounts SET credentials=credentials || '{"model_mapping":{"custom-text-model":"gpt-image-2"}}'::jsonb WHERE id=$1`, id)
	require.NoError(t, err)
	_, err = repo.Save(ctx, id, previous, now)
	require.ErrorIs(t, err, service.ErrAccountQualityTextModel, "a mapping change during model discovery must be rechecked under the account lock")
}

func TestAccountQualityMonitorShadowParentReplacementInvalidatesSnapshots(t *testing.T) {
	ctx := context.Background()
	var parentID, childID int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type,status,schedulable,credentials) VALUES('quality-parent','openai','oauth','active',true,'{"chatgpt_account_id":"parent-one","access_token":"old"}') RETURNING id`).Scan(&parentID))
	t.Cleanup(func() { _, _ = integrationDB.ExecContext(ctx, `DELETE FROM accounts WHERE id=$1`, parentID) })
	require.NoError(t, integrationDB.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type,status,schedulable,credentials,parent_account_id,quota_dimension) VALUES('quality-shadow','openai','oauth','active',true,'{}',$1,'spark') RETURNING id`, parentID).Scan(&childID))
	t.Cleanup(func() { _, _ = integrationDB.ExecContext(ctx, `DELETE FROM accounts WHERE id=$1`, childID) })
	parent := &service.Account{ID: parentID, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "parent-one"}}
	child := &service.Account{ID: childID, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, ParentAccountID: &parentID, QuotaDimension: "spark"}
	repo := NewAccountQualityMonitorRepository(integrationDB)
	now := time.Now()
	config := service.AccountQualityMonitorConfig{Scope: service.AccountActivityScopeWithCredentialAccount(child, parent), Model: "gpt-6.1-sol", IntervalMinutes: 5}
	record, err := repo.Save(ctx, childID, config, now)
	require.NoError(t, err)
	ok, err := repo.Claim(ctx, record, "parent-one-run", now, now.Add(time.Minute), false)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = integrationDB.ExecContext(ctx, `UPDATE accounts SET credentials=credentials || '{"access_token":"refreshed"}'::jsonb WHERE id=$1`, parentID)
	require.NoError(t, err)
	attempt := &service.AccountQualityMonitorResult{State: "passed", Answer: "21", CheckedAt: now, Model: record.Model}
	ok, err = repo.Finish(ctx, record, "parent-one-run", attempt, now)
	require.NoError(t, err)
	require.True(t, ok, "refreshing a parent's token preserves its identity")
	_, err = integrationDB.ExecContext(ctx, `UPDATE accounts SET credentials=credentials || '{"chatgpt_account_id":"parent-two"}'::jsonb WHERE id=$1`, parentID)
	require.NoError(t, err)
	ok, err = repo.Finish(ctx, record, "parent-one-run", attempt, now)
	require.NoError(t, err)
	require.False(t, ok)
	_, err = repo.Save(ctx, childID, config, now)
	require.ErrorIs(t, err, service.ErrAccountQualityScopeChanged)
	parent.Credentials["chatgpt_account_id"] = "parent-two"
	config.Scope = service.AccountActivityScopeWithCredentialAccount(child, parent)
	replaced, err := repo.Ensure(ctx, childID, config, now)
	require.NoError(t, err)
	require.Nil(t, replaced.LatestAttempt)
	require.Nil(t, replaced.LastValid)
	require.NoError(t, repo.Release(ctx, childID, "parent-one-run"), "old owner can release its own lease after identity changes")
}

func TestAccountQualityMonitorExecutionModeInvalidatesOldOwnership(t *testing.T) {
	ctx := context.Background()
	var id int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type,status,schedulable,credentials)
 VALUES('quality-execution-mode','openai','oauth','active',true,'{"chatgpt_account_id":"same-upstream"}') RETURNING id`).Scan(&id))
	t.Cleanup(func() { _, _ = integrationDB.ExecContext(ctx, `DELETE FROM accounts WHERE id=$1`, id) })
	account := &service.Account{ID: id, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "same-upstream"}}
	repo := NewAccountQualityMonitorRepository(integrationDB)
	now := time.Now()
	config := service.AccountQualityMonitorConfig{Scope: service.AccountActivityScope(account), Model: "gpt-6.1-sol", IntervalMinutes: 5}
	record, err := repo.Save(ctx, id, config, now)
	require.NoError(t, err)
	claimed, err := repo.Claim(ctx, record, "normal-run", now, now.Add(time.Minute), false)
	require.NoError(t, err)
	require.True(t, claimed)
	attempt := &service.AccountQualityMonitorResult{State: "passed", Answer: "21", Model: record.Model, CheckedAt: now}
	finished, err := repo.Finish(ctx, record, "normal-run", attempt, now)
	require.NoError(t, err)
	require.True(t, finished)
	// The normal API keeps the mode immutable. This models a deployment migration
	// or stale snapshot while a normal-mode probe still owns a database lease.
	_, err = integrationDB.ExecContext(ctx, `UPDATE accounts SET execution_mode='docker_kernel' WHERE id=$1`, id)
	require.NoError(t, err)
	finished, err = repo.Finish(ctx, record, "normal-run", attempt, now)
	require.NoError(t, err)
	require.False(t, finished)
	_, err = repo.Save(ctx, id, config, now)
	require.ErrorIs(t, err, service.ErrAccountQualityScopeChanged)
	account.ExecutionMode = service.AccountExecutionModeDockerKernel
	config.Scope = service.AccountActivityScope(account)
	replaced, err := repo.Ensure(ctx, id, config, now)
	require.NoError(t, err, "repository scope checks must load execution_mode")
	require.Nil(t, replaced.LatestAttempt)
	require.Nil(t, replaced.LastValid)
	require.NoError(t, repo.Release(ctx, id, "normal-run"))
}
