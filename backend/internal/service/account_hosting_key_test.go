//go:build unit

package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHostingEnableDoesNotRequireAdministratorFallback(t *testing.T) {
	s, _, settings := hostingTestService()
	require.NoError(t, s.UpdateSettings(context.Background(), HostingSettings{Enabled: true, DefaultSeats: 1, RateMultiplier: .03}))
	require.NotContains(t, settings.value, "fallback_group_id")
	cfg, err := s.Settings(context.Background())
	require.NoError(t, err)
	require.True(t, cfg.Enabled)
}

func TestHostingKeyPoolSelectionOwnershipAndDefault(t *testing.T) {
	hosting, hostedRepo, _ := hostingTestService()
	pool, otherPool, zero, no := int64(31), int64(32), int64(0), false
	hostedRepo.pools = map[int64]int64{pool: 7, otherPool: 8}
	repo, cache := newCreateLimitStubs()
	s := newCreateLimitService(repo, cache, 0, 0)
	s.SetHostingService(hosting)
	ctx := context.Background()
	_, err := s.Create(ctx, 7, CreateAPIKeyRequest{Name: "foreign", RouteMode: RouteModeHosted, HostingPoolID: &otherPool, HostingFallbackEnabled: &no})
	require.ErrorIs(t, err, ErrHostingPoolNotFound)
	key, err := s.Create(ctx, 7, CreateAPIKeyRequest{Name: "pool", RouteMode: RouteModeHosted, HostingPoolID: &pool, HostingFallbackEnabled: &no})
	require.NoError(t, err)
	require.Equal(t, pool, *key.HostingPoolID)
	u, _ := newUpdateFieldsAPIKeyService(key)
	u.SetHostingService(hosting)
	_, err = u.Update(ctx, key.ID, 7, UpdateAPIKeyRequest{HostingPoolID: &otherPool})
	require.ErrorIs(t, err, ErrHostingPoolNotFound)
	key, err = u.Update(ctx, key.ID, 7, UpdateAPIKeyRequest{HostingPoolID: &zero})
	require.NoError(t, err)
	require.Nil(t, key.HostingPoolID)
}

func TestHostingPoolAuthCacheRoundTrip(t *testing.T) {
	pool, fallback := int64(31), int64(42)
	s := &APIKeyService{}
	key := &APIKey{ID: 1, UserID: 7, User: &User{ID: 7}, RouteMode: RouteModeHosted, HostingPoolID: &pool, HostingFallbackGroupID: &fallback}
	snapshot := s.snapshotFromAPIKey(context.Background(), key)
	raw, err := json.Marshal(snapshot)
	require.NoError(t, err)
	var restored APIKeyAuthSnapshot
	require.NoError(t, json.Unmarshal(raw, &restored))
	loaded := s.snapshotToAPIKey("secret", &restored)
	require.Equal(t, pool, *loaded.HostingPoolID)
	require.Equal(t, RouteModeHosted, loaded.RouteMode)
	require.Equal(t, fallback, *loaded.HostingFallbackGroupID)
	// Older snapshots can have empty routing fields despite the database being hosted.
	restored.Version = 28
	_, hit, err := s.applyAuthCacheEntry("secret", &APIKeyAuthCacheEntry{Snapshot: &restored})
	require.NoError(t, err)
	require.False(t, hit)
}

func TestHostingKeyCreateRequiresUsersOwnFallbackChoice(t *testing.T) {
	hosting, _, _ := hostingTestService() // Includes a legacy administrator default.
	repo, cache := newCreateLimitStubs()
	s := newCreateLimitService(repo, cache, 0, 0)
	s.SetHostingService(hosting)
	ctx := context.Background()
	_, err := s.Create(ctx, 7, CreateAPIKeyRequest{Name: "hosted", RouteMode: RouteModeHosted})
	require.ErrorIs(t, err, ErrHostingFallbackInvalid)
	require.Empty(t, repo.created)
	no := false
	key, err := s.Create(ctx, 7, CreateAPIKeyRequest{Name: "no fallback", RouteMode: RouteModeHosted, HostingFallbackEnabled: &no})
	require.NoError(t, err)
	require.Nil(t, key.HostingFallbackGroupID)
	group := int64(9)
	key, err = s.Create(ctx, 7, CreateAPIKeyRequest{Name: "chosen", RouteMode: RouteModeHosted, HostingFallbackGroupID: &group})
	require.NoError(t, err)
	require.Equal(t, group, *key.HostingFallbackGroupID)
	group = 12
	_, err = s.Create(ctx, 7, CreateAPIKeyRequest{Name: "invalid", RouteMode: RouteModeHosted, HostingFallbackGroupID: &group})
	require.ErrorIs(t, err, ErrHostingFallbackInvalid)
}

func TestHostingKeyUpdatePreservesChoiceAndRequiresExplicitSelection(t *testing.T) {
	hosting, _, settings := hostingTestService()
	mode, group, yes, no := RouteModeHosted, int64(9), true, false
	ctx := context.Background()
	s, _ := newUpdateFieldsAPIKeyService(&APIKey{ID: 1, UserID: 7, Key: "hosted-key", RouteMode: "group"})
	s.SetHostingService(hosting)
	_, err := s.Update(ctx, 1, 7, UpdateAPIKeyRequest{RouteMode: &mode})
	require.ErrorIs(t, err, ErrHostingFallbackInvalid)
	key, err := s.Update(ctx, 1, 7, UpdateAPIKeyRequest{RouteMode: &mode, HostingFallbackEnabled: &no})
	require.NoError(t, err)
	require.Nil(t, key.HostingFallbackGroupID)
	key, err = s.Update(ctx, 1, 7, UpdateAPIKeyRequest{RouteMode: &mode, HostingFallbackGroupID: &group})
	require.NoError(t, err)
	require.Equal(t, group, *key.HostingFallbackGroupID)
	s, _ = newUpdateFieldsAPIKeyService(key)
	s.SetHostingService(hosting)
	settings.value = `{"enabled":true,"rate_multiplier":0.08,"fallback_group_id":12}`
	key, err = s.Update(ctx, 1, 7, UpdateAPIKeyRequest{RouteMode: &mode})
	require.NoError(t, err)
	require.Equal(t, group, *key.HostingFallbackGroupID)
	key, err = s.Update(ctx, 1, 7, UpdateAPIKeyRequest{HostingFallbackEnabled: &no})
	require.NoError(t, err)
	require.Nil(t, key.HostingFallbackGroupID)
	s, _ = newUpdateFieldsAPIKeyService(key)
	s.SetHostingService(hosting)
	_, err = s.Update(ctx, 1, 7, UpdateAPIKeyRequest{HostingFallbackEnabled: &yes})
	require.ErrorIs(t, err, ErrHostingFallbackInvalid)
}

func TestHostingKeyClearsMultipleGroupsAndRejectsRebinding(t *testing.T) {
	hosting, _, _ := hostingTestService()
	mode, no := RouteModeHosted, false
	group := int64(9)
	s, repo := newUpdateFieldsAPIKeyService(&APIKey{ID: 1, UserID: 7, Key: "hosted-key", RouteMode: "group", GroupID: &group, GroupIDs: []int64{9, 10}})
	s.SetHostingService(hosting)
	key, err := s.Update(t.Context(), 1, 7, UpdateAPIKeyRequest{RouteMode: &mode, HostingFallbackEnabled: &no})
	require.NoError(t, err)
	require.Nil(t, key.GroupID)
	require.Empty(t, key.GroupIDs)
	require.True(t, repo.updateFields[0].GroupID)
	repo.key = key
	groups := []int64{9, 10}
	_, err = s.Update(t.Context(), 1, 7, UpdateAPIKeyRequest{GroupIDs: &groups})
	require.Error(t, err)
	groups = nil
	_, err = s.Update(t.Context(), 1, 7, UpdateAPIKeyRequest{GroupIDs: &groups})
	require.NoError(t, err)
}
