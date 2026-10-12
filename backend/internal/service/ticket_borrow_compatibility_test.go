package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBorrowCookieHeaderHashesAreStableAndSeparated(t *testing.T) {
	balancer, route, pair := borrowCookieHeaderHashes("__oailb=route; __cflb=balancer")
	balancer2, route2, pair2 := borrowCookieHeaderHashes("__cflb=balancer; __oailb=route")
	require.NotEmpty(t, balancer)
	require.NotEmpty(t, route)
	require.NotEmpty(t, pair)
	require.Equal(t, balancer, balancer2)
	require.Equal(t, route, route2)
	require.Equal(t, pair, pair2)
	_, _, differentPair := borrowCookieHeaderHashes("__cflb=other; __oailb=route")
	require.NotEqual(t, pair, differentPair)
}

func TestBorrowCookieResponseHashesIgnoreExpiredAndUnrelatedCookies(t *testing.T) {
	balancer, route, pair := borrowCookieResponseHashes([]*http.Cookie{
		{Name: "other", Value: "ignored"},
		{Name: "__cflb", Value: "balancer"},
		{Name: "__oailb", Value: "expired", MaxAge: -1},
	})
	require.NotEmpty(t, balancer)
	require.Empty(t, route)
	require.NotEmpty(t, pair)
}

func TestBorrowCompatibilityKeyScopesEnvironment(t *testing.T) {
	one := borrowCompatibilityKey("target", 10, "chat.gateway.unified-1.api.openai.com", 2)
	require.NotEmpty(t, one)
	require.NotEqual(t, one, borrowCompatibilityKey("target", 11, "chat.gateway.unified-1.api.openai.com", 2))
	require.NotEqual(t, one, borrowCompatibilityKey("target", 10, "chat.gateway.unified-2.api.openai.com", 2))
	require.NotEqual(t, one, borrowCompatibilityKey("target", 10, "chat.gateway.unified-1.api.openai.com", 3))
	// Fresh source cookies must share the same breaker key so failures can
	// accumulate across candidates from one stable environment.
	require.Equal(t, one, borrowCompatibilityKey("target", 10, "chat.gateway.unified-1.api.openai.com", 2))
}
