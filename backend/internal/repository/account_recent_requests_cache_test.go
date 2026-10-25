package repository

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func verifyRecentRequestsCache(t *testing.T, cache *accountRecentRequestsCache) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	// Deliberately finish/write out of order: retain completion order, not append order.
	for i := 19; i >= 0; i-- {
		require.NoError(t, cache.Append(ctx, "identity-a", service.AccountRecentRequest{AttemptID: fmt.Sprint(i), AccountID: 42, CompletedAt: now.Add(time.Duration(i) * time.Second), Outcome: "success"}))
	}
	got, err := cache.Batch(ctx, map[int64]string{42: "identity-a", 43: "identity-a"})
	require.NoError(t, err)
	require.Len(t, got[42], 16)
	require.Equal(t, "4", got[42][0].AttemptID)
	require.Equal(t, "19", got[42][15].AttemptID)
	require.Empty(t, got[43])
	// Different attempts at the same completion timestamp must not overwrite.
	for i := 0; i < 3; i++ {
		require.NoError(t, cache.Append(ctx, "identity-b", service.AccountRecentRequest{AttemptID: fmt.Sprintf("tie-%d", i), AccountID: 42, CompletedAt: now, Outcome: "failure"}))
	}
	other, err := cache.Batch(ctx, map[int64]string{42: "identity-b"})
	require.NoError(t, err)
	require.Len(t, other[42], 3)
	firstAgain, err := cache.Batch(ctx, map[int64]string{42: "identity-a"})
	require.NoError(t, err)
	require.Len(t, firstAgain[42], 16)
	ttl, err := cache.client.TTL(ctx, accountRecentRequestsKey(42, "identity-a")).Result()
	require.NoError(t, err)
	require.Greater(t, ttl, 7*24*time.Hour-time.Minute)
	require.LessOrEqual(t, ttl, 7*24*time.Hour)
}

func TestAccountRecentRequestsCacheRetentionAndIsolation(t *testing.T) {
	server := miniredis.RunT(t)
	shared := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = shared.Close() })
	cache := NewAccountRecentRequestsCache(shared).(*accountRecentRequestsCache)
	t.Cleanup(func() { _ = cache.Close() })
	verifyRecentRequestsCache(t, cache)
	server.FastForward(8 * 24 * time.Hour)
	got, err := cache.Batch(context.Background(), map[int64]string{42: "identity-a"})
	require.NoError(t, err)
	require.Empty(t, got[42])
}

func TestAccountRecentRequestsCacheSaturationDropsObservation(t *testing.T) {
	cache := &accountRecentRequestsCache{writes: make(chan struct{}, 1)}
	cache.writes <- struct{}{}
	// A full write budget returns without touching even a nil Redis client.
	require.NoError(t, cache.Append(context.Background(), "scope", service.AccountRecentRequest{AccountID: 42}))
}

func TestAccountRecentRequestsCacheConcurrentCompletion(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	cache := &accountRecentRequestsCache{client: client, writes: make(chan struct{}, 4)}
	ctx := context.Background()
	start := time.Now()
	var wg sync.WaitGroup
	errors := make(chan error, 40)
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := worker; i < 40; i += 4 {
				errors <- cache.Append(ctx, "scope", service.AccountRecentRequest{AccountID: 42, AttemptID: fmt.Sprint(i), CompletedAt: start.Add(time.Duration(i) * time.Second)})
			}
		}(worker)
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	got, err := cache.Batch(ctx, map[int64]string{42: "scope"})
	require.NoError(t, err)
	require.Len(t, got[42], 16)
	require.Equal(t, "24", got[42][0].AttemptID)
	require.Equal(t, "39", got[42][15].AttemptID)
}
