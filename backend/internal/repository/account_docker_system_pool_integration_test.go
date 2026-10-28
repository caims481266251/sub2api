//go:build integration

package repository

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// Real PostgreSQL is required: a Go mutex or a mocked count query cannot prove
// that the advisory lock spans allocation, insertion, and transaction commit.
func TestDockerSystemPoolPostgres50ConcurrentCreates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	client := testEntClient(t)
	repo := newAccountRepositoryWithSQL(client, integrationDB, nil)
	prefix := fmt.Sprintf("docker-system-concurrency-%d-", time.Now().UnixNano())
	group, err := client.Group.Create().SetName(prefix + "group").SetPlatform(service.PlatformOpenAI).Save(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanup := context.Background()
		_, _ = integrationDB.ExecContext(cleanup, `DELETE FROM scheduler_outbox WHERE account_id IN (SELECT id FROM accounts WHERE name LIKE $1)`, prefix+"%")
		_, _ = integrationDB.ExecContext(cleanup, `DELETE FROM account_groups WHERE account_id IN (SELECT id FROM accounts WHERE name LIKE $1)`, prefix+"%")
		_, _ = integrationDB.ExecContext(cleanup, `DELETE FROM accounts WHERE name LIKE $1`, prefix+"%")
		_, _ = integrationDB.ExecContext(cleanup, `DELETE FROM groups WHERE id=$1`, group.ID)
	})

	newAccount := func(name, requested string) *service.Account {
		return &service.Account{Name: prefix + name, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, ExecutionMode: service.AccountExecutionModeDockerKernel,
			Status: service.StatusActive, Concurrency: 2, Priority: 50, Credentials: map[string]any{}, Extra: map[string]any{},
			DockerSystemSelection: &service.DockerSystemSelection{Requested: requested, Candidates: service.DockerSystemProfileIDs()}}
	}
	baseline, err := repo.AggregateDockerSystemBindings(ctx)
	require.NoError(t, err)
	expected := make(map[string]int64, len(baseline))
	for id, count := range baseline {
		expected[id] = count
	}
	for i := 0; i < 50; i++ {
		id, err := selectDockerSystemProfile(newAccount("simulation", "auto").DockerSystemSelection, expected)
		require.NoError(t, err)
		expected[id]++
	}

	type result struct {
		account *service.Account
		err     error
	}
	results := make(chan result, 50)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for i := 0; i < 50; i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			<-start
			account := newAccount(fmt.Sprintf("account-%02d", i), "auto")
			if i%3 == 0 {
				account.Status = service.StatusError
				account.ErrorMessage = "initialization pending fixture"
			}
			if i%3 == 1 {
				account.Status = "inactive"
			}
			var err error
			if i%2 == 0 {
				err = repo.Create(ctx, account)
			} else {
				err = repo.CreateWithAccountGroups(service.WithObserverScope(ctx, []int64{group.ID}), account, []service.AccountGroup{{GroupID: group.ID, Priority: 50}})
			}
			results <- result{account: account, err: err}
		}(i)
	}
	close(start)
	workers.Wait()
	close(results)
	var first *service.Account
	seen := make(map[int64]bool)
	for result := range results {
		require.NoError(t, result.err)
		require.NotNil(t, result.account.DockerSystemProfile)
		require.False(t, seen[result.account.ID])
		seen[result.account.ID] = true
		stored, err := repo.GetByID(ctx, result.account.ID)
		require.NoError(t, err)
		require.Equal(t, result.account.DockerSystemProfile, stored.DockerSystemProfile)
		if first == nil {
			first = stored
		}
	}
	require.Len(t, seen, 50)
	counts, err := repo.AggregateDockerSystemBindings(ctx)
	require.NoError(t, err)
	for _, id := range service.DockerSystemProfileIDs() {
		require.Equal(t, expected[id], counts[id], id)
	}

	// Manual choices count alongside automatic allocations, including stopped or
	// failed accounts. A failed insertion must leave the distribution unchanged.
	manual := newAccount("manual", "fedora-44")
	require.NoError(t, repo.CreateWithAccountGroups(ctx, manual, []service.AccountGroup{{GroupID: group.ID, Priority: 20}}))
	require.Equal(t, "fedora-44", *manual.DockerSystemProfile)
	beforeFailure, err := repo.AggregateDockerSystemBindings(ctx)
	require.NoError(t, err)
	// A caller-owned transaction must hold the lock and binding until the same
	// outer commit; rolling it back must also erase an already inserted account.
	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	transactionRepo := newAccountRepositoryWithSQL(tx.Client(), tx.Client(), nil)
	rolledBack := newAccount("outer-rollback", "auto")
	require.NoError(t, transactionRepo.Create(ctx, rolledBack))
	require.Positive(t, rolledBack.ID)
	require.NoError(t, tx.Rollback())
	var rolledBackCount int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE id=$1`, rolledBack.ID).Scan(&rolledBackCount))
	require.Zero(t, rolledBackCount)
	invalid := newAccount("failed", "auto")
	invalid.Name += strings.Repeat("x", 101)
	require.Error(t, repo.Create(ctx, invalid))
	afterFailure, err := repo.AggregateDockerSystemBindings(ctx)
	require.NoError(t, err)
	require.Equal(t, beforeFailure, afterFailure)

	// Old NULL bindings are read as legacy, never reassigned by edits or SQL.
	legacy, err := client.Account.Create().SetName(prefix + "legacy").SetPlatform(service.PlatformOpenAI).SetType(service.AccountTypeOAuth).SetExecutionMode("vm_kernel").Save(ctx)
	require.NoError(t, err)
	legacyRead, err := repo.GetByID(ctx, legacy.ID)
	require.NoError(t, err)
	require.Nil(t, legacyRead.DockerSystemProfile)
	_, err = integrationDB.ExecContext(ctx, `UPDATE accounts SET docker_system_profile='debian-12' WHERE id=$1`, legacy.ID)
	require.Error(t, err, "legacy NULL must not be silently migrated")
	_, err = integrationDB.ExecContext(ctx, `UPDATE accounts SET docker_system_profile=NULL WHERE id=$1`, first.ID)
	require.Error(t, err, "direct SQL cannot erase immutable bindings")
	_, err = integrationDB.ExecContext(ctx, `UPDATE accounts SET extra=jsonb_build_object('docker_system_profile','fedora-44'), name=name || '-edited' WHERE id=$1`, first.ID)
	require.NoError(t, err)
	stored, err := repo.GetByID(ctx, first.ID)
	require.NoError(t, err)
	require.Equal(t, first.DockerSystemProfile, stored.DockerSystemProfile)

	// Soft deletion removes the binding from counts without mutating it.
	_, err = integrationDB.ExecContext(ctx, `UPDATE accounts SET deleted_at=NOW() WHERE id=$1`, manual.ID)
	require.NoError(t, err)
	counts, err = repo.AggregateDockerSystemBindings(ctx)
	require.NoError(t, err)
	require.Equal(t, expected["fedora-44"], counts["fedora-44"])
	require.Equal(t, baseline[""]+1, counts[""])
}
