package repository

import (
	"context"
	"fmt"
	"slices"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

const dockerSystemBindingCountsSQL = `SELECT COALESCE(docker_system_profile, ''), COUNT(*)
FROM accounts
WHERE deleted_at IS NULL AND execution_mode IN ('docker_kernel', 'vm_kernel')
GROUP BY docker_system_profile`

// A transaction-scoped, deployment-wide lock serializes manual and automatic
// admissions across processes. No scheduler state or upstream work is involved.
const dockerSystemAllocationLockSQL = `SELECT pg_advisory_xact_lock(1937072690, 1)`

func cloneStringPtr(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func (r *accountRepository) AggregateDockerSystemBindings(ctx context.Context) (map[string]int64, error) {
	if groups, scoped := service.ObserverGroupIDs(ctx); scoped {
		if len(groups) == 0 {
			return map[string]int64{}, nil
		}
		return readDockerSystemBindingCounts(ctx, r.sql, `SELECT COALESCE(a.docker_system_profile, ''), COUNT(*)
FROM accounts a
WHERE a.deleted_at IS NULL AND a.execution_mode IN ('docker_kernel', 'vm_kernel')
  AND EXISTS (SELECT 1 FROM account_groups ag WHERE ag.account_id = a.id AND ag.group_id = ANY($1))
GROUP BY a.docker_system_profile`, pq.Array(groups))
	}
	return aggregateDockerSystemBindings(ctx, r.sql)
}

func aggregateDockerSystemBindings(ctx context.Context, query sqlExecutor) (map[string]int64, error) {
	return readDockerSystemBindingCounts(ctx, query, dockerSystemBindingCountsSQL)
}

func readDockerSystemBindingCounts(ctx context.Context, query sqlExecutor, statement string, args ...any) (map[string]int64, error) {
	rows, err := query.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := make(map[string]int64)
	for rows.Next() {
		var id string
		var count int64
		if err := rows.Scan(&id, &count); err != nil {
			return nil, err
		}
		counts[id] = count
	}
	return counts, rows.Err()
}

func selectDockerSystemProfile(selection *service.DockerSystemSelection, counts map[string]int64) (string, error) {
	if selection == nil || len(selection.Candidates) == 0 {
		return "", fmt.Errorf("Docker system selection is required for new Docker accounts")
	}
	seen := make(map[string]bool, len(selection.Candidates))
	for _, id := range selection.Candidates {
		if !service.ValidDockerSystemProfileID(id) || seen[id] {
			return "", fmt.Errorf("invalid Docker system candidates")
		}
		seen[id] = true
	}
	if selection.Requested != service.DockerSystemProfileAuto {
		if !slices.Contains(selection.Candidates, selection.Requested) {
			return "", fmt.Errorf("requested Docker system profile is not available")
		}
		return selection.Requested, nil
	}
	selected := ""
	for _, id := range service.DockerSystemProfileIDs() {
		if seen[id] && (selected == "" || counts[id] < counts[selected]) {
			selected = id
		}
	}
	return selected, nil
}

// Called only inside the account insertion transaction. All create entry points
// share this fence, including the initial admission-plan and group transaction.
func assignDockerSystemProfile(ctx context.Context, client *dbent.Client, account *service.Account) error {
	if !account.IsVMKernel() {
		if account.DockerSystemProfile != nil || account.DockerSystemSelection != nil {
			return fmt.Errorf("Docker system profile requires Docker kernel execution")
		}
		return nil
	}
	if _, err := selectDockerSystemProfile(account.DockerSystemSelection, nil); err != nil {
		return err
	}
	if _, err := client.ExecContext(ctx, dockerSystemAllocationLockSQL); err != nil {
		return err
	}
	counts, err := aggregateDockerSystemBindings(ctx, client)
	if err != nil {
		return err
	}
	id, err := selectDockerSystemProfile(account.DockerSystemSelection, counts)
	if err != nil {
		return err
	}
	account.DockerSystemProfile = &id
	return nil
}
