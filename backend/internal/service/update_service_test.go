//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type updateServiceCacheStub struct {
	data string
}

func (s *updateServiceCacheStub) GetUpdateInfo(context.Context) (string, error) {
	if s.data == "" {
		return "", errors.New("cache miss")
	}
	return s.data, nil
}

func (s *updateServiceCacheStub) SetUpdateInfo(_ context.Context, data string, _ time.Duration) error {
	s.data = data
	return nil
}

type updateServiceGitHubClientStub struct {
	release        *GitHubRelease
	recentReleases []*GitHubRelease
	recentErr      error
	latestErr      error
	latestRepo     string
}

func (s *updateServiceGitHubClientStub) FetchLatestRelease(_ context.Context, repo string) (*GitHubRelease, error) {
	s.latestRepo = repo
	return s.release, s.latestErr
}

func TestUpdateServiceCheckUpdateReportsLiveRestartState(t *testing.T) {
	for _, scenario := range []string{"fresh-release", "cached-release", "offline-cached", "offline-no-cache"} {
		t.Run(scenario, func(t *testing.T) {
			cache := &updateServiceCacheStub{}
			client := &updateServiceGitHubClientStub{release: &GitHubRelease{TagName: "v2.15.16"}}
			svc := NewUpdateService(cache, client, "2.15.15", "release")
			if scenario == "cached-release" || scenario == "offline-cached" {
				_, err := svc.CheckUpdate(context.Background(), true)
				require.NoError(t, err)
			}
			if scenario == "offline-cached" || scenario == "offline-no-cache" {
				client.latestErr = errors.New("GitHub unavailable")
			}
			svc.restartRequired.Store(true)
			info, err := svc.CheckUpdate(context.Background(), scenario != "cached-release")
			require.NoError(t, err)
			require.True(t, info.RestartRequired)
			require.Equal(t, "2.15.15", info.CurrentVersion, "installed binary does not change the running version")
			encoded, err := json.Marshal(info)
			require.NoError(t, err)
			require.Contains(t, string(encoded), `"restart_required":true`)
			require.NotContains(t, cache.data, "restart_required", "do not persist process state in the shared release cache")
			if scenario == "offline-cached" || scenario == "offline-no-cache" {
				require.Contains(t, info.Warning, "GitHub unavailable")
			}

			// A restarted process must not inherit a previous process's pending flag.
			restarted := NewUpdateService(cache, client, "2.15.16", "release")
			after, err := restarted.CheckUpdate(context.Background(), false)
			require.NoError(t, err)
			require.False(t, after.RestartRequired)
			require.Equal(t, "2.15.16", after.CurrentVersion)
		})
	}
}

func TestUpdateServiceCheckUpdateDoesNotWaitForInstallation(t *testing.T) {
	svc := NewUpdateService(&updateServiceCacheStub{}, &updateServiceGitHubClientStub{
		release: &GitHubRelease{TagName: "v2.15.16"},
	}, "2.15.15", "release")
	// Installation holds this lock during downloads and runtime preparation.
	svc.installMu.Lock()
	defer svc.installMu.Unlock()
	done := make(chan *UpdateInfo, 1)
	go func() {
		info, _ := svc.CheckUpdate(context.Background(), true)
		done <- info
	}()
	select {
	case info := <-done:
		require.NotNil(t, info)
		require.False(t, info.RestartRequired)
	case <-time.After(time.Second):
		t.Fatal("checking update status must not wait for an in-progress installation")
	}
}

func (s *updateServiceGitHubClientStub) FetchRecentReleases(context.Context, string, int) ([]*GitHubRelease, error) {
	return s.recentReleases, s.recentErr
}

func (s *updateServiceGitHubClientStub) DownloadFile(context.Context, string, string, int64) error {
	panic("DownloadFile should not be called when no update is available")
}

func (s *updateServiceGitHubClientStub) FetchChecksumFile(context.Context, string) ([]byte, error) {
	panic("FetchChecksumFile should not be called when no update is available")
}

func TestUpdateServicePerformUpdateNoUpdateReturnsSentinel(t *testing.T) {
	githubClient := &updateServiceGitHubClientStub{
		release: &GitHubRelease{
			TagName: "v0.1.132",
			Name:    "v0.1.132",
		},
	}
	svc := NewUpdateService(
		&updateServiceCacheStub{},
		githubClient,
		"0.1.132",
		"release",
	)

	err := svc.PerformUpdate(context.Background())

	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNoUpdateAvailable))
	require.ErrorIs(t, err, ErrNoUpdateAvailable)
	require.Equal(t, "caims481266251/sub2api", githubClient.latestRepo)
}

func newRollbackTestService(current string, releases []*GitHubRelease) *UpdateService {
	return NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{recentReleases: releases},
		current,
		"release",
	)
}

func TestUpdateServiceListRollbackVersionsFiltersAndCaps(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.1.148", PublishedAt: "2026-07-09T00:00:00Z"},                       // newer than current: excluded
		{TagName: "v0.1.147", PublishedAt: "2026-07-08T00:00:00Z"},                       // current: excluded
		{TagName: "v0.1.146-rc1", PublishedAt: "2026-07-07T12:00:00Z", Prerelease: true}, // prerelease: excluded
		{TagName: "v0.1.146", PublishedAt: "2026-07-07T00:00:00Z"},
		{TagName: "v0.1.145", PublishedAt: "2026-07-06T00:00:00Z", Draft: true}, // draft: excluded
		{TagName: "v0.1.144", PublishedAt: "2026-07-05T00:00:00Z"},
		{TagName: "v0.1.144", PublishedAt: "2026-07-05T00:00:00Z"}, // duplicate: excluded
		{TagName: "v0.1.143", PublishedAt: "2026-07-04T00:00:00Z"},
		{TagName: "v0.1.142", PublishedAt: "2026-07-03T00:00:00Z"}, // beyond cap of 3: excluded
	}
	svc := newRollbackTestService("0.1.147", releases)

	versions, err := svc.ListRollbackVersions(context.Background())

	require.NoError(t, err)
	require.Len(t, versions, 3)
	require.Equal(t, "0.1.146", versions[0].Version)
	require.Equal(t, "0.1.144", versions[1].Version)
	require.Equal(t, "0.1.143", versions[2].Version)
}

func TestUpdateServiceListRollbackVersionsSortsUnorderedInput(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.1.144"},
		{TagName: "v0.1.146"},
		{TagName: "v0.1.145"},
	}
	svc := newRollbackTestService("0.1.147", releases)

	versions, err := svc.ListRollbackVersions(context.Background())

	require.NoError(t, err)
	require.Len(t, versions, 3)
	require.Equal(t, "0.1.146", versions[0].Version)
	require.Equal(t, "0.1.145", versions[1].Version)
	require.Equal(t, "0.1.144", versions[2].Version)
}

func TestUpdateServiceListRollbackVersionsEmptyWhenNoneOlder(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.1.147"},
		{TagName: "v0.1.148"},
	}
	svc := newRollbackTestService("0.1.147", releases)

	versions, err := svc.ListRollbackVersions(context.Background())

	require.NoError(t, err)
	require.Empty(t, versions)
}

func TestUpdateServiceListRollbackVersionsPropagatesFetchError(t *testing.T) {
	svc := NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{recentErr: errors.New("github unavailable")},
		"0.1.147",
		"release",
	)

	_, err := svc.ListRollbackVersions(context.Background())

	require.Error(t, err)
	require.Contains(t, err.Error(), "github unavailable")
}

func TestUpdateServiceRollbackToVersionRejectsDisallowedTargets(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.1.148"},
		{TagName: "v0.1.147"},
		{TagName: "v0.1.146"},
		{TagName: "v0.1.145"},
		{TagName: "v0.1.144"},
		{TagName: "v0.1.143"},
		{TagName: "v0.1.142"},
	}
	svc := newRollbackTestService("0.1.147", releases)

	for _, target := range []string{
		"",         // empty
		"0.1.147",  // current version
		"v0.1.147", // current version with prefix
		"0.1.148",  // newer than current
		"0.1.142",  // older than the 3 most recent
		"9.9.9",    // nonexistent
	} {
		err := svc.RollbackToVersion(context.Background(), target)
		require.ErrorIs(t, err, ErrRollbackVersionNotAllowed, "target %q should be rejected", target)
	}
}

func TestUpdateServiceRollbackToVersionAcceptsVPrefix(t *testing.T) {
	// No platform asset in the release: the target passes the allowlist check
	// and fails later at asset lookup, proving the version itself was accepted.
	releases := []*GitHubRelease{
		{TagName: "v0.1.147"},
		{TagName: "v0.1.146"},
	}
	svc := newRollbackTestService("0.1.147", releases)

	err := svc.RollbackToVersion(context.Background(), "v0.1.146")

	require.Error(t, err)
	require.NotErrorIs(t, err, ErrRollbackVersionNotAllowed)
	require.Contains(t, err.Error(), "no compatible release found")
}

func TestUpdateServicePublishedVMUpgrade(t *testing.T) {
	for _, current := range []string{"2.10.17", "2.15.0", "2.15.0-vm.1"} {
		client := &updateServiceGitHubClientStub{release: &GitHubRelease{TagName: "v2.15.1"}}
		svc := NewUpdateService(&updateServiceCacheStub{}, client, current, "release")
		info, err := svc.CheckUpdate(context.Background(), true)
		require.NoError(t, err)
		require.True(t, info.HasUpdate, current)
		require.Equal(t, "caims481266251/sub2api", client.latestRepo)
	}
}

func TestUpdateServicePrereleaseOrdering(t *testing.T) {
	require.Less(t, compareVersions("2.15.0-vm.1", "2.15.0"), 0)
	require.Less(t, compareVersions("2.15.0-vm.2", "2.15.0-vm.10"), 0)
	require.Equal(t, 0, compareVersions("v2.15.1", "2.15.1+build"))
	require.Greater(t, compareVersions("2.15.1", "2.15.0-vm.1"), 0)
}

func TestUpdateServiceRejectsForeignCache(t *testing.T) {
	for _, repo := range []string{"", "ranxi2001/sub2api"} {
		data, err := json.Marshal(map[string]any{"repo": repo, "latest": "99.0.0", "timestamp": time.Now().Unix()})
		require.NoError(t, err)
		cache := &updateServiceCacheStub{data: string(data)}
		client := &updateServiceGitHubClientStub{release: &GitHubRelease{TagName: "v2.15.1"}}
		svc := NewUpdateService(cache, client, "2.15.0", "release")
		info, err := svc.CheckUpdate(context.Background(), false)
		require.NoError(t, err)
		require.Equal(t, "2.15.1", info.LatestVersion)
		require.False(t, info.Cached)
		info, err = svc.CheckUpdate(context.Background(), false)
		require.NoError(t, err)
		require.True(t, info.Cached)
	}
}
