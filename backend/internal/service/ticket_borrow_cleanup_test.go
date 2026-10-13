package service

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBorrowCleanupRetriesAfterParentCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		calls := 0
		err := borrowCleanup(ctx, func(cleanup context.Context) error {
			require.NoError(t, cleanup.Err())
			calls++
			if calls == 1 {
				return errors.New("temporary database outage")
			}
			return nil
		})
		require.NoError(t, err)
		require.Equal(t, 2, calls)
	})
}

func TestBorrowCleanupPersistentFailureIsBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		calls := 0
		err := borrowCleanup(t.Context(), func(ctx context.Context) error {
			calls++
			<-ctx.Done()
			return ctx.Err()
		})
		require.Error(t, err)
		require.Equal(t, 3, calls)
		require.LessOrEqual(t, time.Since(start), 10*time.Second)
	})
}
