package repository

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestSelectDockerSystemProfileBalancesInFixedOrder(t *testing.T) {
	selection := &service.DockerSystemSelection{Requested: "auto", Candidates: []string{"fedora-44", "debian-12", "ubuntu-24.04", "archlinux", "centos-stream-9"}}
	counts := map[string]int64{"": 1000}
	for i := 0; i < 50; i++ {
		id, err := selectDockerSystemProfile(selection, counts)
		require.NoError(t, err)
		require.Equal(t, service.DockerSystemProfileIDs()[i%5], id)
		counts[id]++
	}
	for _, id := range service.DockerSystemProfileIDs() {
		require.EqualValues(t, 10, counts[id])
	}
	selection.Requested = "fedora-44"
	counts["fedora-44"] = 99
	id, err := selectDockerSystemProfile(selection, counts)
	require.NoError(t, err)
	require.Equal(t, "fedora-44", id, "manual selection bypasses balancing, not validation")
}

func TestSelectDockerSystemProfileRejectsMissingOrUnknownCandidates(t *testing.T) {
	for _, selection := range []*service.DockerSystemSelection{nil, {}, {Requested: "auto", Candidates: []string{"unknown"}}, {Requested: "auto", Candidates: []string{"debian-12", "debian-12"}}, {Requested: "fedora-44", Candidates: []string{"debian-12"}}} {
		_, err := selectDockerSystemProfile(selection, nil)
		require.Error(t, err)
	}
}

func TestDockerSystemBindingAggregateScopesDisplayOnly(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := newAccountRepositoryWithSQL(nil, db, nil)
	mock.ExpectQuery(regexp.QuoteMeta(dockerSystemBindingCountsSQL)).WillReturnRows(sqlmock.NewRows([]string{"profile", "count"}).AddRow("", 3).AddRow("debian-12", 9))
	counts, err := repo.AggregateDockerSystemBindings(t.Context())
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"": 3, "debian-12": 9}, counts)
	scoped := service.WithObserverScope(context.Background(), []int64{4, 7})
	mock.ExpectQuery(`(?s)SELECT COALESCE.*EXISTS.*account_groups.*group_id = ANY`).WithArgs("{4,7}").WillReturnRows(sqlmock.NewRows([]string{"profile", "count"}).AddRow("debian-12", 2))
	counts, err = repo.AggregateDockerSystemBindings(scoped)
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"debian-12": 2}, counts)
	counts, err = repo.AggregateDockerSystemBindings(service.WithObserverScope(t.Context(), nil))
	require.NoError(t, err)
	require.Empty(t, counts)
	// The allocation helper intentionally ignores observer visibility.
	mock.ExpectQuery(regexp.QuoteMeta(dockerSystemBindingCountsSQL)).WillReturnRows(sqlmock.NewRows([]string{"profile", "count"}).AddRow("debian-12", 9))
	counts, err = aggregateDockerSystemBindings(scoped, db)
	require.NoError(t, err)
	require.EqualValues(t, 9, counts["debian-12"])
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountDockerSystemProfileEntityAndCacheMapping(t *testing.T) {
	profile := "debian-12"
	proxyID := int64(7)
	for _, stored := range []*string{nil, &profile} {
		account := accountEntityToService(&dbent.Account{ID: 7, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, ExecutionMode: service.AccountExecutionModeDockerKernel, DockerSystemProfile: stored})
		account.Credentials = map[string]any{"access_token": "fixture-access-token"}
		account.ProxyID = &proxyID
		account.Proxy = &service.Proxy{ID: proxyID, Protocol: "socks5", Host: "fixture.invalid", Port: 1080}
		require.Equal(t, stored, account.DockerSystemProfile)
		account.DockerSystemSelection = &service.DockerSystemSelection{Requested: "auto", Candidates: service.DockerSystemProfileIDs()}
		full, meta, err := marshalSchedulerCacheAccount(*account)
		require.NoError(t, err)
		for _, payload := range [][]byte{full, meta} {
			got, err := decodeCachedAccount(payload)
			require.NoError(t, err)
			require.NotNil(t, got)
			require.Equal(t, stored, got.DockerSystemProfile)
			require.Nil(t, got.DockerSystemSelection)
		}
	}
}
