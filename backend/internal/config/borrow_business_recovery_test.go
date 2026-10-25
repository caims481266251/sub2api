package config

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestBorrowBusinessRecoveryConfig(t *testing.T) {
	resetViperWithJWTSecret(t)
	cfg, err := Load()
	require.NoError(t, err)
	require.False(t, cfg.Gateway.BorrowBusinessRecoveryDisabled)
	require.Equal(t, 15, cfg.Gateway.BorrowBusinessRetryWindowSeconds)
	require.Equal(t, 30, cfg.Gateway.BorrowBusinessRecoveryTimeoutSeconds)
	for _, v := range []int{-1, 61} {
		cfg.Gateway.BorrowBusinessRetryWindowSeconds = v
		require.ErrorContains(t, cfg.Validate(), "borrow_business_retry_window_seconds")
	}
	cfg.Gateway.BorrowBusinessRetryWindowSeconds = 15
	for _, v := range []int{-1, 1, 29, 121} {
		cfg.Gateway.BorrowBusinessRecoveryTimeoutSeconds = v
		require.ErrorContains(t, cfg.Validate(), "borrow_business_recovery_timeout_seconds")
	}
	t.Setenv("GATEWAY_BORROW_BUSINESS_RECOVERY_DISABLED", "true")
	t.Setenv("GATEWAY_BORROW_BUSINESS_RETRY_WINDOW_SECONDS", "20")
	t.Setenv("GATEWAY_BORROW_BUSINESS_RECOVERY_TIMEOUT_SECONDS", "60")
	cfg, err = Load()
	require.NoError(t, err)
	require.True(t, cfg.Gateway.BorrowBusinessRecoveryDisabled)
	require.Equal(t, 20, cfg.Gateway.BorrowBusinessRetryWindowSeconds)
	require.Equal(t, 60, cfg.Gateway.BorrowBusinessRecoveryTimeoutSeconds)
}
