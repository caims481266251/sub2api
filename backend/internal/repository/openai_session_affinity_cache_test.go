package repository

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func affinityRedisFixture(t *testing.T) (*gatewayCache, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return &gatewayCache{rdb: client}, mr
}

func TestOpenAISessionAffinityCacheConcurrentClaimAndCompletion(t *testing.T) {
	cache, _ := affinityRedisFixture(t)
	const count = 24
	var wg sync.WaitGroup
	results := make(chan *service.OpenAISessionAffinityBinding, count)
	errors := make(chan error, count)
	for i := range count {
		wg.Go(func() {
			// Separate repositories share the same backing Redis.
			instance := &gatewayCache{rdb: cache.rdb}
			b, err := instance.ClaimOpenAISessionAffinity(t.Context(), "session", service.OpenAISessionAffinityBinding{
				AccountID: int64(i + 1), Version: fmt.Sprint(i), Channel: "ordinary",
			}, time.Minute)
			results <- b
			errors <- err
		})
	}
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	var winner *service.OpenAISessionAffinityBinding
	for b := range results {
		if winner == nil {
			winner = b
		}
		require.Equal(t, winner, b)
	}
	require.True(t, winner.Pending)
	confirmed := *winner
	confirmed.Pending = false
	ok, err := cache.CompleteOpenAISessionAffinity(t.Context(), "session", *winner, confirmed, time.Hour)
	require.NoError(t, err)
	require.True(t, ok)
	loaded, err := cache.GetOpenAISessionAffinity(t.Context(), "session")
	require.NoError(t, err)
	require.Equal(t, confirmed, *loaded)
	// A concurrent request using the winning provisional identity may refresh it.
	ok, err = cache.CompleteOpenAISessionAffinity(t.Context(), "session", *winner, confirmed, time.Hour)
	require.NoError(t, err)
	require.True(t, ok)
	// A late spillover that saw the same provisional generation cannot replace
	// an owner that has since completed successfully.
	other := service.OpenAISessionAffinityBinding{AccountID: 100, Channel: "borrowed", Version: "other"}
	ok, err = cache.CompleteOpenAISessionAffinity(t.Context(), "session", *winner, other, time.Hour)
	require.NoError(t, err)
	require.False(t, ok)
}

func TestOpenAISessionAffinityCacheFencesTakeoverAndExpiry(t *testing.T) {
	cache, mr := affinityRedisFixture(t)
	a := service.OpenAISessionAffinityBinding{AccountID: 11, Version: "a", Channel: "borrowed"}
	pending, err := cache.ClaimOpenAISessionAffinity(t.Context(), "session", a, time.Minute)
	require.NoError(t, err)
	ok, err := cache.CompleteOpenAISessionAffinity(t.Context(), "session", *pending, a, time.Hour)
	require.NoError(t, err)
	require.True(t, ok)
	b := service.OpenAISessionAffinityBinding{AccountID: 12, Version: "b", Channel: "ordinary"}
	ok, err = cache.CompleteOpenAISessionAffinity(t.Context(), "session", a, b, time.Hour)
	require.NoError(t, err)
	require.True(t, ok)
	for _, stale := range []service.OpenAISessionAffinityBinding{a, *pending} {
		ok, err = cache.CompleteOpenAISessionAffinity(t.Context(), "session", stale, a, time.Hour)
		require.NoError(t, err)
		require.False(t, ok)
	}
	mr.FastForward(2 * time.Hour)
	ok, err = cache.CompleteOpenAISessionAffinity(t.Context(), "session", b, b, time.Hour)
	require.NoError(t, err)
	require.False(t, ok, "expired owners cannot be resurrected by old streams")
	missing, err := cache.GetOpenAISessionAffinity(t.Context(), "session")
	require.NoError(t, err)
	require.Nil(t, missing)
	pending, err = cache.ClaimOpenAISessionAffinity(t.Context(), "session", a, time.Minute)
	require.NoError(t, err)
	mr.FastForward(2 * time.Minute)
	ok, err = cache.CompleteOpenAISessionAffinity(t.Context(), "session", *pending, a, time.Hour)
	require.NoError(t, err)
	require.False(t, ok)
}

func TestOpenAISessionAffinityCacheDoesNotTouchResponseOrLegacyBindings(t *testing.T) {
	cache, mr := affinityRedisFixture(t)
	require.NoError(t, cache.SetSessionAccountID(t.Context(), 7, "openai:legacy", 91, time.Hour))
	require.NoError(t, cache.SetSessionAccountID(t.Context(), 7, "openai:response:resp_1", 92, time.Hour))
	pending, err := cache.ClaimOpenAISessionAffinity(t.Context(), "session", service.OpenAISessionAffinityBinding{AccountID: 11, Version: "v", Channel: "borrowed"}, time.Minute)
	require.NoError(t, err)
	mr.FastForward(2 * time.Minute)
	missing, err := cache.GetOpenAISessionAffinity(t.Context(), "session")
	require.NoError(t, err)
	require.Nil(t, missing)
	for key, want := range map[string]int64{"openai:legacy": 91, "openai:response:resp_1": 92} {
		id, err := cache.GetSessionAccountID(t.Context(), 7, key)
		require.NoError(t, err)
		require.Equal(t, want, id)
	}
	_, err = cache.ClaimOpenAISessionAffinity(t.Context(), "invalid", *pending, 0)
	require.Error(t, err)
	mr.Set(openAISessionAffinityKeys("corrupt")[0], "invalid-json")
	_, err = cache.GetOpenAISessionAffinity(t.Context(), "corrupt")
	require.Error(t, err, "corrupt state must not silently turn into a cache miss")
}
