//go:build integration

package repository

import (
	"fmt"
	"sort"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func sharedProxyFixture(t *testing.T, count int, text string) (*ticketBorrowRepository, *service.ProxyPoolService, []service.BorrowSource) {
	t.Helper()
	_, pool, _ := poolFixture(t)
	repo, _ := borrowIntegrationFixture(t)
	r := repo.(*ticketBorrowRepository)
	selection := borrowSelectionFixture(t, r)
	selection.AccountIDs = nil
	for range count {
		selection.AccountIDs = append(selection.AccountIDs, borrowNormalAccountFixture(t).ID)
	}
	selection.UseProxyPool = true
	require.NoError(t, r.SaveSelection(t.Context(), selection))
	_, err := pool.Import(t.Context(), service.ProxyPoolImport{Revision: 1, Mode: "append", Text: text})
	require.NoError(t, err)
	sources, err := r.Sources(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, count)
	return r, pool, sources
}

func requireBalancedSources(t *testing.T, sources []service.BorrowSource, expected []int) {
	t.Helper()
	counts, fingerprints := map[int64]int{}, map[string]bool{}
	for _, source := range sources {
		require.True(t, source.Enabled)
		require.Empty(t, source.UnavailableReason)
		require.NotZero(t, source.PoolEntryID)
		require.NotEmpty(t, source.FingerprintID)
		require.False(t, fingerprints[source.FingerprintID], "each source keeps its own fingerprint")
		fingerprints[source.FingerprintID] = true
		counts[source.PoolEntryID]++
	}
	actual := make([]int, 0, len(counts))
	for _, count := range counts {
		actual = append(actual, count)
	}
	sort.Ints(actual)
	require.Equal(t, expected, actual)
}

func TestTicketBorrowSharedProxyDistributionAndBatchRotation(t *testing.T) {
	r, _, sources := sharedProxyFixture(t, 7, "a.example:80\nb.example:80\nc.example:80")
	requireBalancedSources(t, sources, []int{2, 2, 3})
	ids := make([]int64, 0, len(sources))
	for _, s := range sources {
		ids = append(ids, s.AccountID)
	}
	for range 3 {
		before := map[int64]string{}
		for _, s := range sources {
			before[s.AccountID] = s.FingerprintID
		}
		job, err := r.QueueRotation(t.Context(), rotationInput("source", ids...), "manual")
		require.NoError(t, err)
		require.NoError(t, r.ProcessRotations(t.Context()))
		require.Equal(t, "completed", rotationPhase(t, job.ID))
		sources, err = r.Sources(t.Context())
		require.NoError(t, err)
		requireBalancedSources(t, sources, []int{2, 2, 3})
		for _, s := range sources {
			require.NotEqual(t, before[s.AccountID], s.FingerprintID)
		}
	}
}

func TestTicketBorrowSingleProxyRotationAndPoolGrowth(t *testing.T) {
	r, pool, sources := sharedProxyFixture(t, 4, "a.example:80")
	requireBalancedSources(t, sources, []int{4})
	// Single-source rotation remains possible with one proxy and replaces only
	// that source's fingerprint after its in-flight request drains.
	old := sources[0]
	job, err := r.QueueRotation(t.Context(), rotationInput("source", old.AccountID), "manual")
	require.NoError(t, err)
	require.NoError(t, r.ProcessRotations(t.Context()))
	require.Equal(t, "completed", rotationPhase(t, job.ID))
	changed, err := r.Source(t.Context(), old.ID)
	require.NoError(t, err)
	require.Equal(t, old.PoolEntryID, changed.PoolEntryID)
	require.NotEqual(t, old.FingerprintID, changed.FingerprintID)
	states, err := r.RotationStates(t.Context())
	require.NoError(t, err)
	require.Equal(t, "balanced_shared", states[0].SelectionKind)
	state, err := pool.State(t.Context())
	require.NoError(t, err)
	_, err = pool.Import(t.Context(), service.ProxyPoolImport{Revision: state.Revision, Mode: "append", Text: "b.example:80\nc.example:80\nd.example:80"})
	require.NoError(t, err)
	ids := []int64{}
	for _, s := range sources {
		ids = append(ids, s.AccountID)
	}
	job, err = r.QueueRotation(t.Context(), rotationInput("source", ids...), "manual")
	require.NoError(t, err)
	require.NoError(t, r.ProcessRotations(t.Context()))
	require.Equal(t, "completed", rotationPhase(t, job.ID))
	sources, err = r.Sources(t.Context())
	require.NoError(t, err)
	requireBalancedSources(t, sources, []int{1, 1, 1, 1})
}

func TestTicketBorrowSharedProxyPendingReservationsCountOnce(t *testing.T) {
	r, _, sources := sharedProxyFixture(t, 7, "a.example:80\nb.example:80\nc.example:80")
	// Two independently queued jobs see one another's future allocation. They
	// must not both claim an apparently underloaded proxy while sources drain.
	for _, s := range sources[:2] {
		ok, err := r.AcquireJob(t.Context(), "source", s.ID, fmt.Sprintf("lease-%d", s.ID))
		require.NoError(t, err)
		require.True(t, ok)
		_, err = r.QueueRotation(t.Context(), rotationInput("source", s.AccountID), "manual")
		require.NoError(t, err)
	}
	tx, err := integrationDB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	entries, loads, err := borrowProxyLoads(t.Context(), tx, []int64{})
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	counts := []int{}
	for _, entry := range entries {
		counts = append(counts, loads[entry])
	}
	sort.Ints(counts)
	require.Equal(t, []int{2, 2, 3}, counts, "future bindings are counted once per account")
	for _, s := range sources[:2] {
		require.NoError(t, r.FinishJob(t.Context(), "source", s.ID, fmt.Sprintf("lease-%d", s.ID), "done", 0))
	}
	require.NoError(t, r.ProcessRotations(t.Context()))
	sources, err = r.Sources(t.Context())
	require.NoError(t, err)
	requireBalancedSources(t, sources, []int{2, 2, 3})
}
