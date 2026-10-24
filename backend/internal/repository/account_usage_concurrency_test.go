//go:build unit

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpenAIUsageConcurrencyRedisAdmissionAndProjection(t *testing.T) {
	ctx := context.Background()
	cache, _ := newSchedulerCacheUnitWithRedis(t)
	a := &service.Account{ID: 71, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Concurrency: 15, Status: service.StatusActive, Schedulable: true, Extra: map[string]any{
		service.OpenAIUsageConcurrencyKey: map[string]any{"enabled": true, "threshold_percent": 5, "concurrency": 6},
		"codex_usage_updated_at":          time.Now().Format(time.RFC3339), "codex_5h_used_percent": 1.0, "codex_7d_used_percent": 4.0}}
	require.NoError(t, cache.SetAccount(ctx, a))
	meta := buildSchedulerMetadataAccount(*a)
	require.Equal(t, 6, meta.SchedulingConcurrency(), "metadata must preserve the policy and usage")
	slots := NewConcurrencyCache(cache.rdb, 5, 30)
	for i := 0; i < 6; i++ {
		ok, err := slots.AcquireAccountSlot(ctx, a.ID, meta.SchedulingConcurrency(), fmt.Sprint(i))
		require.NoError(t, err)
		require.True(t, ok)
	}
	ok, err := slots.AcquireAccountSlot(ctx, a.ID, meta.SchedulingConcurrency(), "seventh")
	require.NoError(t, err)
	require.False(t, ok)
	a.Extra["codex_7d_used_percent"] = 5.0
	require.NoError(t, cache.SetAccount(ctx, a))
	fresh, err := cache.GetAccount(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, 15, fresh.SchedulingConcurrency())
	for i := 6; i < 15; i++ {
		ok, err := slots.AcquireAccountSlot(ctx, a.ID, fresh.SchedulingConcurrency(), fmt.Sprint(i))
		require.NoError(t, err)
		require.True(t, ok)
	}
	ok, err = slots.AcquireAccountSlot(ctx, a.ID, fresh.SchedulingConcurrency(), "sixteenth")
	require.NoError(t, err)
	require.False(t, ok)
	a.Extra["codex_7d_used_percent"] = 0.0
	require.NoError(t, cache.SetAccount(ctx, a))
	fresh, err = cache.GetAccount(ctx, a.ID)
	require.NoError(t, err)
	count, err := slots.GetAccountConcurrency(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, 15, count, "lowering limit must not cancel existing leases")
	ok, err = slots.AcquireAccountSlot(ctx, a.ID, fresh.SchedulingConcurrency(), "after-reset")
	require.NoError(t, err)
	require.False(t, ok)
	for i := 0; i < 10; i++ {
		require.NoError(t, slots.ReleaseAccountSlot(ctx, a.ID, fmt.Sprint(i)))
	}
	ok, err = slots.AcquireAccountSlot(ctx, a.ID, fresh.SchedulingConcurrency(), "after-drain")
	require.NoError(t, err)
	require.True(t, ok)
}
