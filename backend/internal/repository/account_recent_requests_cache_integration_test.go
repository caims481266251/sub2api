//go:build integration

package repository

import "testing"

func TestAccountRecentRequestsCacheRedisIntegration(t *testing.T) {
	// Keep testRedis's namespace hook on the actual client used by Eval and ZRange.
	cache := &accountRecentRequestsCache{client: testRedis(t), writes: make(chan struct{}, 4)}
	verifyRecentRequestsCache(t, cache)
}
