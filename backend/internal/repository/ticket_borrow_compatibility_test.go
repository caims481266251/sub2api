//go:build unit

package repository

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func expectBorrowCompatibilityRecord(t *testing.T, mock sqlmock.Sqlmock, now time.Time, previousFailures int, reason string, success bool) {
	t.Helper()
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT clock_timestamp\(\)`).WillReturnRows(sqlmock.NewRows([]string{"clock_timestamp"}).AddRow(now))
	row := mock.ExpectQuery(`SELECT consecutive_failures FROM ticket_borrow_compatibility`)
	if previousFailures == 0 {
		row.WillReturnError(sql.ErrNoRows)
	} else {
		row.WillReturnRows(sqlmock.NewRows([]string{"consecutive_failures"}).AddRow(previousFailures))
	}
	expectedFailures := previousFailures + boolToInt(!success)
	if success {
		expectedFailures = 0
	}
	mock.ExpectExec(`INSERT INTO ticket_borrow_compatibility`).WithArgs(
		int64(63), "target-identity", "stable-environment", expectedFailures, reason,
		sqlmock.AnyArg(), success, now,
	).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestBorrowCompatibilityRepositoryBackoffAndReset(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := &ticketBorrowRepository{db: db}
	ctx := context.Background()
	now := time.Date(2099, time.October, 12, 0, 0, 0, 0, time.UTC)

	// The first two failures remain immediately retryable. The third failure
	// starts the 30-second circuit, even though each candidate may carry a new
	// source Cookie pair (the stable key is supplied by the service).
	for failures := 0; failures < 3; failures++ {
		expectBorrowCompatibilityRecord(t, mock, now, failures, "target_ticket_changed", false)
		next, err := repo.RecordBorrowCompatibility(ctx, 63, "target-identity", "stable-environment", "target_ticket_changed", false)
		require.NoError(t, err)
		if failures < 2 {
			require.Equal(t, now, next)
		} else {
			require.Equal(t, now.Add(30*time.Second), next)
		}
	}

	mock.ExpectQuery(`SELECT next_attempt_at,consecutive_failures`).WithArgs(int64(63), "target-identity", "stable-environment").
		WillReturnRows(sqlmock.NewRows([]string{"next_attempt_at", "consecutive_failures"}).AddRow(now.Add(30*time.Second), 3))
	next, blocked, err := repo.CheckBorrowCompatibility(ctx, 63, "target-identity", "stable-environment")
	require.NoError(t, err)
	require.True(t, blocked)
	require.Equal(t, now.Add(30*time.Second), next)

	resetAt := now.Add(time.Minute)
	expectBorrowCompatibilityRecord(t, mock, resetAt, 3, "target_probe_passed", true)
	next, err = repo.RecordBorrowCompatibility(ctx, 63, "target-identity", "stable-environment", "target_probe_passed", true)
	require.NoError(t, err)
	require.Equal(t, resetAt, next)
	require.NoError(t, mock.ExpectationsWereMet())
}
